package ledger

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// tesseraVersion is the claim schema version, not a settlement generation.
	//
	// It stays 1 across the addition of the cluster claim on purpose. A verifier
	// that keyed the presence of a cluster on this number would have to reject
	// every pre-Phase-3 receipt to stay consistent, and the design requires those
	// to remain valid: the signing key is the security boundary, and the absence
	// of the claim is what identifies a receipt as predating cluster awareness.
	// The claim's presence is the signal; a version bump would be redundant at
	// best and misleading at worst.
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

// VerificationKeys is every key a verifier may accept, the current one first. A
// receipt signed before a rotation verifies under a retired key, so a reader
// needs the whole set rather than just the key signing today.
func (l *Ledger) VerificationKeys() []string {
	return l.signer.VerificationKeyIDs()
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
	// Cluster names the chain the signature settles on. It is absent for an
	// off-chain tessera, which settles on no chain at all, and empty for a
	// pre-Phase-3 on-chain tessera, which was issued by a build with no cluster
	// awareness. An empty value on an on-chain tessera is therefore itself a
	// signal, and Verify rejects it rather than reading it as "any cluster".
	Cluster string `json:"cluster,omitempty"`
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

// Record issues a tessera for a settled trade. cluster names the chain the
// on-chain settlement happened on, and is ignored for an off-chain trade.
//
// The claim is inside the signed payload rather than beside it, so it cannot be
// edited after signing. A devnet tessera and a mainnet-beta tessera for the same
// trade carry the same signature field and the same ledger sequence, and the
// cluster is the only thing that tells them apart.
func (l *Ledger) Record(ctx context.Context, t domain.Trade, solanaSignature, clusterName string) (domain.Receipt, error) {
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
			Cluster:         clusterName,
		},
	}
	signed, err := l.signClaims(claims)
	if err != nil {
		return domain.Receipt{}, err
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

// signClaims signs a tessera and names the signing key in the JOSE header.
//
// The kid is what lets a receipt be verified after the key that made it has been
// retired: the signature alone does not say which key to use, and the current key
// is the wrong answer for every receipt issued before a rotation.
func (l *Ledger) signClaims(claims TesseraClaims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = l.signer.PublicKeyBase58()
	signed, err := token.SignedString(l.signer.CurrentPrivate())
	if err != nil {
		return "", fmt.Errorf("sign tessera: %w", err)
	}
	return signed, nil
}

// candidateKeys picks the keys a tessera may be checked against. A receipt that
// names its key is checked against that key alone; a receipt predating the kid
// header is tried against every trusted key, which are all this marketplace's.
func (l *Ledger) candidateKeys(jws string) ([]ed25519.PublicKey, error) {
	token, _, err := jwt.NewParser().ParseUnverified(jws, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("verify tessera: %w", err)
	}
	if kid, _ := token.Header["kid"].(string); kid != "" {
		key, ok := l.signer.PublicKeyForID(kid)
		if !ok {
			return nil, fmt.Errorf("verify tessera: no trusted key %q", kid)
		}
		return []ed25519.PublicKey{key}, nil
	}
	return l.signer.PublicKeys(), nil
}

func (l *Ledger) Verify(jws string) (*TesseraClaims, error) {
	keys, err := l.candidateKeys(jws)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, key := range keys {
		claims, err := verifyTesseraUnder(jws, key)
		if err != nil {
			lastErr = err
			continue
		}
		return claims, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no trusted key verified the tessera")
	}
	return nil, fmt.Errorf("verify tessera: %w", lastErr)
}

func verifyTesseraUnder(jws string, key ed25519.PublicKey) (*TesseraClaims, error) {
	claims := &TesseraClaims{}
	token, err := jwt.ParseWithClaims(jws, claims, func(*jwt.Token) (any, error) {
		return key, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}))
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("tessera signature is not valid")
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

// SignerKeyID is the key that signed a tessera, as named by its kid header. For a
// receipt issued before kid existed it is the trusted key the signature verifies
// under, and for one that verifies under none it falls back to the current key so
// a caller always has a string to advertise.
func (l *Ledger) SignerKeyID(jws string) string {
	if token, _, err := jwt.NewParser().ParseUnverified(jws, jwt.MapClaims{}); err == nil {
		if kid, _ := token.Header["kid"].(string); kid != "" {
			if _, ok := l.signer.PublicKeyForID(kid); ok {
				return kid
			}
		}
	}
	for _, key := range l.signer.PublicKeys() {
		if _, err := jwt.Parse(jws, func(*jwt.Token) (any, error) {
			return key, nil
		}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()})); err == nil {
			return l.signer.KeyID(key)
		}
	}
	return l.VerificationKey()
}

// A receipt issued before Phase 3 carries no cluster claim, and the design
// keeps it valid rather than invalidating it: the signature still proves the
// trade settled, and the signing key is the security boundary, not the claim.
// What the receipt cannot do is name a chain, so the absence is reported rather
// than silently read as "anywhere".
//
// TesseraClaims.Cluster returns "" for such a receipt. A verifier that needs to
// know the chain must refuse the ambiguity, not the tessera.
