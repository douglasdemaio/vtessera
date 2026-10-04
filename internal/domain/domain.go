package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/mr-tron/base58"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrStale    = errors.New("stale state")
	// ErrInvalid marks a caller's own malformed input, as opposed to a server
	// or store fault. Callers that return it get a 400, not a 500: an agent
	// that sent a bad field should retry with a correction, never treat the
	// marketplace as broken.
	ErrInvalid = errors.New("invalid")
)

type SettlementMode string

const (
	SettlementOffchain SettlementMode = "offchain"
	SettlementOnchain  SettlementMode = "onchain"
)

func (m SettlementMode) Valid() bool {
	return m == SettlementOffchain || m == SettlementOnchain
}

type AgentStatus string

const (
	AgentActive    AgentStatus = "active"
	AgentSuspended AgentStatus = "suspended"
)

func (s AgentStatus) Valid() bool {
	return s == AgentActive || s == AgentSuspended
}

type Agent struct {
	ID        string      `json:"id"`
	Card      AgentCard   `json:"card"`
	Status    AgentStatus `json:"status"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

type AgentCard struct {
	Name            string           `json:"name"`
	Description     string           `json:"description,omitempty"`
	Version         string           `json:"version,omitempty"`
	URL             string           `json:"url,omitempty"`
	PublicKey       string           `json:"publicKey"`
	Capabilities    []string         `json:"capabilities,omitempty"`
	Skills          []AgentSkill     `json:"skills,omitempty"`
	Currencies      []string         `json:"currencies,omitempty"`
	SettlementModes []SettlementMode `json:"settlementModes,omitempty"`
}

type AgentSkill struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Tags   []string `json:"tags,omitempty"`
	Input  []string `json:"inputModes,omitempty"`
	Output []string `json:"outputModes,omitempty"`
}

// Validate wraps every field failure in ErrInvalid so the HTTP layer can tell a
// bad request from a broken service. The specific message is still the first
// thing the caller reads; the sentinel only decides the status code.
func (c AgentCard) Validate() error {
	if err := c.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

func (c AgentCard) validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("agent card name is required")
	}
	if len(c.Name) > 200 {
		return fmt.Errorf("agent card name exceeds 200 characters")
	}
	if len(c.Description) > 2000 {
		return fmt.Errorf("agent card description exceeds 2000 characters")
	}
	if _, err := ParsePublicKey(c.PublicKey); err != nil {
		return fmt.Errorf("agent card publicKey: %w", err)
	}
	if err := validateServiceURL(c.URL); err != nil {
		return fmt.Errorf("agent card url: %w", err)
	}
	for _, m := range c.SettlementModes {
		if !m.Valid() {
			return fmt.Errorf("settlement mode %q is not valid", m)
		}
	}
	for _, cur := range c.Currencies {
		if err := ValidateMint(cur); err != nil {
			return fmt.Errorf("currency: %w", err)
		}
	}
	if len(c.Capabilities) > 64 {
		return fmt.Errorf("agent card declares more than 64 capabilities")
	}
	return nil
}

func validateServiceURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("is not a valid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("must be an absolute http or https URL, got %q", raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("must include a host, got %q", raw)
	}
	return nil
}

func ParsePublicKey(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("public key is required")
	}
	raw, err := base58.Decode(s)
	if err != nil {
		return "", fmt.Errorf("public key is not valid base58: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("public key must decode to 32 bytes, got %d", len(raw))
	}
	return s, nil
}

func ValidateMint(address string) error {
	if address == "" {
		return fmt.Errorf("mint address is required")
	}
	raw, err := base58.Decode(address)
	if err != nil {
		return fmt.Errorf("mint address is not valid base58: %w", err)
	}
	if len(raw) != 32 {
		return fmt.Errorf("mint address must decode to 32 bytes, got %d", len(raw))
	}
	return nil
}

// MintInfo describes a settlement mint. Identity is the address alone: a symbol
// is decoration, never an identity. The governed list of mints lives in
// internal/tokens, which both settlement and discovery read.
type MintInfo struct {
	Address  string `json:"address"`
	Symbol   string `json:"symbol,omitempty"`
	Decimals int    `json:"decimals"`
	Known    bool   `json:"known"`
}

type OfferQuery struct {
	AgentID      string
	Direction    OfferDirection
	Status       OfferStatus
	Capability   string
	Text         string
	Mint         string
	Mode         SettlementMode
	ExcludeAgent string
	Limit        int
	Offset       int
}

type OfferDirection string

const (
	DirectionAsk OfferDirection = "ask"
	DirectionBid OfferDirection = "bid"
)

func (d OfferDirection) Valid() bool {
	return d == DirectionAsk || d == DirectionBid
}

type OfferStatus string

const (
	OfferOpen   OfferStatus = "open"
	OfferClosed OfferStatus = "closed"
)

func (s OfferStatus) Valid() bool {
	return s == OfferOpen || s == OfferClosed
}

type Offer struct {
	ID              string           `json:"id"`
	AgentID         string           `json:"agentId"`
	Direction       OfferDirection   `json:"direction"`
	Description     string           `json:"description"`
	Capabilities    []string         `json:"capabilities,omitempty"`
	PriceAmount     money.Amount     `json:"priceAmount"`
	PriceMint       string           `json:"priceMint"`
	SettlementModes []SettlementMode `json:"settlementModes"`
	Status          OfferStatus      `json:"status"`
	CreatedAt       time.Time        `json:"createdAt"`
	UpdatedAt       time.Time        `json:"updatedAt"`
}

// Validate wraps every field failure in ErrInvalid, for the same reason as
// AgentCard.Validate.
func (o Offer) Validate() error {
	if err := o.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

func (o Offer) validate() error {
	if strings.TrimSpace(o.Description) == "" {
		return fmt.Errorf("offer description is required")
	}
	if len(o.Description) > 4000 {
		return fmt.Errorf("offer description exceeds 4000 characters")
	}
	if !o.Direction.Valid() {
		return fmt.Errorf("offer direction %q is not valid", o.Direction)
	}
	if !o.Status.Valid() {
		return fmt.Errorf("offer status %q is not valid", o.Status)
	}
	if o.PriceAmount.IsZero() {
		return fmt.Errorf("offer price amount must be greater than zero")
	}
	if err := ValidateMint(o.PriceMint); err != nil {
		return fmt.Errorf("offer priceMint: %w", err)
	}
	if len(o.SettlementModes) == 0 {
		return fmt.Errorf("offer must accept at least one settlement mode")
	}
	for _, m := range o.SettlementModes {
		if !m.Valid() {
			return fmt.Errorf("settlement mode %q is not valid", m)
		}
	}
	if len(o.Capabilities) > 64 {
		return fmt.Errorf("offer declares more than 64 capabilities")
	}
	return nil
}

func (o Offer) AcceptsMode(m SettlementMode) bool {
	for _, candidate := range o.SettlementModes {
		if candidate == m {
			return true
		}
	}
	return false
}

type TradeState string

const (
	TradeProposed          TradeState = "proposed"
	TradeNegotiating       TradeState = "negotiating"
	TradeAccepted          TradeState = "accepted"
	TradeSettlementPending TradeState = "settlement_pending"
	TradeRecorded          TradeState = "recorded"
	TradeSettled           TradeState = "settled"
	TradeDisputed          TradeState = "disputed"
	TradeCancelled         TradeState = "cancelled"
)

func (s TradeState) Valid() bool {
	switch s {
	case TradeProposed, TradeNegotiating, TradeAccepted, TradeSettlementPending,
		TradeRecorded, TradeSettled, TradeDisputed, TradeCancelled:
		return true
	}
	return false
}

func (s TradeState) Terminal() bool {
	switch s {
	case TradeRecorded, TradeSettled, TradeDisputed, TradeCancelled:
		return true
	}
	return false
}

type Trade struct {
	ID             string         `json:"id"`
	OfferID        string         `json:"offerId"`
	BuyerAgentID   string         `json:"buyerAgentId"`
	SellerAgentID  string         `json:"sellerAgentId"`
	Description    string         `json:"description"`
	Amount         money.Amount   `json:"amount"`
	Mint           string         `json:"mint"`
	SettlementMode SettlementMode `json:"settlementMode"`
	State          TradeState     `json:"state"`
	IdempotencyKey string         `json:"-"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
	Acceptances    []string       `json:"acceptances"`
	Events         []TradeEvent   `json:"events,omitempty"`
}

