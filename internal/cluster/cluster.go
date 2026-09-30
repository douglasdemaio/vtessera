// Package cluster is the single place a Solana cluster is defined. A cluster is
// not a display string: it is a chain identity, verified against the node's
// genesis hash before the service will settle anything.
//
// The service must never infer its cluster from a URL. A devnet endpoint and a
// mainnet endpoint answer the same JSON-RPC methods with the same shapes, so
// VTESSERA_RPC_URL naming a chain proves nothing — only the genesis hash does.
package cluster

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Cluster is a named Solana cluster the service can settle against.
type Cluster string

const (
	// MainnetBeta is Solana mainnet-beta, where real value settles.
	MainnetBeta Cluster = "mainnet-beta"
	// Devnet is the public devnet. No real value, but a real chain identity.
	Devnet Cluster = "devnet"
	// Localnet is a disposable validator under the operator's control. It has
	// no meaningful genesis pin, so it is constrained by host instead.
	Localnet Cluster = "localnet"
	// Testnet is named so that "testnet" is refused deliberately rather than by
	// falling through to an unknown-cluster error. It is a real network with a
	// real genesis hash and this service governs no mints on it.
	//
	// It is not a Cluster this service will settle against: Parse rejects it and
	// nothing in the package returns it without an error.
	Testnet Cluster = "testnet"
)

var (
	// ErrUnknownCluster marks a value that names no cluster at all.
	ErrUnknownCluster = errors.New("unknown cluster")
	// ErrClusterUnsupported marks a real cluster this service will not settle
	// against, named in the message so the operator is not left guessing.
	ErrClusterUnsupported = errors.New("unsupported cluster")
	// ErrClusterMismatch means the endpoint's genesis hash contradicts the
	// declared cluster. The endpoint is not the chain that was declared.
	ErrClusterMismatch = errors.New("cluster mismatch")
	// ErrEndpointNotAllowed means the localnet endpoint resolves somewhere a
	// disposable chain must not be: anything that is not loopback.
	ErrEndpointNotAllowed = errors.New("endpoint not allowed")
)

// genesisPins is the chain identity per cluster, read from getGenesisHash on
// each of the public networks on 2026-09-27. localnet is absent because a
// disposable validator's genesis hash carries no meaning off that machine, and
// testnet is absent because this service refuses it before reaching here.
//
// These are the load-bearing constants of the whole package. A mismatch between
// one of these and a node's reported genesis hash means either a provider
// incident or a hijack, and the correct response is to stop — never to edit the
// pin until the error disappears. See design spec §12.4.
var genesisPins = map[Cluster]string{
	MainnetBeta: "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d",
	Devnet:      "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG",
}

// supported is ordered, and is what an error message names.
var supported = []Cluster{MainnetBeta, Devnet, Localnet}

// Parse resolves a configured cluster name. Matching is case-insensitive and
// tolerates surrounding whitespace, because a value that survives that and names
// nothing is a genuine operator mistake rather than a formatting one.
//
// testnet is recognised and rejected. It differs from devnet by one character
// in a string an operator types, so it is a supported network that this service
// will not settle against — said plainly, rather than being mistaken for an
// unknown cluster and guessed at.
func Parse(s string) (Cluster, error) {
	name := strings.ToLower(strings.TrimSpace(s))
	if name == "" {
		return "", fmt.Errorf("%w: VTESSERA_CLUSTER is not set; supported: %s", ErrUnknownCluster, names())
	}
	if name == string(Testnet) {
		return "", fmt.Errorf("%w: %s is not supported; supported: %s", ErrClusterUnsupported, Testnet, names())
	}
	for _, c := range supported {
		if string(c) == name {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: %q; supported: %s", ErrUnknownCluster, s, names())
}

// String returns the cluster's canonical name.
func (c Cluster) String() string { return string(c) }

// GenesisHash is the chain identity this cluster must report, or "" for
// localnet, which is identified by host rather than by genesis hash.
func (c Cluster) GenesisHash() string { return genesisPins[c] }

// IsProduction reports whether real value can move. mainnet-beta is the only
// production cluster: it is where a settlement is irreversible in the way a
// counterparty means, and where the service's own defaults are not overridable.
func (c Cluster) IsProduction() bool { return c == MainnetBeta }

// AllowsExtraMints reports whether an operator may widen the governed mint set
// by configuration. Only localnet, where the tokens are the operator's own test
// fixtures and the governed set is empty to begin with.
func (c Cluster) AllowsExtraMints() bool { return c == Localnet }

// RequiresLoopback reports whether the RPC endpoint must resolve to a loopback
// address. Only localnet, because a disposable chain has no genesis pin and the
// host is the only thing binding it to this machine.
func (c Cluster) RequiresLoopback() bool { return c == Localnet }

// Verify compares a genesis hash reported by the node against this cluster's
// pin. An empty pin is localnet, which has nothing to compare and must be gated
// by its endpoint host instead.
func (c Cluster) Verify(reported string) error {
	want := c.GenesisHash()
	if want == "" {
		return nil
	}
	if reported == want {
		return nil
	}
	return fmt.Errorf("%w: VTESSERA_CLUSTER=%s expects genesis %s but the endpoint reports %s",
		ErrClusterMismatch, c, want, reported)
}

// CheckEndpoint enforces the host rules a cluster imposes. On mainnet-beta and
// devnet there is no host rule: any endpoint reporting the right genesis hash is
// acceptable, which is what makes a private or self-operated node a
// configuration choice rather than a code change.
//
// On localnet the endpoint must resolve to loopback. The literal string
// "localhost" is not trusted on its own, because it can be pointed at a
// non-loopback address in /etc/hosts, and the string is not the destination —
// the address the name resolves to is.
func (c Cluster) CheckEndpoint(endpoint string, allowHosts []string) error {
	if !c.RequiresLoopback() {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return fmt.Errorf("%w: %q is not a URL: %v", ErrEndpointNotAllowed, endpoint, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%w: %q must be an http or https URL", ErrEndpointNotAllowed, endpoint)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("%w: %q has no host", ErrEndpointNotAllowed, endpoint)
	}
	for _, allowed := range allowHosts {
		if strings.EqualFold(strings.TrimSpace(allowed), host) {
			return nil
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("%w: %s is %s, not a loopback address", ErrEndpointNotAllowed, host, ip)
	}
	// A name, so resolve it and judge the address rather than the spelling.
	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve %s: %v", ErrEndpointNotAllowed, host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %s resolves to no addresses", ErrEndpointNotAllowed, host)
	}
	for _, addr := range addrs {
		if !addr.IsLoopback() {
			return fmt.Errorf("%w: %s resolves to %s, not a loopback address", ErrEndpointNotAllowed, host, addr)
		}
	}
	return nil
}

func names() string {
	out := make([]string, len(supported))
	for i, c := range supported {
		out[i] = string(c)
	}
	return strings.Join(out, ", ")
}
