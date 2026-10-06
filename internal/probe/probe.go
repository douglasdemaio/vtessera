// Package probe runs a capability probe against an agent's declared endpoint.
//
// The marketplace makes the request, so this package is the one place where an
// agent can cause the service to send traffic somewhere. Everything about the
// egress therefore lives here rather than at the call site, because a check that
// is easy to forget is a check that will be forgotten.
package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/douglasdemaio/vtessera/internal/attest"
)

// ErrRefused means the probe was not attempted, or was stopped before it
// reached the agent. It is distinct from a probe that ran and reported a
// capability as failing: nothing was learned about the agent.
var ErrRefused = errors.New("probe refused")

// ErrTarget means the declared target is not something this service will call.
// It is a configuration or policy failure rather than the agent's fault, and it
// is reported rather than attempted so a mistyped target cannot be probed by
// retrying.
var ErrTarget = errors.New("probe target is not permitted")

// Status is what a probe concluded about one capability.
type Status string

const (
	// StatusPass means the agent answered and reported the capability working.
	StatusPass Status = "pass"
	// StatusFail means the agent answered and reported the capability not
	// working.
	StatusFail Status = "fail"
	// StatusUnknown means the agent did not claim the capability either way.
	StatusUnknown Status = "unknown"
)

// Result is one capability's outcome.
type Result struct {
	Capability string `json:"capability"`
	Status     Status `json:"status"`
	Detail     string `json:"detail,omitempty"`
}

// Report is a probe's answer about one agent.
type Report struct {
	AgentID   string    `json:"agentId"`
	Target    string    `json:"target"`
	Results   []Result  `json:"results"`
	CheckedAt time.Time `json:"checkedAt"`
	// Signature is the marketplace's attestation over this report. It is set when
	// the report is read back from storage and is nil on a report that has just
	// been produced and not yet signed.
	Signature *attest.Signature `json:"signature,omitempty"`
}

// Passed reports whether every claimed capability passed.
func (r Report) Passed() bool {
	if len(r.Results) == 0 {
		return false
	}
	for _, res := range r.Results {
		if res.Status != StatusPass {
			return false
		}
	}
	return true
}

// Limits bound one probe. They are fields rather than constants because a
// caller tightening them must not require a different binary, and because a test
// asserting the timeout is asserting these values.
type Limits struct {
	// Timeout bounds the whole exchange, dial included.
	Timeout time.Duration
	// MaxResponseBytes caps what is read from the agent. An agent that streams
	// without end must not be able to hold a goroutine and a connection open
	// until the timeout, repeatedly.
	MaxResponseBytes int64
}

// DefaultLimits are the values used when a caller does not set them.
func DefaultLimits() Limits {
	return Limits{Timeout: 5 * time.Second, MaxResponseBytes: 64 << 10}
}

// request is what a probe sends. The challenge is echoed by the agent, which is
// what makes the answer evidence of a live agent rather than a cached page or a
// proxy that answers for it.
type request struct {
	Challenge    string   `json:"challenge"`
	AgentID      string   `json:"agentId"`
	Capabilities []string `json:"capabilities"`
	Version      string   `json:"version"`
}

// response is what a probe accepts. Unknown fields are refused rather than
// ignored, so a probe cannot report a pass on a schema it did not read.
type response struct {
	Challenge string   `json:"challenge"`
	AgentID   string   `json:"agentId"`
	Results   []Result `json:"results"`
}

// routable decides whether an address may be dialled.
type routable func(net.IP) error

// Runner executes probes against declared targets.
type Runner struct {
	limits  Limits
	client  *http.Client
	version string
}

// New builds a runner whose egress is constrained.
//
// The returned client does not follow redirects, dials only public addresses
// checked at the moment of connection, and bounds the response. Those three are
// the whole of the outbound risk; everything else is policy.
//
// There is deliberately no way to relax the address check from configuration.
// A setting that could turn it off is a setting a deploy could get wrong in the
// direction that turns this service into a request proxy, and the test suite
// reaches the unexported constructor instead of a flag.
func New(version string, limits Limits) *Runner {
	return newRunner(version, limits, checkRoutable, nil)
}

