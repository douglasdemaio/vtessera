package settlement

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/gagliardetto/solana-go"
)

// DefaultBlockhashTTL is the advisory window during which a blockhash stays
// usable. Solana blockhashes are valid for roughly 60-90 seconds; the chain is
// authoritative, and this is the local clock the service uses to decide whether
// a live settlement request still blocks cancellation.
const DefaultBlockhashTTL = 90 * time.Second

// Request is the persisted settlement request from design spec §4: one active
// unsigned transaction per trade.
type Request struct {
	TradeID    string    `json:"tradeId"`
	Blockhash  string    `json:"blockhash"`
	LastValid  uint64    `json:"lastValidBlockHeight"`
	UnsignedTx string    `json:"unsignedTx"`
	SellerATA  string    `json:"sellerAta"`
	BuyerATA   string    `json:"buyerAta"`
	CreatedATA bool      `json:"createdAta"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Signature  string    `json:"signature,omitempty"`
	Expired    bool      `json:"expired"`
}

// Live reports whether the request still holds a usable blockhash. A live
// request blocks cancellation of the trade.
func (r Request) Live(now time.Time) bool {
	return r.Blockhash != "" && !r.Expired && now.Before(r.ExpiresAt)
}

// Build is the result of assembling a settlement transaction.
type Build struct {
	Request  Request
	Terms    Terms
	Unsigned *solana.Transaction
}

// Builder assembles unsigned settlement transactions.
type Builder struct {
	client Client
	ttl    time.Duration
	now    func() time.Time
}

// NewBuilder returns a builder that fetches blockhashes and account state from
// the given client.
func NewBuilder(client Client) *Builder {
	return &Builder{client: client, ttl: DefaultBlockhashTTL, now: func() time.Time { return time.Now().UTC() }}
}

// SetTTL overrides the advisory blockhash window, for tests and for clusters
// with unusual block times.
func (b *Builder) SetTTL(ttl time.Duration) *Builder {
	b.ttl = ttl
	return b
}

// SetClock overrides the clock, for tests.
func (b *Builder) SetClock(now func() time.Time) *Builder {
	b.now = now
	return b
}

// Build assembles the unsigned settlement transaction for the terms. The
// result carries no signatures: the buyer is the sole signer and fee payer.
func (b *Builder) Build(ctx context.Context, t Terms) (Build, error) {
	sellerATA, err := t.SellerATA()
	if err != nil {
		return Build{}, fmt.Errorf("derive seller ata: %w", err)
	}
	buyerATA, err := t.BuyerATA()
	if err != nil {
		return Build{}, fmt.Errorf("derive buyer ata: %w", err)
	}
	// The seller's account only needs creating when it is absent; reusing an
	// existing ATA keeps the transaction to the three required instructions.
	sellerATAExists, err := b.client.AccountExists(ctx, sellerATA)
	if err != nil {
		return Build{}, fmt.Errorf("read seller ata: %w", err)
	}
	instructions, err := expectedInstructions(t, sellerATAExists)
	if err != nil {
		return Build{}, err
	}
	blockhash, lastValid, err := b.client.LatestBlockhash(ctx)
	if err != nil {
		return Build{}, err
	}
	built, err := assemble(instructions, blockhash, t.Buyer)
	if err != nil {
		return Build{}, err
	}
	raw, err := built.MarshalBinary()
	if err != nil {
		return Build{}, fmt.Errorf("serialize unsigned transaction: %w", err)
	}
	now := b.now().UTC()
	return Build{
		Request: Request{
			TradeID:    t.TradeID,
			Blockhash:  blockhash.String(),
			LastValid:  lastValid,
			UnsignedTx: base64.StdEncoding.EncodeToString(raw),
			SellerATA:  sellerATA.String(),
			BuyerATA:   buyerATA.String(),
			CreatedATA: !sellerATAExists,
			CreatedAt:  now,
			ExpiresAt:  now.Add(b.ttl),
		},
		Terms:    t,
		Unsigned: built,
	}, nil
}

// assemble compiles settlement instructions into an unsigned transaction with
// the buyer as fee payer.
func assemble(instructions []solana.Instruction, blockhash solana.Hash, payer solana.PublicKey) (*solana.Transaction, error) {
	tx, err := solana.NewTransaction(instructions, blockhash, solana.TransactionPayer(payer))
	if err != nil {
		return nil, fmt.Errorf("assemble transaction: %w", err)
	}
	return tx, nil
}
