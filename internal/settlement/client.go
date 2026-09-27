package settlement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// ErrTransactionNotFound means the signature is not visible yet, which during
// the confirm flow is expected rather than fatal: RPC finality lags behind
// submission and the caller retries with backoff.
var ErrTransactionNotFound = errors.New("transaction not found on chain")

// Fetched is a transaction as the chain reports it.
type Fetched struct {
	Transaction *solana.Transaction
	Slot        uint64
	// ExecErr is the on-chain execution error, non-nil when the transaction
	// landed but failed.
	ExecErr error
}

// Client is the narrow slice of Solana RPC this package needs. Keeping it an
// interface lets the build and verify logic be unit tested without a validator.
type Client interface {
	// LatestBlockhash returns a recent blockhash and the last block height at
	// which it is still valid.
	LatestBlockhash(ctx context.Context) (solana.Hash, uint64, error)
	// AccountExists reports whether an account is present on chain.
	AccountExists(ctx context.Context, account solana.PublicKey) (bool, error)
	// Transaction fetches a confirmed transaction, returning
	// ErrTransactionNotFound when it is not visible yet.
	Transaction(ctx context.Context, sig solana.Signature) (Fetched, error)
}

// RPCClient adapts a solana-go JSON-RPC client to Client.
type RPCClient struct {
	inner *rpc.Client
}

// DefaultRPCTimeout bounds a single JSON-RPC call.
const DefaultRPCTimeout = 30 * time.Second

// NewRPCClient wraps an endpoint such as http://127.0.0.1:8899 or a devnet URL.
func NewRPCClient(endpoint string) *RPCClient {
	return &RPCClient{inner: rpc.NewWithTimeoutAndCommitment(endpoint, DefaultRPCTimeout, rpc.CommitmentConfirmed)}
}

func (c *RPCClient) LatestBlockhash(ctx context.Context) (solana.Hash, uint64, error) {
	res, err := c.inner.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return solana.Hash{}, 0, fmt.Errorf("get latest blockhash: %w", err)
	}
	if res == nil || res.Value == nil {
		return solana.Hash{}, 0, errors.New("get latest blockhash: empty response")
	}
	return res.Value.Blockhash, res.Value.LastValidBlockHeight, nil
}

func (c *RPCClient) AccountExists(ctx context.Context, account solana.PublicKey) (bool, error) {
	res, err := c.inner.GetAccountInfoWithOpts(ctx, account, &rpc.GetAccountInfoOpts{
		Commitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		if errors.Is(err, rpc.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("get account info: %w", err)
	}
	return res != nil && res.Value != nil, nil
}

func (c *RPCClient) Transaction(ctx context.Context, sig solana.Signature) (Fetched, error) {
	version := uint64(0)
	res, err := c.inner.GetTransaction(ctx, sig, &rpc.GetTransactionOpts{
		Encoding:                       solana.EncodingBase64,
		Commitment:                     rpc.CommitmentConfirmed,
		MaxSupportedTransactionVersion: &version,
	})
	if err != nil {
		if errors.Is(err, rpc.ErrNotFound) {
			return Fetched{}, ErrTransactionNotFound
		}
		return Fetched{}, fmt.Errorf("get transaction: %w", err)
	}
	if res == nil || res.Transaction == nil {
		return Fetched{}, ErrTransactionNotFound
	}
	tx, err := res.Transaction.GetTransaction()
	if err != nil {
		return Fetched{}, fmt.Errorf("decode transaction: %w", err)
	}
	fetched := Fetched{Transaction: tx, Slot: res.Slot}
	if res.Meta != nil && res.Meta.Err != nil {
		fetched.ExecErr = fmt.Errorf("transaction %s failed on chain: %v", sig, res.Meta.Err)
	}
	return fetched, nil
}
