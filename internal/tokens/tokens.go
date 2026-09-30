// Package tokens is the service-governed registry of settlement mints. Token
// identity is by mint address only — never by symbol — because symbols are not
// unique across issuers and are trivially spoofed.
//
// The registry is scoped to one cluster. That is the whole point: a mint address
// names an account on a particular chain, so a registry that does not say which
// chain is a registry that will happily price a trade in a token that does not
// exist where the money would move.
package tokens

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/gagliardetto/solana-go"
)

var (
	// ErrNotRegistered means the address is not a governed mint at all — not
	// even on another cluster. Nothing in this service has an opinion about it.
	ErrNotRegistered = errors.New("mint is not in the token registry")
	// ErrUngoverned means a real, governed token that is not governed on the
	// cluster this process is running. It is a different operational situation
	// from an unrecognised address: the seller chose it, the buyer cannot
	// correct it, and the fix is a configuration or governance decision.
	ErrUngoverned = errors.New("mint is not governed on this cluster")
	// ErrDisabled means the mint is governed here and switched off.
	ErrDisabled = errors.New("mint is registered but disabled")
	// ErrInvalid means the registry entry itself is malformed.
	ErrInvalid = errors.New("token registry entry is not valid")
)

// MaxDecimals mirrors the SPL Token limit.
const MaxDecimals = 9

// TokenProgram is the SPL Token program that owns every governed mint. The check
// is pinned to the classic program, so a Token-2022 stablecoin will fail
// preflight rather than be silently accepted. That is a known limitation
// carried into the deferred governance spec, which needs a per-entry program.
const TokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

// Token is one registry entry.
//
// MintAuthority and FreezeAuthority are observational pins, not identity. They
// record what the issuer looked like when the pin was taken so a later drift is
// detectable; identity remains the address, and scale is Decimals. Both
// authorities are optional because a mint may have none, and an absent one is
// as much a fact as a present one.
type Token struct {
	Address         string `json:"address"`
	Symbol          string `json:"symbol"`
	Decimals        int    `json:"decimals"`
	Enabled         bool   `json:"enabled"`
	Cluster         string `json:"cluster,omitempty"`
	MintAuthority   string `json:"mintAuthority,omitempty"`
	FreezeAuthority string `json:"freezeAuthority,omitempty"`
}

// MintInfo projects the entry onto the shared vocabulary type used by AGP.
func (t Token) MintInfo() domain.MintInfo {
	return domain.MintInfo{
		Address:  t.Address,
		Symbol:   t.Symbol,
		Decimals: t.Decimals,
		Known:    true,
	}
}

// MintAuthorityKey returns the pinned mint authority, or nil when the entry
// records that the mint has none. Exported because preflight compares the pin
// against the chain, and an absent pin is a fact to be checked, not a value to
// be re-parsed by each caller.
func (t Token) MintAuthorityKey() *solana.PublicKey { return optionalKey(t.MintAuthority) }

// FreezeAuthorityKey returns the pinned freeze authority, or nil when the entry
// records that the mint has none.
func (t Token) FreezeAuthorityKey() *solana.PublicKey { return optionalKey(t.FreezeAuthority) }

func optionalKey(address string) *solana.PublicKey {
	if address == "" {
		return nil
	}
	key, err := solana.PublicKeyFromBase58(address)
	if err != nil {
		return nil
	}
	return &key
}

func (t Token) validate() error {
	if _, err := solana.PublicKeyFromBase58(t.Address); err != nil {
		return fmt.Errorf("%w: address %q: %v", ErrInvalid, t.Address, err)
	}
	if strings.TrimSpace(t.Symbol) == "" {
		return fmt.Errorf("%w: %s has no symbol", ErrInvalid, t.Address)
	}
	if t.Decimals < 0 || t.Decimals > MaxDecimals {
		return fmt.Errorf("%w: %s has %d decimals", ErrInvalid, t.Address, t.Decimals)
	}
	// An authority pin that is present must at least be a real address: a typo
	// in a pin would otherwise be read as "this mint has no authority", which
	// is the one reading a rotation must never be allowed to produce.
	for name, address := range map[string]string{"mint": t.MintAuthority, "freeze": t.FreezeAuthority} {
		if address == "" {
			continue
		}
		if _, err := solana.PublicKeyFromBase58(address); err != nil {
			return fmt.Errorf("%w: %s has an unparseable %s authority %q: %v", ErrInvalid, t.Address, name, address, err)
		}
	}
	return nil
}

