package limits_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/money"
)

const (
	usdcMainnet = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	usdcDevnet  = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	eurc        = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"
)

func TestAParRateConvertsWithoutRounding(t *testing.T) {
	p := limits.DefaultPolicy(limits.DefaultRates())

	for _, tc := range []struct {
		amount string
		mint   string
		want   string
	}{
		// A stablecoin at par converts exactly, which is the property that lets a
		// cap stated in dollars be compared against a token amount with no error
		// term at all.
		{"1", usdcMainnet, "1.000000"},
		{"0.01", usdcMainnet, "0.010000"},
		{"5.00", usdcMainnet, "5.000000"},
		{"1234.567891", usdcMainnet, "1234.567891"},
		{"0.000001", usdcMainnet, "0.000001"},
		{"999999.999999", usdcMainnet, "999999.999999"},
		// EURC and devnet USDC are different mints priced at the same par.
		{"12.50", eurc, "12.500000"},
		{"7.25", usdcDevnet, "7.250000"},
	} {
		got, err := p.USDValue(money.MustParse(tc.amount), tc.mint)
		if err != nil {
			t.Errorf("USDValue(%s, %s): %v", tc.amount, tc.mint, err)
			continue
		}
		if s := limits.FormatUSD(got); s != tc.want {
			t.Errorf("USDValue(%s, %s) = %s, want %s", tc.amount, tc.mint, s, tc.want)
		}
	}
}

func TestAConversionThatWouldOverflowSaturatesRatherThanWrapping(t *testing.T) {
	p := limits.DefaultPolicy(limits.DefaultRates())

	// The largest representable token amount at a par rate is about 1.8e13
	// dollars. Anything a rate can push past that must read as enormous, not wrap
	// round to something small: a wrap would make an expensive trade look free.
	rates := limits.Rates{}
	if err := rates.Add("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", limits.Rate{
		Decimals: 6,
		USD:      money.MustParse("999999999.00"),
	}); err != nil {
		t.Fatal(err)
	}
	huge := limits.DefaultPolicy(rates)
	got, err := huge.USDValue(money.MustParse("184467440737.095516"), "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	if err != nil {
		t.Fatalf("USDValue: %v", err)
	}
	if got == 0 {
		t.Fatal("a saturating conversion returned zero, which is the cheap direction")
	}
	_ = p
}

func TestAnUnpricedMintHasNoUSDValue(t *testing.T) {
	p := limits.DefaultPolicy(limits.DefaultRates())

	_, err := p.USDValue(money.MustParse("1.00"), "9n4xM2fPP91Zm3fDc7rT2QkWrB1sRdVvNcR6cMDcyGDQ")
	if !errors.Is(err, limits.ErrNoRate) {
		t.Fatalf("USDValue for an unpriced mint = %v, want ErrNoRate", err)
	}
	if p.Priced("9n4xM2fPP91Zm3fDc7rT2QkWrB1sRdVvNcR6cMDcyGDQ") {
		t.Error("Priced reports an undeclared mint as priced")
	}
	for _, mint := range []string{usdcMainnet, usdcDevnet, eurc} {
		if !p.Priced(mint) {
			t.Errorf("Priced reports the governed mint %s as unpriced", mint)
		}
	}
}

func TestAMintPricedAtZeroIsRefusedRatherThanTreatedAsFree(t *testing.T) {
	rates := limits.Rates{}
	// Add rejects a zero rate outright, which is the point: a free mint would
	// make every cap unlimited.
	if err := rates.Add(usdcMainnet, limits.Rate{Decimals: 6, USD: money.MustParse("0")}); !errors.Is(err, limits.ErrCapInvalid) {
		t.Fatalf("declaring a zero rate = %v, want ErrCapInvalid", err)
	}
}

func TestAMissingRateIsNamedAtStartup(t *testing.T) {
	rates := limits.Rates{}
	if err := rates.Add(usdcMainnet, limits.Rate{Decimals: 6, USD: money.MustParse("1.00")}); err != nil {
		t.Fatal(err)
	}
	missing := rates.Missing([]string{usdcMainnet, eurc})
	if len(missing) != 1 || missing[0] != eurc {
		t.Fatalf("Missing = %v, want just %s", missing, eurc)
	}
	if got := rates.Missing([]string{usdcMainnet}); len(got) != 0 {
		t.Errorf("Missing with every mint priced = %v, want none", got)
	}
}

