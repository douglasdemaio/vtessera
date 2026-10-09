// Package vtessera is a read-only client for vtessera's public HTTP API.
//
// It is a separate module from the vtessera service itself, and that separation is
// the point rather than an accident of layout: everything here is reachable by any
// outside agent holding a URL, so it must be able to depend on nothing the
// service considers private. The service holds the marketplace signing key, the
// session secret and the database. Nothing built on this package holds any of
// them. It reads the same endpoints an agent reads with curl.
package vtessera

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxBody caps a response from the marketplace. The service's own limits are small
// and this reads untrusted JSON over the network, so the read is bounded rather
// than trusted to be as short as it usually is.
const maxBody = 4 << 20

// Client is a read-only client for vtessera's public API.
//
// Every method here hits an endpoint that requires no session. There is
// deliberately no method that sends a credential: an MCP server that could
// publish an offer or accept a trade on an agent's behalf would be holding
// something to lose, and an agent that wants to trade has an HTTP API.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a client for a vtessera deployment.
//
// The base URL is required and is checked at startup rather than on first use, so
// a misconfigured server says so immediately instead of answering every tool call
// with a confusing transport error.
func NewClient(baseURL string, timeout time.Duration) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, fmt.Errorf("a vtessera base URL is required, for example http://localhost:8080")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("base URL %q is not a URL: %w", baseURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("base URL must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("base URL %q has no host", baseURL)
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		baseURL: trimmed,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// get fetches a path and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, target, nil, path, out)
}

// postJSON sends a JSON body and decodes the JSON response.
func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	encoded, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, c.baseURL+path, encoded, path, out)
}

// do performs one marketplace call.
//
// The marketplace's error bodies carry a code and a message, and both are
// preserved: an operator reading a tool result needs to know a refusal was a
// refusal and not a transport failure. GET and POST share this because a bound
// applied to one and not the other is a limit that holds only when convenient.
func (c *Client) do(ctx context.Context, method, target string, encoded []byte, path string, out any) error {
	var body io.Reader
	if encoded != nil {
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	if encoded != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", path, err)
	}
	defer resp.Body.Close()
	// One byte past the cap, so a body that does not fit is refused instead of
	// arriving truncated. Truncation would normally break the decode below, and a
	// limit whose failure depends on where it lands is not a limit.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if len(raw) > maxBody {
		return fmt.Errorf("reading %s: the marketplace sent more than the %d byte cap", path, maxBody)
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

// APIError is a refusal from the marketplace, kept verbatim.
//
// The service distinguishes a missing agent from an unpriced mint from a spent
// cap, and an MCP client that flattened those into "request failed" would be
// hiding the one thing the caller could act on.
type APIError struct {
	Path   string
	Status int
	Body   string
	// RPCCode and Message are set when the marketplace refused in a JSON-RPC error
	// rather than an HTTP status. It answers 200 in that case, so reporting the
	// status would tell a reader the request succeeded.
	RPCCode string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		// The marketplace's own message already names its code, so nothing is
		// prefixed onto it here.
		return fmt.Sprintf("vtessera %s refused: %s", e.Path, e.Message)
	}
	return fmt.Sprintf("vtessera %s answered %d: %s", e.Path, e.Status, strings.TrimSpace(e.Body))
}

// Code is the marketplace's own error code, when it sent one.
func (e *APIError) Code() string {
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(e.Body), &payload); err != nil {
		return ""
	}
	return payload.Code
}

// Health is the deployment's answer from /healthz.
//
// The verification key is here because that is what a reader needs in order to
// check anything else this marketplace signs, and the sandbox flag is here
// because the most important question about any answer from this service is
// whether real value can move behind it.
type Health struct {
	Status          string `json:"status"`
	Version         string `json:"version"`
	VerificationKey string `json:"verificationKey"`
	Sandbox         bool   `json:"sandbox"`
	Cluster         string `json:"cluster"`
	GenesisHash     string `json:"genesisHash"`
	SettlementTier  string `json:"settlementTier"`
}

// Capability status values, as the marketplace writes them.
//
// Declared here rather than imported from the service because this module must not
// depend on the service: these are wire values, and a test pins them, so a change
// on either side fails a test here instead of quietly reporting every probe result
// as unknown.
const (
	StatusPass    = "pass"
	StatusFail    = "fail"
	StatusUnknown = "unknown"
)

// BaseURL is the marketplace this client reads, for logs and error text.
func (c *Client) BaseURL() string { return c.baseURL }