func (t Trade) Party(agentID string) bool {
	return agentID == t.BuyerAgentID || agentID == t.SellerAgentID
}

type TradeEventType string

const (
	EventProposed          TradeEventType = "proposed"
	EventNegotiating       TradeEventType = "negotiating"
	EventAccepted          TradeEventType = "accepted"
	EventSettlementPending TradeEventType = "settlement_pending"
	EventRecorded          TradeEventType = "recorded"
	EventSettled           TradeEventType = "settled"
	EventCancelled         TradeEventType = "cancelled"
	EventDisputed          TradeEventType = "disputed"
)

// SettlementStatus is the lifecycle of a single issued settlement request. A
// trade has at most one issued request at a time; a replacement supersedes it.
type SettlementStatus string

const (
	SettlementIssued    SettlementStatus = "issued"
	SettlementConfirmed SettlementStatus = "confirmed"
	SettlementExpired   SettlementStatus = "expired"
)

func (s SettlementStatus) Valid() bool {
	switch s {
	case SettlementIssued, SettlementConfirmed, SettlementExpired:
		return true
	}
	return false
}

// PrePhase3Cluster is the value settlement_requests.cluster carries for rows
// written before cluster-awareness. It is deliberately not a declarable
// Cluster: a request whose terms were compiled with a flat registry cannot be
// known to be valid for any particular chain, so it can never be confirmed.
const PrePhase3Cluster = "pre-phase-3"