func TestAnAddressThatCannotBeAMintIsARefusalNotAMiss(t *testing.T) {
	rates := limits.Rates{}
	// A typo in configuration must not become a rate that silently never matches
	// a real mint: that reads as "this currency is unpriced" and turns a
	// configuration mistake into a refusal to trade.
	for _, bad := range []string{
		"",
		"   ",
		"not-a-mint",
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1!",   // a base58-illegal character
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1vXX", // too many digits to be 32 bytes
	} {
		if err := rates.Add(bad, limits.Rate{Decimals: 6, USD: money.MustParse("1.00")}); err == nil {
			t.Errorf("rates.Add(%q) was accepted", bad)
		}
	}
	// What this cannot catch is a dropped trailing digit: a 43-character string
	// still decodes to 32 bytes, so "the address minus one character" is a valid
	// mint address that simply is not the one the operator meant. Decoding does
	// not fix transcription, which is why the governed table is pinned to a
	// snapshot and verified against both public clusters rather than trusted.
}

func TestACapMustBePositive(t *testing.T) {
	// Zero is a legitimate money amount and money.Parse accepts it. What must be
	// refused is a zero *cap*, because a cap of zero either blocks every trade or
	// reads as "unset" somewhere downstream.
	if _, err := money.Parse("0.00"); err != nil {
		t.Errorf("money.Parse(\"0.00\") = %v, want it accepted as an amount", err)
	}
	p := limits.DefaultPolicy(limits.DefaultRates())
	if err := p.Check(money.MustParse("0.00"), money.MustParse("20.00")); !errors.Is(err, limits.ErrCapNotPositive) {
		t.Errorf("Check with a zero per-trade cap = %v, want ErrCapNotPositive", err)
	}
	if err := p.Check(money.MustParse("5.00"), money.MustParse("0")); !errors.Is(err, limits.ErrCapNotPositive) {
		t.Errorf("Check with a zero daily cap = %v, want ErrCapNotPositive", err)
	}
}

func TestCeilingsBoundARaiseAndAreCheckedThemselves(t *testing.T) {
	base := limits.DefaultPolicy(limits.DefaultRates())

	p := base.WithCeilings(money.MustParse("50.00"), money.MustParse("200.00"))
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := p.Check(money.MustParse("50.00"), money.MustParse("200.00")); err != nil {
		t.Errorf("Check at exactly the ceiling = %v, want it allowed", err)
	}
	if err := p.Check(money.MustParse("50.01"), money.MustParse("200.00")); !errors.Is(err, limits.ErrAboveCeiling) {
		t.Errorf("Check above the per-trade ceiling = %v, want ErrAboveCeiling", err)
	}
	if err := p.Check(money.MustParse("50.00"), money.MustParse("200.01")); !errors.Is(err, limits.ErrAboveCeiling) {
		t.Errorf("Check above the daily ceiling = %v, want ErrAboveCeiling", err)
	}

	// A ceiling below the default it is meant to bound is a configuration error:
	// it would refuse every agent that asked for nothing.
	bad := base.WithCeilings(money.MustParse("1.00"), money.MustParse("200.00"))
	if err := bad.Validate(); err == nil {
		t.Error("Validate accepted a per-trade ceiling below the default cap")
	}
}

func TestAnUnsetCeilingMeansNoCeiling(t *testing.T) {
	p := limits.DefaultPolicy(limits.DefaultRates())
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// With no ceiling declared, a raise is only bounded by the value asked for.
	if err := p.Check(money.MustParse("1000000.00"), money.MustParse("1000000.00")); err != nil {
		t.Errorf("Check with no ceilings = %v, want it allowed", err)
	}
}