// Health reports whether the marketplace is answering and what it is.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.get(ctx, "/healthz", nil, &out)
	return out, err
}

// Agent is one registered agent as the marketplace publishes it.
//
// The card is the agent's own declaration, reported verbatim: this server does not
// restate an agent's claims in its own words, because a summary that differs from
// the card is a claim nobody signed.
type Agent struct {
	ID        string   `json:"id"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
	Card      CardView `json:"card"`
}

// CardView is the agent's declared card, as published.
type CardView struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Version         string   `json:"version"`
	URL             string   `json:"url"`
	PublicKey       string   `json:"publicKey"`
	Capabilities    []string `json:"capabilities"`
	Skills          []Skill  `json:"skills"`
	Currencies      []string `json:"currencies"`
	SettlementModes []string `json:"settlementModes"`
	ProbeTarget     string   `json:"probeTarget"`
}

// Skill is one declared skill on a card.
type Skill struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Tags   []string `json:"tags"`
	Input  []string `json:"inputModes"`
	Output []string `json:"outputModes"`
}

// Agent returns one agent's listing and status.
func (c *Client) Agent(ctx context.Context, id string) (Agent, error) {
	var out Agent
	err := c.get(ctx, "/v1/agents/"+url.PathEscape(id), nil, &out)
	return out, err
}

// Offer is one published offer.
//
// PriceAmount is the string the seller asked for, "10.00", and PriceMint is the
// governed stablecoin they want. There is no base-unit field on the wire: the
// amount is a decimal string on purpose, because a float would quietly lose the
// precision a cap is computed in.
type Offer struct {
	ID              string   `json:"id"`
	AgentID         string   `json:"agentId"`
	Direction       string   `json:"direction"`
	Description     string   `json:"description"`
	Capabilities    []string `json:"capabilities"`
	PriceAmount     string   `json:"priceAmount"`
	PriceMint       string   `json:"priceMint"`
	SettlementModes []string `json:"settlementModes"`
	Status          string   `json:"status"`
	CreatedAt       string   `json:"createdAt"`
	UpdatedAt       string   `json:"updatedAt"`
}

// Offer returns one offer.
func (c *Client) Offer(ctx context.Context, id string) (Offer, error) {
	var out Offer
	err := c.get(ctx, "/v1/offers/"+url.PathEscape(id), nil, &out)
	return out, err
}

// SearchOffers finds open offers. Empty filters are left out of the query rather
// than sent blank, so the marketplace applies its own defaults instead of being
// told to match an empty string.
func (c *Client) SearchOffers(ctx context.Context, q OfferQuery) ([]Offer, error) {
	query := url.Values{}
	if q.Capability != "" {
		query.Set("capability", q.Capability)
	}
	if q.Mint != "" {
		query.Set("mint", q.Mint)
	}
	if q.Mode != "" {
		query.Set("mode", q.Mode)
	}
	if q.Direction != "" {
		query.Set("direction", q.Direction)
	}
	if q.Text != "" {
		query.Set("q", q.Text)
	}
	if q.Limit > 0 {
		query.Set("limit", fmt.Sprint(q.Limit))
	}
	var payload struct {
		Offers []Offer `json:"offers"`
	}
	err := c.get(ctx, "/v1/offers", query, &payload)
	return payload.Offers, err
}

// OfferQuery filters an offer search. Every field is optional.
type OfferQuery struct {
	Capability string
	Mint       string
	Mode       string
	Direction  string
	Text       string
	Limit      int
}

// Side is one side of a card attestation verdict.
type Side struct {
	Attested  bool   `json:"attested"`
	Valid     bool   `json:"valid"`
	KeyID     string `json:"keyId"`
	Signature any    `json:"signature"`
}

// CardAttestation is the marketplace's and the agent's own account of a card.
type CardAttestation struct {
	AgentID        string `json:"agentId"`
	Marketplace    Side   `json:"marketplace"`
	Agent          Side   `json:"agent"`
	CanonicalForm  string `json:"canonicalForm"`
	PreAttestation bool   `json:"preAttestation"`
}

// CardAttestation reports who vouched for an agent's card.
//
// This is the answer to "did this marketplace publish this card", and it is
// deliberately two-sided: an unsigned card is not the same as a card this
// marketplace declines to vouch for.
func (c *Client) CardAttestation(ctx context.Context, id string) (CardAttestation, error) {
	var out CardAttestation
	err := c.get(ctx, "/v1/agents/"+url.PathEscape(id)+"/attestation", nil, &out)
	return out, err
}

// CapabilityResult is one capability's outcome within a probe.
type CapabilityResult struct {
	Capability string `json:"capability"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	LatencyMS  int64  `json:"latencyMs"`
}

