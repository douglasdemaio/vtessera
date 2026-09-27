// Package fees implements the marketplace settlement fee policy: a flat,
// non-negotiable per-settlement fee that the buyer pays inside the same
// transaction that moves the trade amount. Embedding the fee in the buyer's
// transaction is the enforcement mechanism — there is no custody and no smart
// contract, so the fee cannot be removed without invalidating the settlement.
package fees

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

const (
	// DefaultLamports is 0.0005 SOL.
	DefaultLamports uint64 = 500_000
	// DefaultWallet receives every on-chain settlement fee.
	DefaultWallet = "J59EPyPHf9wtoLjf8rG4f9cARnLnUPKCdNwZX241rakh"
)

var (
	ErrInvalidLamports = errors.New("fee must be greater than zero")
	ErrInvalidWallet   = errors.New("fee wallet is not a valid Solana address")
)

// Policy is an immutable, validated fee policy. Use New or Default to build one;
// the zero value is deliberately not usable.
type Policy struct {
	lamports uint64
	wallet   solana.PublicKey
}

// New validates and returns a fee policy. A zero lamport fee is rejected: the
// fee is what makes the settlement non-strippable.
func New(lamports uint64, wallet string) (Policy, error) {
	if lamports == 0 {
		return Policy{}, fmt.Errorf("%w: got %d", ErrInvalidLamports, lamports)
	}
	key, err := solana.PublicKeyFromBase58(wallet)
	if err != nil {
		return Policy{}, fmt.Errorf("%w: %q: %v", ErrInvalidWallet, wallet, err)
	}
	return Policy{lamports: lamports, wallet: key}, nil
}

// Default returns the production policy from the design spec.
func Default() Policy {
	return Policy{lamports: DefaultLamports, wallet: solana.MustPublicKeyFromBase58(DefaultWallet)}
}

func (p Policy) Lamports() uint64 { return p.lamports }

func (p Policy) Wallet() solana.PublicKey { return p.wallet }

func (p Policy) WalletAddress() string { return p.wallet.String() }

// Description is the human-readable form used in API responses and AGP
// announcements, for example "500000 lamports".
func (p Policy) Description() string {
	return fmt.Sprintf("%d lamports", p.lamports)
}

func (p Policy) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Lamports    uint64 `json:"lamports"`
		Wallet      string `json:"wallet"`
		Description string `json:"description"`
	}{
		Lamports:    p.lamports,
		Wallet:      p.WalletAddress(),
		Description: p.Description(),
	})
}