// Registry is the read surface the settlement and announcement paths depend on.
// Implementations must be safe for concurrent use.
type Registry interface {
	// Lookup returns the entry for a mint address.
	Lookup(address string) (Token, bool)
	// Enabled returns the entry for a mint address only if it is governed and
	// enabled on this registry's cluster, which is the precondition for settling
	// a trade.
	Enabled(address string) (Token, error)
	// List returns every entry, ordered by symbol then address.
	List() []Token
	// Cluster names the cluster this registry governs. It is what the service
	// reports and what a settlement request records.
	Cluster() cluster.Cluster
}

type registry struct {
	mu      sync.RWMutex
	cluster cluster.Cluster
	byMint  map[string]Token
	// elsewhere holds every governed mint on every other cluster, so that an
	// address governed somewhere but not here can be reported as ungoverned
	// rather than as unrecognised. The distinction is what tells a caller the
	// seller picked a token this service does not settle in, as opposed to a
	// string that was never a token.
	elsewhere map[string]cluster.Cluster
	ordered   []Token
}

// New builds a registry for one cluster from entries, rejecting invalid or
// duplicate mints. Use ForCluster for the governed sets; this exists for the
// test-local and operator-supplied cases, and requires a cluster so that no
// registry exists without one.
func New(c cluster.Cluster, entries []Token) (Registry, error) {
	if _, err := cluster.Parse(string(c)); err != nil {
		return nil, err
	}
	byMint := make(map[string]Token, len(entries))
	for _, entry := range entries {
		if err := entry.validate(); err != nil {
			return nil, err
		}
		if _, exists := byMint[entry.Address]; exists {
			return nil, fmt.Errorf("%w: duplicate mint %s", ErrInvalid, entry.Address)
		}
		entry.Cluster = string(c)
		byMint[entry.Address] = entry
	}
	// The other clusters' mints are indexed here rather than in ForCluster, so
	// that every registry can tell "a real token, wrong chain" from "not a
	// token". Building it in ForCluster alone meant a directly constructed
	// registry reported ErrNotRegistered for a governed address, which is a
	// misleading answer that points an operator at the wrong problem.
	return &registry{
		cluster:   c,
		byMint:    byMint,
		ordered:   sorted(entries),
		elsewhere: elsewhere(c, entries),
	}, nil
}

// governed is one row of the verified on-chain table. See the testdata snapshot
// and design spec §3.1 for provenance; the authorities were read from
// getAccountInfo on the named networks and are pins, not identity.
type governed struct {
	address         string
	symbol          string
	decimals        int
	mintAuthority   string
	freezeAuthority string
}

// mainnet mints, verified 2026-09-27 against api.mainnet-beta.solana.com and
// solana-rpc.publicnode.com independently. Re-derive rather than copy: this
// table is exactly where a transcription error once hid a 30-character-prefix
// lookalike that resolved to nothing.
var mainnetMints = []governed{
	{
		address:         "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		symbol:          "USDC",
		decimals:        6,
		mintAuthority:   "BJE5MMbqXjVwjAF7oxwPYXnTXDyspzZyt4vwenNw5ruG",
		freezeAuthority: "7dGbd2QZcCKcTndnHcTL8q7SMVXAkp688NTQYwrRCrar",
	},
	{
		address:         "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr",
		symbol:          "EURC",
		decimals:        6,
		mintAuthority:   "Hy9168u7b2Toujh8SJKtKK8DWbyaBbXPzxm337R4o4XY",
		freezeAuthority: "6FF2CPcL6fat16QVJH6W27DeBcX7TSqbwY3a5wtyVJLC",
	},
}