// newRunner takes the address policy and the trust roots as arguments so a test
// can exercise the exchange itself against a loopback TLS server. Only this
// package can call it, and production always passes the real policy and no extra
// roots.
func newRunner(version string, limits Limits, check routable, tlsConf *tls.Config) *Runner {
	if limits.Timeout <= 0 {
		limits.Timeout = DefaultLimits().Timeout
	}
	if limits.MaxResponseBytes <= 0 {
		limits.MaxResponseBytes = DefaultLimits().MaxResponseBytes
	}
	dialer := &net.Dialer{Timeout: limits.Timeout}
	r := &Runner{
		limits:  limits,
		version: version,
		client: &http.Client{
			Timeout: limits.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// A redirect is a way to send the probe somewhere the agent chose
				// on the way to the place it declared, and it is the cheapest way
				// to reach an address the target check would have refused.
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				TLSClientConfig: tlsConf,
				// The address is resolved and checked here, immediately before the
				// connection, rather than earlier. Checking earlier is a check on
				// a name, and the name can answer differently the second time.
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					host, port, err := net.SplitHostPort(addr)
					if err != nil {
						return nil, fmt.Errorf("%w: bad address %q", ErrRefused, addr)
					}
					ips, err := publicAddrs(ctx, host, check)
					if err != nil {
						return nil, err
					}
					return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0], port))
				},
				// Probes are small and infrequent; a proxy environment variable is
				// not a place the marketplace's egress should be able to be
				// redirected by whoever set it.
				Proxy: nil,
			},
		},
	}
	return r
}

// Run probes one agent.
//
// target is the agent's declared probe endpoint and capabilities are what the
// agent's card claims. Both come from the card, which both the agent and the
// marketplace have signed, so an agent cannot be pointed at an address it did
// not declare.
func (r *Runner) Run(ctx context.Context, target, agentID string, capabilities []string, challenge string) (Report, error) {
	checked := time.Now().UTC()
	report := Report{AgentID: agentID, Target: target, CheckedAt: checked}
	endpoint, err := r.endpoint(target)
	if err != nil {
		return report, err
	}
	body, err := json.Marshal(request{
		Challenge: challenge, AgentID: agentID,
		Capabilities: capabilities, Version: r.version,
	})
	if err != nil {
		return report, fmt.Errorf("%w: encode probe: %w", ErrRefused, err)
	}
	ctx, cancel := context.WithTimeout(ctx, r.limits.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return report, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vtessera-probe/"+r.version)

	resp, err := r.client.Do(req)
	if err != nil {
		return report, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The agent answered, so something is known, but not what was asked.
		report.Results = []Result{{
			Capability: "probe-endpoint",
			Status:     StatusFail,
			Detail:     "probe endpoint answered " + resp.Status,
		}}
		return report, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, r.limits.MaxResponseBytes+1))
	if err != nil {
		return report, fmt.Errorf("%w: read probe response: %w", ErrRefused, err)
	}
	if int64(len(raw)) > r.limits.MaxResponseBytes {
		report.Results = []Result{{
			Capability: "probe-endpoint",
			Status:     StatusFail,
			Detail:     "probe response exceeded the size limit",
		}}
		return report, nil
	}
	var got response
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		report.Results = []Result{{
			Capability: "probe-endpoint",
			Status:     StatusFail,
			Detail:     "probe response did not match the probe schema",
		}}
		return report, nil
	}
	// The echo is what separates a live agent from a cached or proxied answer. An
	// agent that answers with a stale challenge has not just proved it is slow, it
	// has not proved anything, and reporting its capabilities as results would be
	// reporting a cache's claims as the agent's.
	if got.Challenge != challenge {
		return report, fmt.Errorf("%w: agent echoed challenge %q, not the one sent",
			ErrRefused, truncate(got.Challenge, 16))
	}
	if got.AgentID != "" && got.AgentID != agentID {
		return report, fmt.Errorf("%w: probe answered for agent %s, not %s",
			ErrRefused, got.AgentID, agentID)
	}
	report.Results = normalise(got.Results, capabilities)
	return report, nil
}

// normalise restricts the report to the capabilities the card claims.
//
// An agent cannot introduce a capability its card does not declare and have it
// recorded, which would let a probe be the thing that publishes an offer.
func normalise(got []Result, claimed []string) []Result {
	byCapability := make(map[string]Result, len(got))
	for _, res := range got {
		if _, dup := byCapability[res.Capability]; !dup {
			byCapability[res.Capability] = res
		}
	}
	out := make([]Result, 0, len(claimed))
	for _, capability := range claimed {
		res, ok := byCapability[capability]
		if !ok {
			// Silence about a capability is not a pass. An agent that omits one it
			// was asked about has declined to claim it.
			res = Result{Capability: capability, Status: StatusUnknown,
				Detail: "the agent did not report this capability"}
			out = append(out, res)
			continue
		}
		switch res.Status {
		case StatusPass, StatusFail, StatusUnknown:
		default:
			res = Result{Capability: capability, Status: StatusUnknown,
				Detail: "the agent reported a status this marketplace does not recognise"}
		}
		if len(res.Detail) > 200 {
			res.Detail = truncate(res.Detail, 200)
		}
		out = append(out, res)
	}
	return out
}

