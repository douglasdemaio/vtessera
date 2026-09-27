package domain

import (
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/mr-tron/base58"
)

func validPublicKey() string {
	return base58.Encode(make([]byte, 32))
}

func TestSettlementModeValid(t *testing.T) {
	cases := map[SettlementMode]bool{
		SettlementOffchain: true,
		SettlementOnchain:  true,
		"":                 false,
		"onchainish":       false,
	}
	for mode, want := range cases {
		if got := mode.Valid(); got != want {
			t.Errorf("SettlementMode(%q).Valid() = %v, want %v", mode, got, want)
		}
	}
}

func TestAgentStatusValid(t *testing.T) {
	cases := map[AgentStatus]bool{
		AgentActive:    true,
		AgentSuspended: true,
		"":             false,
		"deleted":      false,
	}
	for status, want := range cases {
		if got := status.Valid(); got != want {
			t.Errorf("AgentStatus(%q).Valid() = %v, want %v", status, got, want)
		}
	}
}

func TestAgentCardValidateAcceptsWellFormedCard(t *testing.T) {
	card := AgentCard{
		Name:            "Trading Bot",
		PublicKey:       validPublicKey(),
		URL:             "https://example.com/agent",
		SettlementModes: []SettlementMode{SettlementOffchain},
		Currencies:      []string{validPublicKey()},
	}
	if err := card.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestAgentCardValidateRejectsInvalidFields(t *testing.T) {
	base := func() AgentCard {
		return AgentCard{Name: "Bot", PublicKey: validPublicKey(), URL: "https://example.com"}
	}

	t.Run("empty name", func(t *testing.T) {
		c := base()
		c.Name = "   "
		if err := c.Validate(); err == nil {
			t.Fatal("want error for empty name")
		}
	})

	t.Run("name too long", func(t *testing.T) {
		c := base()
		c.Name = string(make([]byte, 201))
		if err := c.Validate(); err == nil {
			t.Fatal("want error for overlong name")
		}
	})

	t.Run("description too long", func(t *testing.T) {
		c := base()
		c.Description = string(make([]byte, 2001))
		if err := c.Validate(); err == nil {
			t.Fatal("want error for overlong description")
		}
	})

	t.Run("bad public key", func(t *testing.T) {
		c := base()
		c.PublicKey = "not-base58!"
		if err := c.Validate(); err == nil {
			t.Fatal("want error for bad public key")
		}
	})

	t.Run("bad url", func(t *testing.T) {
		c := base()
		c.URL = "not a url"
		if err := c.Validate(); err == nil {
			t.Fatal("want error for bad url")
		}
	})

	t.Run("invalid settlement mode", func(t *testing.T) {
		c := base()
		c.SettlementModes = []SettlementMode{"sidechain"}
		if err := c.Validate(); err == nil {
			t.Fatal("want error for invalid settlement mode")
		}
	})

	t.Run("invalid currency", func(t *testing.T) {
		c := base()
		c.Currencies = []string{"not-a-mint"}
		if err := c.Validate(); err == nil {
			t.Fatal("want error for invalid currency")
		}
	})

	t.Run("too many capabilities", func(t *testing.T) {
		c := base()
		caps := make([]string, 65)
		c.Capabilities = caps
		if err := c.Validate(); err == nil {
			t.Fatal("want error for too many capabilities")
		}
	})
}

func TestValidateServiceURLRejectsNonHTTP(t *testing.T) {
	cases := []string{"", "ftp://example.com", "https://", "example.com"}
	for _, raw := range cases {
		if err := validateServiceURL(raw); err == nil {
			t.Errorf("validateServiceURL(%q) = nil, want error", raw)
		}
	}
}

func TestValidateServiceURLAcceptsHTTPAndHTTPS(t *testing.T) {
	for _, raw := range []string{"http://example.com", "https://example.com/path"} {
		if err := validateServiceURL(raw); err != nil {
			t.Errorf("validateServiceURL(%q) = %v, want nil", raw, err)
		}
	}
}

func TestParsePublicKeyRejectsMalformedInput(t *testing.T) {
	cases := []string{"", "not-base58!", base58.Encode(make([]byte, 31)), base58.Encode(make([]byte, 33))}
	for _, raw := range cases {
		if _, err := ParsePublicKey(raw); err == nil {
			t.Errorf("ParsePublicKey(%q) = nil, want error", raw)
		}
	}
}

func TestParsePublicKeyAcceptsThirtyTwoBytes(t *testing.T) {
	key := validPublicKey()
	got, err := ParsePublicKey(key)
	if err != nil {
		t.Fatalf("ParsePublicKey(%q) = %v, want nil", key, err)
	}
	if got != key {
		t.Errorf("ParsePublicKey(%q) = %q, want %q", key, got, key)
	}
}

func TestValidateMintRejectsMalformedInput(t *testing.T) {
	cases := []string{"", "not-base58!", base58.Encode(make([]byte, 31))}
	for _, raw := range cases {
		if err := ValidateMint(raw); err == nil {
			t.Errorf("ValidateMint(%q) = nil, want error", raw)
		}
	}
}

func TestValidateMintAcceptsThirtyTwoBytes(t *testing.T) {
	if err := ValidateMint(validPublicKey()); err != nil {
		t.Fatalf("ValidateMint() = %v, want nil", err)
	}
}

func TestOfferDirectionValid(t *testing.T) {
	cases := map[OfferDirection]bool{DirectionAsk: true, DirectionBid: true, "": false, "swap": false}
	for d, want := range cases {
		if got := d.Valid(); got != want {
			t.Errorf("OfferDirection(%q).Valid() = %v, want %v", d, got, want)
		}
	}
}

func TestOfferStatusValid(t *testing.T) {
	cases := map[OfferStatus]bool{OfferOpen: true, OfferClosed: true, "": false, "pending": false}
	for s, want := range cases {
		if got := s.Valid(); got != want {
			t.Errorf("OfferStatus(%q).Valid() = %v, want %v", s, got, want)
		}
	}
}

func validOffer() Offer {
	return Offer{
		Description:     "sample offer",
		Direction:       DirectionAsk,
		Status:          OfferOpen,
		PriceAmount:     money.MustParse("1"),
		PriceMint:       validPublicKey(),
		SettlementModes: []SettlementMode{SettlementOffchain},
	}
}

func TestOfferValidateAcceptsWellFormedOffer(t *testing.T) {
	if err := validOffer().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestOfferValidateRejectsInvalidFields(t *testing.T) {
	t.Run("empty description", func(t *testing.T) {
		o := validOffer()
		o.Description = "  "
		if err := o.Validate(); err == nil {
			t.Fatal("want error for empty description")
		}
	})

	t.Run("description too long", func(t *testing.T) {
		o := validOffer()
		o.Description = string(make([]byte, 4001))
		if err := o.Validate(); err == nil {
			t.Fatal("want error for overlong description")
		}
	})

	t.Run("invalid direction", func(t *testing.T) {
		o := validOffer()
		o.Direction = "swap"
		if err := o.Validate(); err == nil {
			t.Fatal("want error for invalid direction")
		}
	})

	t.Run("invalid status", func(t *testing.T) {
		o := validOffer()
		o.Status = "pending"
		if err := o.Validate(); err == nil {
			t.Fatal("want error for invalid status")
		}
	})

	t.Run("zero price", func(t *testing.T) {
		o := validOffer()
		o.PriceAmount = money.Amount{}
		if err := o.Validate(); err == nil {
			t.Fatal("want error for zero price")
		}
	})

	t.Run("invalid price mint", func(t *testing.T) {
		o := validOffer()
		o.PriceMint = "not-a-mint"
		if err := o.Validate(); err == nil {
			t.Fatal("want error for invalid price mint")
		}
	})

	t.Run("no settlement modes", func(t *testing.T) {
		o := validOffer()
		o.SettlementModes = nil
		if err := o.Validate(); err == nil {
			t.Fatal("want error for no settlement modes")
		}
	})

	t.Run("invalid settlement mode", func(t *testing.T) {
		o := validOffer()
		o.SettlementModes = []SettlementMode{"sidechain"}
		if err := o.Validate(); err == nil {
			t.Fatal("want error for invalid settlement mode")
		}
	})

	t.Run("too many capabilities", func(t *testing.T) {
		o := validOffer()
		o.Capabilities = make([]string, 65)
		if err := o.Validate(); err == nil {
			t.Fatal("want error for too many capabilities")
		}
	})
}

func TestOfferAcceptsMode(t *testing.T) {
	o := Offer{SettlementModes: []SettlementMode{SettlementOffchain}}
	if !o.AcceptsMode(SettlementOffchain) {
		t.Error("AcceptsMode(SettlementOffchain) = false, want true")
	}
	if o.AcceptsMode(SettlementOnchain) {
		t.Error("AcceptsMode(SettlementOnchain) = true, want false")
	}
}

func TestTradeStateValid(t *testing.T) {
	valid := []TradeState{
		TradeProposed, TradeNegotiating, TradeAccepted, TradeSettlementPending,
		TradeRecorded, TradeSettled, TradeDisputed, TradeCancelled,
	}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("TradeState(%q).Valid() = false, want true", s)
		}
	}
	if TradeState("unknown").Valid() {
		t.Error(`TradeState("unknown").Valid() = true, want false`)
	}
}

