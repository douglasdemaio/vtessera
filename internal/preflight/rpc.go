package preflight

import (
	"context"
	"fmt"

	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
)

// AccountInfo is the slice of account state preflight reads. It is deliberately
// not the settlement RPC interface: preflight needs reads the settlement path
// never performs, and widening that interface would force every existing test
// fake to implement methods no settlement test exercises.
type AccountInfo struct {
	// Owner is the program that owns the account. A mint owned by anything
	// other than the SPL Token program is not the token we priced.
	Owner solana.PublicKey
	// Data is the raw account bytes, or nil when the account does not exist.
	Data []byte
	// Exists distinguishes an account holding zero bytes from a missing one.
	// A missing account still returns a non-nil AccountInfo with Exists false,
	// because "not there" is an answer and not a transport failure.
	Exists bool
}

// RPC is the read surface preflight needs from a Solana node.
type RPC interface {
	GetGenesisHash(ctx context.Context) (string, error)
	GetAccountInfo(ctx context.Context, addr solana.PublicKey) (AccountInfo, error)
	GetBalance(ctx context.Context, addr solana.PublicKey) (uint64, error)
	GetMinimumBalanceForRentExemption(ctx context.Context, dataLen uint64) (uint64, error)
}

// Mint is a mint account decoded from chain, and the properties preflight checks
// against the governed pin.
type Mint struct {
	Decimals        uint8
	IsInitialized   bool
	MintAuthority   *solana.PublicKey
	FreezeAuthority *solana.PublicKey
}

// DecodeMint unpacks an SPL Token mint account. A nil or truncated account is
// an error rather than a zero-valued mint: silently reading a missing account as
// "zero decimals" is how a 10^6 error gets in.
func DecodeMint(data []byte) (Mint, error) {
	if len(data) == 0 {
		return Mint{}, fmt.Errorf("account is empty")
	}
	var unpacked token.Mint
	if err := bin.NewBinDecoder(data).Decode(&unpacked); err != nil {
		return Mint{}, fmt.Errorf("decode mint account: %w", err)
	}
	return Mint{
		Decimals:        unpacked.Decimals,
		IsInitialized:   unpacked.IsInitialized,
		MintAuthority:   unpacked.MintAuthority,
		FreezeAuthority: unpacked.FreezeAuthority,
	}, nil
}
