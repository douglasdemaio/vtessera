// Package tokens is the service-governed registry of settlement mints. Token
// identity is by mint address only — never by symbol — because symbols are not
// unique across issuers and are trivially spoofed.
package tokens

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/gagliardetto/solana-go"
)

var (
	ErrNotRegistered = errors.New("mint is not in the token registry")
	ErrDisabled      = errors.New("mint is registered but disabled")
	ErrInvalid       = errors.New("token registry entry is not valid")
)

// MaxDecimals mirrors the SPL Token limit.
const MaxDecimals = 9

// Token is one registry entry.
type Token struct {
	Address  string `json:"address"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
	Enabled  bool   `json:"enabled"`
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
	return nil
}

// Registry is the read surface the settlement and announcement paths depend on.
// Implementations must be safe for concurrent use.
type Registry interface {
	// Lookup returns the entry for a mint address.
	Lookup(address string) (Token, bool)
	// Enabled returns the entry for a mint address only if it is registered and
	// enabled, which is the precondition for settling a trade.
	Enabled(address string) (Token, error)
	// List returns every entry, ordered by symbol then address.
	List() []Token
}

type registry struct {
	mu      sync.RWMutex
	byMint  map[string]Token
	ordered []Token
}

// New builds a registry from entries, rejecting invalid or duplicate mints.
func New(entries []Token) (Registry, error) {
	byMint := make(map[string]Token, len(entries))
	for _, entry := range entries {
		if err := entry.validate(); err != nil {
			return nil, err
		}
		if _, exists := byMint[entry.Address]; exists {
			return nil, fmt.Errorf("%w: duplicate mint %s", ErrInvalid, entry.Address)
		}
		byMint[entry.Address] = entry
	}
	return &registry{byMint: byMint, ordered: sorted(entries)}, nil
}

// Default returns the launch registry: USDC and EURC.
//
// The EURC address is Circle's mint on mainnet-beta. It was previously a
// 30-character-prefix lookalike, ...c2iXXcyK85CNzz7iwQc, which resolves to a
// null account; the two differ only after that shared prefix, which is exactly
// why a visual check missed it. Verified 2026-09-28 at mainnet slot 451247490
// via getAccountInfo: owner Tokenkeg..., type mint, decimals 6, initialized.
func Default() Registry {
	reg, err := New([]Token{
		{
			Address:  "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
			Symbol:   "USDC",
			Decimals: 6,
			Enabled:  true,
		},
		{
			Address:  "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr",
			Symbol:   "EURC",
			Decimals: 6,
			Enabled:  true,
		},
	})
	if err != nil {
		panic(err)
	}
	return reg
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
