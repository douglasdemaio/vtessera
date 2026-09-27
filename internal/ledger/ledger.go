package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	tesseraVersion = 1
	appendAttempts = 3
)

var (
	ErrNotSettled    = errors.New("trade is not in a completable state")
	ErrAlreadyIssued = errors.New("tessera already issued for trade")
)

type Store interface {
	AppendLedgerEntry(ctx context.Context, a domain.LedgerAppend, hashEntry domain.EntryHasher) (domain.LedgerEntry, error)
	HeadLedgerEntry(ctx context.Context) (domain.LedgerEntry, bool, error)
	ListLedgerEntries(ctx context.Context) ([]domain.LedgerEntry, error)
	SaveReceipt(ctx context.Context, r domain.Receipt) error
	GetReceiptByTrade(ctx context.Context, tradeID string) (domain.Receipt, error)
}

type Ledger struct {
	store  Store
	signer *Signer
	now    func() time.Time
}

func New(store Store, signer *Signer) *Ledger {
	return &Ledger{store: store, signer: signer, now: func() time.Time { return time.Now().UTC() }}
}

func (l *Ledger) VerificationKey() string {
	return l.signer.PublicKeyBase58()
}

type TradeRecord struct {
	TradeID     string                `json:"tradeId"`
	OfferID     string                `json:"offerId"`
	Buyer       string                `json:"buyer"`
	Seller      string                `json:"seller"`
	Description string                `json:"description"`
	Amount      string                `json:"amount"`
	Mint        string                `json:"mint"`
	Mode        domain.SettlementMode `json:"mode"`
	State       domain.TradeState     `json:"state"`
	RecordedAt  time.Time             `json:"recordedAt"`
}

type SettlementRecord struct {
	Mode            domain.SettlementMode `json:"mode"`
	Signature       string                `json:"signature,omitempty"`
	LedgerSequence  int64                 `json:"ledgerSequence"`
	LedgerEntryHash string                `json:"ledgerEntryHash"`
}

type TesseraClaims struct {
	jwt.RegisteredClaims
	Version    int              `json:"vt"`
	Trade      TradeRecord      `json:"trade"`
	Settlement SettlementRecord `json:"settlement"`
}

func tradeRecord(t domain.Trade, at time.Time) TradeRecord {
	return TradeRecord{
		TradeID:     t.ID,
		OfferID:     t.OfferID,
		Buyer:       t.BuyerAgentID,
		Seller:      t.SellerAgentID,
		Description: t.Description,
		Amount:      t.Amount.String(),
		Mint:        t.Mint,
		Mode:        t.SettlementMode,
		State:       t.State,
		RecordedAt:  at,
	}
}

func (l *Ledger) Record(ctx context.Context, t domain.Trade, solanaSignature string) (domain.Receipt, error) {
	if t.State != domain.TradeRecorded && t.State != domain.TradeSettled {
		return domain.Receipt{}, fmt.Errorf("%w: state %s", ErrNotSettled, t.State)
	}
	if _, err := l.store.GetReceiptByTrade(ctx, t.ID); err == nil {
		return domain.Receipt{}, fmt.Errorf("%w: trade %s", ErrAlreadyIssued, t.ID)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.Receipt{}, err
	}

	now := l.now().UTC()
	record := tradeRecord(t, now)
	payload, err := json.Marshal(record)
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("marshal ledger payload: %w", err)
	}

	entry, err := l.append(ctx, domain.LedgerAppend{
		TradeID:     t.ID,
		Payload:     payload,
		PayloadHash: PayloadHash(payload),
		CreatedAt:   now,
	})
	if err != nil {
		return domain.Receipt{}, err
	}

	claims := TesseraClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    "vtessera",
			Subject:   t.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
		},
		Version: tesseraVersion,
		Trade:   record,
		Settlement: SettlementRecord{
			Mode:            t.SettlementMode,
			Signature:       solanaSignature,
			LedgerSequence:  entry.Seq,
			LedgerEntryHash: entry.Hash,
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(l.signer.private)
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("sign tessera: %w", err)
	}

	receipt := domain.Receipt{ID: claims.ID, TradeID: t.ID, JWS: signed, IssuedAt: now}
	if err := l.store.SaveReceipt(ctx, receipt); err != nil {
		return domain.Receipt{}, err
	}
	return receipt, nil
}

func (l *Ledger) append(ctx context.Context, a domain.LedgerAppend) (domain.LedgerEntry, error) {
	var lastErr error
	for attempt := 0; attempt < appendAttempts; attempt++ {
		head, found, err := l.store.HeadLedgerEntry(ctx)
		if err != nil {
			return domain.LedgerEntry{}, err
		}
		a.PrevHash = domain.GenesisHash
		if found {
			a.PrevHash = head.Hash
		}
		entry, err := l.store.AppendLedgerEntry(ctx, a, EntryHash)
		if err == nil {
			return entry, nil
		}
		if !errors.Is(err, domain.ErrStale) {
			return domain.LedgerEntry{}, err
		}
		lastErr = err
	}
	return domain.LedgerEntry{}, fmt.Errorf("append ledger entry after %d attempts: %w", appendAttempts, lastErr)
}

func (l *Ledger) Receipt(ctx context.Context, tradeID string) (domain.Receipt, error) {
	return l.store.GetReceiptByTrade(ctx, tradeID)
}

func (l *Ledger) Entries(ctx context.Context) ([]domain.LedgerEntry, error) {
	return l.store.ListLedgerEntries(ctx)
}

func (l *Ledger) Verify(jws string) (*TesseraClaims, error) {
	publicKey := l.signer.PublicKey()
	claims := &TesseraClaims{}
	if _, err := jwt.ParseWithClaims(jws, claims, func(token *jwt.Token) (any, error) {
		return publicKey, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()})); err != nil {
		return nil, fmt.Errorf("verify tessera: %w", err)
	}
	switch {
	case claims.Trade.TradeID == "":
		return nil, errors.New("tessera is missing trade.tradeId")
	case claims.Trade.Buyer == "" || claims.Trade.Seller == "":
		return nil, errors.New("tessera is missing trade parties")
	case claims.Subject == "":
		return nil, errors.New("tessera is missing sub claim")
	case !claims.Trade.Mode.Valid():
		return nil, fmt.Errorf("tessera carries invalid settlement mode %q", claims.Trade.Mode)
	}
	return claims, nil
}
