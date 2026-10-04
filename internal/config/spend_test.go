package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/tokens"
)

const devnetRPC = "https://api.devnet.solana.com"

func TestSpendCapsDefaultToFiveAndTwenty(t *testing.T) {
	cfg, err := Parse([]string{"--session-secret", goodSecret})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Spend.PerTradeUSD.String() != "5.00" {
		t.Errorf("per-trade cap = %s, want 5.00", cfg.Spend.PerTradeUSD)
	}
	if cfg.Spend.PerDayUSD.String() != "20.00" {
		t.Errorf("daily cap = %s, want 20.00", cfg.Spend.PerDayUSD)
	}
	if cfg.Spend.DailyWindow != 24*time.Hour {
		t.Errorf("window = %s, want 24h", cfg.Spend.DailyWindow)
	}
	if cfg.Sandbox {
		t.Error("sandbox is on by default")
	}
}

func TestSpendCapsAreConfigurable(t *testing.T) {
	cfg, err := Parse([]string{
		"--session-secret", goodSecret,
		"--spend-cap-per-trade", "2.50",
		"--spend-cap-per-day", "7",
		"--spend-cap-window", "1h",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Spend.PerTradeUSD.String() != "2.50" || cfg.Spend.PerDayUSD.String() != "7" {
		t.Errorf("caps = %s per trade, %s per day", cfg.Spend.PerTradeUSD, cfg.Spend.PerDayUSD)
	}
	if cfg.Spend.DailyWindow != time.Hour {
		t.Errorf("window = %s, want 1h", cfg.Spend.DailyWindow)
	}
}

func TestAnUnsetCeilingMeansRaisingIsNotAllowed(t *testing.T) {
	cfg, err := Parse([]string{"--session-secret", goodSecret})
	if err != nil {
		t.Fatal(err)
	}
	policy := cfg.Spend.Policy()
	// No ceiling is the same as no permission to raise. An operator who has not
	// thought about it must not have silently allowed every agent to set its own
	// limit.
	if err := policy.Check(cfg.Spend.PerTradeUSD, cfg.Spend.PerDayUSD); err != nil {
		t.Fatalf("raising to the default with no ceiling declared = %v, want it allowed", err)
	}
	raised := policy.WithCeilings(cfg.Spend.PerTradeUSD, cfg.Spend.PerDayUSD)
	if err := raised.Check(mustAmount("1000.00"), mustAmount("1000.00")); !errors.Is(err, limits.ErrAboveCeiling) {
		t.Errorf("raising above a ceiling declared at the default = %v, want ErrAboveCeiling", err)
	}
}

func TestACapThatIsNotADecimalIsAStartupRefusal(t *testing.T) {
	for _, bad := range []struct{ flag, value string }{
		{"--spend-cap-per-trade", "five dollars"},
		{"--spend-cap-per-trade", "-1.00"},
		{"--spend-cap-per-trade", "0"},
		{"--spend-cap-per-day", "abc"},
		{"--spend-cap-per-day", "0.00"},
		{"--spend-cap-max-per-trade", "not-a-number"},
		{"--spend-cap-window", "0s"},
		{"--spend-cap-window", "a day"},
	} {
		_, err := Parse([]string{"--session-secret", goodSecret, bad.flag, bad.value})
		if err == nil {
			t.Errorf("Parse(%s %s) was accepted", bad.flag, bad.value)
		}
	}
}

func TestAMalformedRateIsAStartupRefusal(t *testing.T) {
	for _, bad := range []string{
		"not-a-pair",
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v=1.00",     // no decimals
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v=1.00@",    // no decimals value
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v=1.00@six", // decimals not a number
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v=0@6",      // a free mint
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v=1.00@10",  // more decimals than supported
		"notamintaddressatallnolongenough000000=1.00@6",
	} {
		if _, err := Parse([]string{"--session-secret", goodSecret, "--spend-rates", bad}); err == nil {
			t.Errorf("Parse(--spend-rates %q) was accepted", bad)
		}
	}
}

func TestARateOverridesTheBuiltInParRate(t *testing.T) {
	cfg, err := Parse([]string{
		"--session-secret", goodSecret,
		"--spend-rates", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v=0.98@6",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Spend.Policy().USDValue(mustAmount("100.00"), "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	if err != nil {
		t.Fatal(err)
	}
	if s := limits.FormatUSD(got); s != "98.000000" {
		t.Errorf("100 tokens at a declared 0.98 = %s, want 98.000000", s)
	}
}

func TestSandboxIsOffUnlessAskedFor(t *testing.T) {
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sandbox {
		t.Error("--sandbox did not set the flag")
	}
}

func TestSandboxRefusesToStartWithAnOnChainEndpoint(t *testing.T) {
	// Sandbox means no real value moves. An operator who also configured an
	// endpoint has asked for two things that cannot both be true, and the failure
	// must land at boot with a reason rather than as a 501 an agent discovers.
	_, err := Parse([]string{
		"--session-secret", goodSecret,
		"--sandbox",
		"--rpc-url", devnetRPC,
		"--cluster", "devnet",
	})
	if err == nil {
		t.Fatal("a sandbox deployment configured with an RPC endpoint started anyway")
	}
	if !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("error %q does not say sandbox is the reason", err)
	}
}

func TestSandboxOnItsOwnIsAValidConfiguration(t *testing.T) {
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--sandbox"})
	if err != nil {
		t.Fatalf("sandbox without settlement: %v", err)
	}
	if !cfg.Sandbox {
		t.Error("sandbox flag lost")
	}
	if cfg.SettlementEnabled() {
		t.Error("sandbox mode reports settlement as enabled")
	}
}

func TestEveryGovernedMintIsPricedByDefault(t *testing.T) {
	// The built-in table has to cover both clusters' governed stablecoins, because
	// a governed mint with no rate makes the caps unmeasurable in that currency and
	// the service refuses to start rather than trading it uncapped.
	// devnet first: t.Setenv lasts for the whole test, and the acknowledgement is
	// only meaningful on mainnet-beta, so setting it before the devnet case would
	// fail that case instead.
	for _, name := range []cluster.Cluster{cluster.Devnet, cluster.MainnetBeta} {
		if name == cluster.MainnetBeta {
			t.Setenv("VTESSERA_MAINNET_ACK", "1")
		}
		cfg, err := Parse([]string{
			"--session-secret", goodSecret,
			"--rpc-url", devnetRPC,
			"--cluster", string(name),
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mints, err := tokens.ForCluster(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		entries := mints.List()
		addrs := make([]string, 0, len(entries))
		for _, m := range entries {
			addrs = append(addrs, m.Address)
		}
		if missing := cfg.Spend.Policy().Rates().Missing(addrs); len(missing) > 0 {
			t.Errorf("%s: governed mints with no declared rate: %s", name, strings.Join(missing, ", "))
		}
	}
}

func mustAmount(s string) money.Amount {
	a, err := money.Parse(s)
	if err != nil {
		panic(err)
	}
	return a
}