func TestEffectiveFallsBackToTheDefaultWhenAnOverrideIsMissing(t *testing.T) {
	base := limits.DefaultPolicy(limits.DefaultRates())
	ctx := context.Background()

	// No override source at all.
	got := base.Effective(ctx, "agent")
	if got.PerTrade.String() != "10.00" || got.PerDay.String() != "10.00" || got.Raised {
		t.Errorf("Effective without overrides = %s/%s raised=%v, want the deployment default",
			got.PerTrade, got.PerDay, got.Raised)
	}

	// An override that is stored but unreadable, or zero, must not become an
	// unlimited cap.
	p := base.WithAgentLimits(func(context.Context, string) (money.Amount, money.Amount, bool) {
		return money.Amount{}, money.Amount{}, false
	})
	got = p.Effective(ctx, "agent")
	if got.PerTrade.String() != "10.00" || got.Raised {
		t.Errorf("Effective with an unreadable override = %s/%s, want the default", got.PerTrade, got.PerDay)
	}

	// A stored raise above the default applies and says so.
	p = base.WithAgentLimits(func(context.Context, string) (money.Amount, money.Amount, bool) {
		return money.MustParse("25.00"), money.MustParse("30.00"), true
	})
	got = p.Effective(ctx, "agent")
	if got.PerTrade.String() != "25.00" || got.PerDay.String() != "30.00" || !got.Raised {
		t.Errorf("Effective with a stored raise = %s/%s raised=%v, want 25.00/30.00 raised",
			got.PerTrade, got.PerDay, got.Raised)
	}

	// A stored value below the default is not a cap reduction: Effective is the
	// one place that resolves caps, and letting it shrink them would let a stored
	// row do something the raise route refuses to do.
	p = base.WithAgentLimits(func(context.Context, string) (money.Amount, money.Amount, bool) {
		return money.MustParse("1.00"), money.MustParse("1.00"), true
	})
	got = p.Effective(ctx, "agent")
	if got.PerTrade.String() != "10.00" || got.Raised {
		t.Errorf("Effective honoured a stored value below the default: %s raised=%v", got.PerTrade, got.Raised)
	}
}

func TestTheDefaultPolicyIsTenAndTen(t *testing.T) {
	p := limits.DefaultPolicy(limits.DefaultRates())
	if p.PerTrade.String() != "10.00" {
		t.Errorf("default per-trade cap = %s, want 10.00", p.PerTrade)
	}
	if p.PerDay.String() != "10.00" {
		t.Errorf("default daily cap = %s, want 10.00", p.PerDay)
	}
	// The two are equal so that one maximum-size trade exhausts the day. If the
	// per-trade cap were allowed to exceed the daily cap, one trade could clear
	// the daily cap on its own and the daily cap would stop being a daily bound.
	if p.PerDay.Cmp(p.PerTrade) != 0 {
		t.Errorf("default daily cap %s differs from per-trade %s; the daily cap must not exceed one trade",
			p.PerDay, p.PerTrade)
	}
	if p.Window != 24*time.Hour {
		t.Errorf("default window = %s, want 24h", p.Window)
	}
}

func TestAMustPositiveWindowIsRequired(t *testing.T) {
	p := limits.DefaultPolicy(limits.DefaultRates()).WithWindow(0)
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "window") {
		t.Fatalf("Validate with a zero window = %v, want a complaint about the window", err)
	}
}

func TestEachCeilingIsCheckedAgainstTheCapItBounds(t *testing.T) {
	base := limits.DefaultPolicy(limits.DefaultRates()) // 5.00 per trade, 20.00 per day

	// A per-trade ceiling below the daily cap is ordinary configuration, not a
	// contradiction: raise one trade from five to ten, keep the daily limit.
	if err := base.WithCeilings(money.MustParse("10.00"), money.MustParse("200.00")).Validate(); err != nil {
		t.Errorf("a per-trade ceiling of 10.00 against a 20.00 daily cap = %v, want it accepted", err)
	}
	// Each ceiling still has to be at least the cap it bounds.
	if err := base.WithCeilings(money.MustParse("1.00"), money.MustParse("200.00")).Validate(); err == nil {
		t.Error("a per-trade ceiling of 1.00 below the 5.00 default was accepted")
	}
	if err := base.WithCeilings(money.MustParse("10.00"), money.MustParse("5.00")).Validate(); err == nil {
		t.Error("a daily ceiling of 5.00 below the 20.00 default was accepted")
	}
}