// probeStatusFail is the status a result carries when the agent answered and the
// capability did not work. A report where nothing passed is reported, never
// presented as an absence of failures.
const probeStatusFail = "fail"

// CapabilityReport is the marketplace's last probe of an agent.
type CapabilityReport struct {
	AgentID    string             `json:"agentId"`
	Target     string             `json:"target"`
	Results    []CapabilityResult `json:"results"`
	Passed     bool               `json:"passed"`
	Valid      bool               `json:"valid"`
	CheckedAt  string             `json:"checkedAt"`
	Signature  any                `json:"signature"`
	AttestedBy string             `json:"attestedBy"`
}

// CapabilityReport returns an agent's last recorded capability probe.
//
// An agent that has never been probed answers 404 NOT_PROBED, which is reported
// to the caller as an error rather than as an empty passing report. An empty
// report would read to a model as "nothing failed".
func (c *Client) CapabilityReport(ctx context.Context, id string) (CapabilityReport, error) {
	var out CapabilityReport
	err := c.get(ctx, "/v1/agents/"+url.PathEscape(id)+"/capabilities", nil, &out)
	return out, err
}

// Route asks the marketplace which agent should serve an intent, and what it
// costs. This is the routing table, not a search: it applies the marketplace's
// own ordering rather than the caller's.
func (c *Client) Route(ctx context.Context, intent RouteIntent) (RouteResult, error) {
	var out RouteResult
	// The intent goes inside params because /agp/route is JSON-RPC, and the
	// answer is unwrapped from result. Both halves matter: the service answers a
	// malformed call with HTTP 200 and an error object, so posting the intent as the
	// body and decoding the envelope into RouteResult would report an empty route
	// rather than the refusal, which is the one thing a caller can act on.
	payload := intent.Payload
	if payload == nil {
		// The marketplace rejects a nil payload, so an omitted one is sent as an
		// empty object: a capability that takes no arguments is still routable.
		payload = map[string]any{}
	}
	params := map[string]any{
		"target_capability": intent.TargetCapability,
		"payload":           payload,
	}
	if len(intent.PolicyConstraints) > 0 {
		params["policy_constraints"] = intent.PolicyConstraints
	}
	call := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "agp/route",
		"params":  params,
	}
	var envelope rpcEnvelope
	if err := c.postJSON(ctx, "/agp/route", call, &envelope); err != nil {
		return out, err
	}
	if envelope.Error != nil {
		return out, &APIError{
			Path:    "/agp/route",
			RPCCode: fmt.Sprintf("%d", envelope.Error.Code),
			Message: envelope.Error.Message,
		}
	}
	return envelope.Result, nil
}

// rpcEnvelope is a JSON-RPC answer. The service reports a refused route in here
// with HTTP 200, so an error object is a refusal rather than a transport failure.
type rpcEnvelope struct {
	Result RouteResult  `json:"result"`
	Error  *rpcRPCError `json:"error"`
}

// rpcRPCError is the error member of a JSON-RPC answer.
type rpcRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// RouteIntent is a capability to satisfy, with optional constraints.
type RouteIntent struct {
	TargetCapability  string         `json:"target_capability"`
	Payload           map[string]any `json:"payload,omitempty"`
	PolicyConstraints map[string]any `json:"policy_constraints,omitempty"`
}

// RouteResult is the marketplace's chosen route.
type RouteResult struct {
	TargetCapability string         `json:"target_capability"`
	Route            RouteEntry     `json:"route"`
	Considered       int            `json:"considered"`
	Rejected         map[string]int `json:"rejected"`
	TableFingerprint string         `json:"table_fingerprint"`
	TableAsOf        string         `json:"table_as_of"`
}

// RouteEntry is the agent the marketplace chose, and what it charges.
//
// Path is the A2A squad path of the chosen offer, which is what a caller sends its
// request to. It is reported as the marketplace wrote it, because rewriting a path
// here would be this server guessing at an address the marketplace signed.
type RouteEntry struct {
	Path         string         `json:"path"`
	Cost         float64        `json:"cost"`
	Policy       map[string]any `json:"policy"`
	CostAmount   string         `json:"cost_amount"`
	CostMint     string         `json:"cost_mint"`
	OfferID      string         `json:"offer_id"`
	AgentID      string         `json:"agent_id"`
	Direction    string         `json:"direction"`
	Announcement map[string]any `json:"announcement"`
}