// SettlementRequest is one unsigned transaction issued to the buyer, persisted
// so a confirm can be matched against exactly what was offered and so an
// operator can audit a dispute. The service holds no keys: the transaction is
// issued unsigned and only the buyer can sign it.
type SettlementRequest struct {
	ID          string           `json:"id"`
	TradeID     string           `json:"tradeId"`
	UnsignedTx  string           `json:"unsignedTx"`
	Blockhash   string           `json:"blockhash"`
	LastValid   uint64           `json:"lastValidBlockHeight"`
	BuyerATA    string           `json:"buyerAta"`
	SellerATA   string           `json:"sellerAta"`
	CreatedATA  bool             `json:"createdAta"`
	FeeLamports uint64           `json:"feeLamports"`
	FeeWallet   string           `json:"feeWallet"`
	Status      SettlementStatus `json:"status"`
	Signature   string           `json:"signature,omitempty"`
	// Cluster is the chain whose mint set compiled the frozen terms, so a
	// request issued on one chain is never confirmed on another. It is empty
	// for pre-Phase-3 requests, which is what keeps them unconfirmable.
	Cluster   cluster.Cluster `json:"cluster,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
	ExpiresAt time.Time       `json:"expiresAt"`
}

// Live reports whether the request still holds a usable blockhash. A live
// request blocks cancellation of the trade, per design spec §5.
func (r SettlementRequest) Live(now time.Time) bool {
	return r.Status == SettlementIssued && now.UTC().Before(r.ExpiresAt)
}

type TradeEvent struct {
	Seq          int64           `json:"seq"`
	TradeID      string          `json:"tradeId"`
	ActorAgentID string          `json:"actorAgentId"`
	Type         TradeEventType  `json:"type"`
	FromState    TradeState      `json:"fromState"`
	ToState      TradeState      `json:"toState"`
	Detail       json.RawMessage `json:"detail,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
}

const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

type LedgerAppend struct {
	TradeID     string
	Payload     []byte
	PayloadHash string
	PrevHash    string
	CreatedAt   time.Time
}

type EntryHasher func(seq int64, prevHash, payloadHash string) string

type LedgerEntry struct {
	Seq         int64           `json:"seq"`
	TradeID     string          `json:"tradeId"`
	Payload     json.RawMessage `json:"payload"`
	PayloadHash string          `json:"payloadHash"`
	PrevHash    string          `json:"prevHash"`
	Hash        string          `json:"hash"`
	CreatedAt   time.Time       `json:"createdAt"`
}

type Receipt struct {
	ID       string    `json:"id"`
	TradeID  string    `json:"tradeId"`
	JWS      string    `json:"jws"`
	IssuedAt time.Time `json:"issuedAt"`
}

type Challenge struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agentId"`
	Nonce     string     `json:"nonce"`
	ExpiresAt time.Time  `json:"expiresAt"`
	Consumed  *time.Time `json:"consumedAt,omitempty"`
}

type UsageTotals struct {
	Delivered int `json:"delivered"`
	Disputed  int `json:"disputed"`
	Cancelled int `json:"cancelled"`
	Consumers int `json:"consumers"`
	Services  int `json:"services"`
}

type AgentUsage struct {
	AgentID   string `json:"agentId"`
	Delivered int    `json:"delivered"`
	Disputed  int    `json:"disputed"`
	Cancelled int    `json:"cancelled"`
}

type UsageMetrics struct {
	GeneratedAt time.Time    `json:"generatedAt"`
	AsOf        *time.Time   `json:"asOf"`
	Totals      UsageTotals  `json:"totals"`
	Agents      []AgentUsage `json:"agents"`
}

// AgentLimits is an agent's own opt-in to spending caps above the deployment
// default. Amounts are USD decimal strings, not token amounts: the caps are
// denominated in dollars and the trades they bound are not.
type AgentLimits struct {
	AgentID     string       `json:"agentId"`
	PerTradeUSD money.Amount `json:"perTradeUsd"`
	PerDayUSD   money.Amount `json:"perDayUsd"`
	RaisedAt    time.Time    `json:"raisedAt"`
}

// SpendRow is one committed trade's amount, kept in token terms rather than USD
// so that a change of price does not require rewriting history: the cap is
// measured against the rates in force when it is read.
type SpendRow struct {
	Mint   string       `json:"mint"`
	Amount money.Amount `json:"amount"`
}