func TestTradeStateTerminal(t *testing.T) {
	terminal := []TradeState{TradeRecorded, TradeSettled, TradeDisputed, TradeCancelled}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("TradeState(%q).Terminal() = false, want true", s)
		}
	}
	nonTerminal := []TradeState{TradeProposed, TradeNegotiating, TradeAccepted, TradeSettlementPending}
	for _, s := range nonTerminal {
		if s.Terminal() {
			t.Errorf("TradeState(%q).Terminal() = true, want false", s)
		}
	}
}

func TestTradeParty(t *testing.T) {
	trade := Trade{BuyerAgentID: "buyer-1", SellerAgentID: "seller-1"}
	if !trade.Party("buyer-1") {
		t.Error("Party(buyer) = false, want true")
	}
	if !trade.Party("seller-1") {
		t.Error("Party(seller) = false, want true")
	}
	if trade.Party("stranger") {
		t.Error("Party(stranger) = true, want false")
	}
}

func TestSettlementStatusValid(t *testing.T) {
	valid := []SettlementStatus{SettlementIssued, SettlementConfirmed, SettlementExpired}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("SettlementStatus(%q).Valid() = false, want true", s)
		}
	}
	if SettlementStatus("unknown").Valid() {
		t.Error(`SettlementStatus("unknown").Valid() = true, want false`)
	}
}

func TestSettlementRequestLive(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	live := SettlementRequest{Status: SettlementIssued, ExpiresAt: now.Add(time.Minute)}
	if !live.Live(now) {
		t.Error("Live() = false, want true for issued and not yet expired")
	}

	expired := SettlementRequest{Status: SettlementIssued, ExpiresAt: now.Add(-time.Minute)}
	if expired.Live(now) {
		t.Error("Live() = true, want false for issued but expired")
	}

	confirmed := SettlementRequest{Status: SettlementConfirmed, ExpiresAt: now.Add(time.Minute)}
	if confirmed.Live(now) {
		t.Error("Live() = true, want false for non-issued status")
	}
}