// devnet mints, verified 2026-09-30 against api.devnet.solana.com and
// devnet.rpcpool.com independently.
//
// EURC has the same address on devnet as on mainnet-beta and is a completely
// different account, with a different mint authority — the ledgers are
// independent, so the same address holds different state. An implementation
// that deduplicates by address, or that verifies address alone, is wrong.
var devnetMints = []governed{
	{
		address:         "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
		symbol:          "USDC",
		decimals:        6,
		mintAuthority:   "GrNg1XM2ctzeE2mXxXCfhcTUbejM8Z4z4wNVTy2FjMEz",
		freezeAuthority: "CJtyoKSLrktozQzjERTiK3btQtiTK3nN4QrqGHLidyCT",
	},
	{
		address:       "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr",
		symbol:        "EURC",
		decimals:      6,
		mintAuthority: "DuYQKfdunafuUqS7h6gugadrAiprxwtwBZFZLAMk4A99",
		// Circle freezes its own mints: the COption tag at raw[46] is 1 here and
		// the authority at raw[50:82] is the same as the mint authority. Verified
		// against devnet getAccountInfo on both public endpoints. Pinning this
		// as "none" would be indistinguishable from a mint an issuer had genuinely
		// given up the ability to freeze.
		freezeAuthority: "DuYQKfdunafuUqS7h6gugadrAiprxwtwBZFZLAMk4A99",
	},
}

// localnet governs nothing. Its mints are the operator's own test fixtures and
// are supplied explicitly, because a validator has no stable token to ship with.
var localnetMints []governed

// ForCluster returns the governed registry for a cluster. It is the only
// constructor: there is deliberately no Default, because a default is how a
// service ends up pricing in mainnet's tokens while pointed at a devnet chain.
//
// Extra mints may only be supplied on localnet, where the governed set is empty
// and the tokens are the operator's. On any other cluster an extra would widen
// the governed set in production, which is the one thing the per-cluster scoping
// exists to prevent.
func ForCluster(c cluster.Cluster, extra ...Token) (Registry, error) {
	if _, err := cluster.Parse(string(c)); err != nil {
		return nil, err
	}
	if len(extra) > 0 && !c.AllowsExtraMints() {
		return nil, fmt.Errorf("%w: extra mints are not permitted on %s", ErrInvalid, c)
	}
	rows := mintsFor(c)
	entries := make([]Token, 0, len(rows)+len(extra))
	for _, row := range rows {
		entries = append(entries, Token{
			Address:         row.address,
			Symbol:          row.symbol,
			Decimals:        row.decimals,
			Enabled:         true,
			Cluster:         string(c),
			MintAuthority:   row.mintAuthority,
			FreezeAuthority: row.freezeAuthority,
		})
	}
	entries = append(entries, extra...)
	return New(c, entries)
}

func mintsFor(c cluster.Cluster) []governed {
	switch c {
	case cluster.MainnetBeta:
		return mainnetMints
	case cluster.Devnet:
		return devnetMints
	case cluster.Localnet:
		return localnetMints
	}
	return nil
}

// elsewhere indexes every governed mint that is not in this registry, so the
// ungoverned-versus-unregistered distinction survives the scoping.
func elsewhere(c cluster.Cluster, mine []Token) map[string]cluster.Cluster {
	have := make(map[string]bool, len(mine))
	for _, entry := range mine {
		have[entry.Address] = true
	}
	out := map[string]cluster.Cluster{}
	for other, rows := range map[cluster.Cluster][]governed{
		cluster.MainnetBeta: mainnetMints,
		cluster.Devnet:      devnetMints,
	} {
		if other == c {
			continue
		}
		for _, row := range rows {
			if !have[row.address] {
				out[row.address] = other
			}
		}
	}
	return out
}

func (r *registry) Cluster() cluster.Cluster {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cluster
}

func (r *registry) Lookup(address string) (Token, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byMint[address]
	return entry, ok
}

func (r *registry) Enabled(address string) (Token, error) {
	entry, ok := r.Lookup(address)
	if !ok {
		r.mu.RLock()
		other, governedElsewhere := r.elsewhere[address]
		c := r.cluster
		r.mu.RUnlock()
		if governedElsewhere {
			return Token{}, fmt.Errorf("%w: %s is governed on %s, not %s", ErrUngoverned, address, other, c)
		}
		return Token{}, fmt.Errorf("%w: %s", ErrNotRegistered, address)
	}
	if !entry.Enabled {
		return Token{}, fmt.Errorf("%w: %s (%s)", ErrDisabled, address, entry.Symbol)
	}
	return entry, nil
}

func (r *registry) List() []Token {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Token, len(r.ordered))
	copy(out, r.ordered)
	return out
}

func sorted(entries []Token) []Token {
	out := make([]Token, len(entries))
	copy(out, entries)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Address < out[j].Address
	})
	return out
}
