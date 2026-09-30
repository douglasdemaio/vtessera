// Package fees implements the marketplace settlement fee policy: a flat,
// non-negotiable per-settlement fee that the buyer pays inside the same
// transaction that moves the trade amount. Embedding the fee in the buyer's
// transaction is the enforcement mechanism — there is no custody and no smart
// contract, so a settlement missing the fee is one this service will not
// recognise.
//
// That is a limit on recognition, not on the chain. A buyer who strips the fee
// submits the remaining instructions, and the chain executes them: the trade
// amount has already reached the seller, and the memo still lands. Verification
// then fails, the settlement is refused as a mismatch, and the trade ends
// disputed with no tessera and no ledger entry. The transfer is not reversed.
// The fee deters tampering; it does not prevent it.
package fees

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

const (
	// DefaultLamports is 0.000001 SOL.
	//
	// Phase 2 charged 500,000 lamports, which is below the 650,240 lamport rent
	// exemption for a zero-data account on every real cluster: the fee transfer
	// would have failed on a wallet that did not already hold a balance above
	// that line. The fee exists to bind a settlement to a real signature, not to
	// raise money, so it is set just above a base transaction.
	DefaultLamports uint64 = 1_000
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

// New validates and returns a fee policy. A zero lamport fee is rejected
// because it would make stripping the fee cost the buyer nothing.
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