// endpoint validates a declared probe target and returns the URL to call.
//
// Only https, because a probe carries a challenge and an agent's capability
// list and neither should travel in the clear. A port is refused rather than
// defaulted, so a target has to be exactly what was declared.
func (r *Runner) endpoint(target string) (string, error) {
	if strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("%w: no probe target is declared", ErrTarget)
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("%w: probe target is not a URL: %w", ErrTarget, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("%w: probe target must be https, got %q", ErrTarget, u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: probe target must not carry credentials", ErrTarget)
	}
	if u.Port() == "" {
		return "", fmt.Errorf("%w: probe target must name a port, got %q", ErrTarget, target)
	}
	if _, err := net.LookupPort("tcp", u.Port()); err != nil {
		return "", fmt.Errorf("%w: probe target port is not valid", ErrTarget)
	}
	if u.Path == "" || u.Path == "/" {
		return "", fmt.Errorf("%w: probe target must name a path, got %q", ErrTarget, target)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: probe target must not carry a query or fragment", ErrTarget)
	}
	return u.String(), nil
}

// publicAddrs resolves a host and returns only its routable addresses.
//
// An address that is not globally routable is refused rather than filtered out,
// because a name with one public and one private address is a name that should
// not be called at all.
func publicAddrs(ctx context.Context, host string, check routable) ([]string, error) {
	if ip := net.ParseIP(host); ip != nil {
		if err := check(ip); err != nil {
			return nil, err
		}
		return []string{ip.String()}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve %q: %w", ErrTarget, host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %q resolved to no addresses", ErrTarget, host)
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if err := check(a.IP); err != nil {
			return nil, fmt.Errorf("%w: %q resolves to %s: %w", ErrTarget, host, a.IP, err)
		}
		out = append(out, a.IP.String())
	}
	return out, nil
}

// checkRoutable refuses any address that is not a public internet address.
//
// The list is deliberately longer than the obvious cases. The ones that matter
// most are the ones an agent would not think to avoid: a link-local address is
// where a cloud instance's metadata service lives, and a name resolving to both
// a public and a private address is the shape of a rebinding answer.
func checkRoutable(ip net.IP) error {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified(), ip.IsLoopback(), ip.IsPrivate(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return fmt.Errorf("%s is not a public address", ip)
	}
	for _, block := range blockedRanges {
		if block.net.Contains(ip) {
			return fmt.Errorf("%s is inside %s, which is not a public range", ip, block.name)
		}
	}
	return nil
}

type blockedRange struct {
	name string
	net  net.IPNet
}

var blockedRanges = func() []blockedRange {
	cidrs := []struct {
		name string
		cidr string
	}{
		// Shared address space for carrier NAT, which reaches infrastructure that
		// is not on the public internet and is not addressed by RFC1918.
		{"100.64.0.0/10", "100.64.0.0/10"},
		// Protocol assignments, including the 192.0.0.170/171 NAT64 discovery
		// pair, which is a way of naming an address that is not the one checked.
		{"192.0.0.0/24", "192.0.0.0/24"},
		{"192.0.2.0/24", "192.0.2.0/24"},
		{"198.51.100.0/24", "198.51.100.0/24"},
		{"203.0.113.0/24", "203.0.113.0/24"},
		// Benchmarking, and the reserved space above the last allocation.
		{"198.18.0.0/15", "198.18.0.0/15"},
		{"240.0.0.0/4", "240.0.0.0/4"},
		// IPv6 unique local and documentation.
		{"fc00::/7", "fc00::/7"},
		{"2001:db8::/32", "2001:db8::/32"},
	}
	out := make([]blockedRange, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c.cidr)
		if err != nil {
			panic("probe: bad blocked range " + c.cidr + ": " + err.Error())
		}
		out = append(out, blockedRange{name: c.name, net: *n})
	}
	return out
}()

// Statement is the signed form of a probe report.
//
// The results are signed because a probe is the only thing in this service that
// observes an agent's behaviour, and an unsigned observation is worth nothing to
// a directory deciding whether to trust a listing.
func Statement(report Report) attest.Probe {
	out := attest.Probe{
		AgentID:   report.AgentID,
		Target:    report.Target,
		Results:   make([]attest.ProbeResult, 0, len(report.Results)),
		CheckedAt: report.CheckedAt,
	}
	for _, res := range report.Results {
		out.Results = append(out.Results, attest.ProbeResult{
			Capability: res.Capability,
			Status:     string(res.Status),
			Detail:     res.Detail,
		})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
