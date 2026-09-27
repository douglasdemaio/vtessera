//go:build solana

// Package e2e drives the whole on-chain settlement path against a real
// solana-test-validator: the design spec §6 build flow over HTTP, the buyer's
// own verification of what it was handed, submission, and the §6.3 confirm flow
// reading the verdict back off the chain.
//
// These tests need a funded validator, so they sit behind the `solana` build tag
// and never run in the hermetic suite:
//
//	solana-test-validator --reset --quiet &
//	VTESSERA_TEST_RPC_URL=http://127.0.0.1:8899 go test -tags solana ./internal/e2e/
package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

const defaultRPCURL = "http://127.0.0.1:8899"

// maxTxVersion asks the node for version 0 transactions, which is what a legacy
// settlement transaction is.
var maxTxVersion uint64

// testRPCURL lets CI point at a validator on another port.
func testRPCURL() string {
	if url := os.Getenv("VTESSERA_TEST_RPC_URL"); url != "" {
		return url
	}
	return defaultRPCURL
}

func newKey(t *testing.T) solana.PrivateKey {
	t.Helper()
	out, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// bank returns a well-funded account used to open the accounts a test needs by
// transfer. One airdrop per test is the floor for a validator's faucet, and
// transfers from here are much faster than airdropping every keypair.
func bank(t *testing.T) solana.PrivateKey {
	t.Helper()
	key := newKey(t)
	fund(t, client(t), key)
	return key
}

func client(t *testing.T) *rpc.Client {
	t.Helper()
	c := rpc.NewWithCommitment(testRPCURL(), rpc.CommitmentFinalized)
	// A clear failure beats a wall of RPC timeouts when CI forgets the validator.
	if _, err := c.GetHealth(context.Background()); err != nil {
		t.Fatalf("no solana-test-validator at %s: %v\nstart one with: solana-test-validator --reset --quiet",
			testRPCURL(), err)
	}
	return c
}

func signer(keys ...solana.PrivateKey) func(solana.PublicKey) *solana.PrivateKey {
	return func(pub solana.PublicKey) *solana.PrivateKey {
		for i := range keys {
			if keys[i].PublicKey().Equals(pub) {
				return &keys[i]
			}
		}
		return nil
	}
}

func fund(t *testing.T, c *rpc.Client, key solana.PrivateKey) {
	t.Helper()
	sig, err := c.RequestAirdrop(context.Background(), key.PublicKey(), 20*solana.LAMPORTS_PER_SOL, rpc.CommitmentFinalized)
	if err != nil {
		t.Fatal(err)
	}
	confirm(t, c, sig)
}

// confirm blocks until the signature is finalized, and fails the test if the
// transaction errored on-chain. Submission alone proves nothing.
func confirm(t *testing.T, c *rpc.Client, sig solana.Signature) {
	t.Helper()
	for i := 0; i < 400; i++ {
		res, err := c.GetSignatureStatuses(context.Background(), false, sig)
		if err == nil && res != nil && len(res.Value) > 0 && res.Value[0] != nil {
			if res.Value[0].Err != nil {
				t.Fatalf("transaction %s failed on-chain: %v", sig, res.Value[0].Err)
			}
			if res.Value[0].ConfirmationStatus == rpc.ConfirmationStatusFinalized {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("transaction %s never reached finalized", sig)
}

func send(t *testing.T, c *rpc.Client, tx *solana.Transaction, keys ...solana.PrivateKey) solana.Signature {
	t.Helper()
	recent, err := c.GetLatestBlockhash(context.Background(), rpc.CommitmentFinalized)
	if err != nil {
		t.Fatal(err)
	}
	tx.Message.RecentBlockhash = recent.Value.Blockhash
	if _, err := tx.Sign(signer(keys...)); err != nil {
		t.Fatal(err)
	}
	sig, err := c.SendTransactionWithOpts(context.Background(), tx, rpc.TransactionOpts{
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentFinalized,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// accountExists reports whether an account is present on chain, using an explicit
// commitment rather than the RPC default.
func accountExists(t *testing.T, c *rpc.Client, pub solana.PublicKey) bool {
	t.Helper()
	for i := 0; i < 40; i++ {
		res, err := c.GetAccountInfoWithOpts(context.Background(), pub, &rpc.GetAccountInfoOpts{
			Commitment: rpc.CommitmentFinalized,
		})
		if err == nil && res != nil && res.Value != nil {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
