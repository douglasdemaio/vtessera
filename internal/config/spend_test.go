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

func TestSpendCapsDefaultToTenAndTen(t *testing.T) {
	cfg, err := Parse([]string{"--session-secret", goodSecret})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Spend.PerTradeUSD.String() != "10.00" {
		t.Errorf("per-trade cap = %s, want 10.00", cfg.Spend.PerTradeUSD)
	}
	if cfg.Spend.PerDayUSD.String() != "10.00" {
		t.Errorf("daily cap = %s, want 10.00", cfg.Spend.PerDayUSD)
	}
	// One maximum-size trade exhausts the day. A daily cap above the per-trade cap
	// would mean the daily cap is not what an operator reading it would assume.
	if cfg.Spend.PerDayUSD.Cmp(cfg.Spend.PerTradeUSD) < 0 {
		t.Errorf("daily cap %s is below the per-trade cap %s; one trade could not be refused for exceeding the day",
			cfg.Spend.PerDayUSD, cfg.Spend.PerTradeUSD)
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

func TestTheAdminTokenIsOptionalAndAbsentByDefault(t *testing.T) {
	cfg, err := Parse([]string{"--session-secret", goodSecret})
	if err != nil {
		t.Fatal(err)
	}
	// No default token. A deployment that has not chosen to have the capability
	// must not have one that anybody could have guessed.
	if len(cfg.AdminToken) != 0 {
		t.Errorf("admin token is set by default: %q", cfg.AdminToken)
	}
}

func TestAShortAdminTokenIsAStartupRefusal(t *testing.T) {
	// The token is a bearer credential held by whoever operates the marketplace,
	// so its only protection is length. A short one is a guessable one.
	_, err := Parse([]string{"--session-secret", goodSecret, "--admin-token", "hunter2"})
	if err == nil {
		t.Fatal("a two-word admin token was accepted")
	}
	if !strings.Contains(err.Error(), "admin-token") {
		t.Errorf("error = %v, want it to name the setting", err)
	}

	cfg, err := Parse([]string{
		"--session-secret", goodSecret,
		"--admin-token", strings.Repeat("a", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AdminToken) != 32 {
		t.Errorf("admin token length = %d, want 32", len(cfg.AdminToken))
	}
}

func TestTheAdminTokenCanComeFromTheEnvironment(t *testing.T) {
	t.Setenv("VTESSERA_ADMIN_TOKEN", strings.Repeat("b", 48))
	cfg, err := Parse([]string{"--session-secret", goodSecret})
	if err != nil {
		t.Fatal(err)
	}
	// Deployments set it as a secret, the same as the session secret, so it must
	// not have to appear in the process arguments where `ps` would show it.
	if len(cfg.AdminToken) != 48 {
		t.Errorf("admin token length = %d, want 48", len(cfg.AdminToken))
	}
}

func TestNamedOperatorTokensCarryTheirNameAndToken(t *testing.T) {
	alice := strings.Repeat("a", 32)
	bob := strings.Repeat("b", 32)
	cfg, err := Parse([]string{
		"--session-secret", goodSecret,
		"--admin-operators", "alice=" + alice + ",bob=" + bob,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AdminOperators) != 2 {
		t.Fatalf("operators = %d, want 2", len(cfg.AdminOperators))
	}
	if cfg.AdminOperators[0].Name != "alice" || string(cfg.AdminOperators[0].Token) != alice {
		t.Errorf("first operator = %+v, want alice bound to its token", cfg.AdminOperators[0])
	}
	if cfg.AdminOperators[1].Name != "bob" || string(cfg.AdminOperators[1].Token) != bob {
		t.Errorf("second operator = %+v, want bob bound to its token", cfg.AdminOperators[1])
	}
}

func TestNamedOperatorTokensCanComeFromTheEnvironment(t *testing.T) {
	// On Fly the credential is a secret, so the named form must work from the
	// environment rather than only from the process arguments.
	t.Setenv("VTESSERA_ADMIN_OPERATORS", "release="+strings.Repeat("c", 40))
	cfg, err := Parse([]string{"--session-secret", goodSecret})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AdminOperators) != 1 || cfg.AdminOperators[0].Name != "release" {
		t.Errorf("operators = %+v, want the named one from the environment", cfg.AdminOperators)
	}
}

func TestAnOperatorEntryWithoutANameOrTokenIsRefused(t *testing.T) {
	// A credential with no name is the header-trust problem over again, and a
	// credential with no token is a route nobody can use, so both are refused at
	// boot rather than half-configured.
	for _, entry := range []string{"alice", "=" + strings.Repeat("a", 32), "alice="} {
		if _, err := Parse([]string{"--session-secret", goodSecret, "--admin-operators", entry}); err == nil {
			t.Errorf("admin-operators %q was accepted", entry)
		}
	}
}

func TestAShortNamedOperatorTokenIsAStartupRefusal(t *testing.T) {
	_, err := Parse([]string{"--session-secret", goodSecret, "--admin-operators", "alice=hunter2"})
	if err == nil {
		t.Fatal("a short named operator token was accepted")
	}
}

func TestDuplicateOperatorNamesOrTokensAreRefused(t *testing.T) {
	tok := strings.Repeat("d", 32)
	_, err := Parse([]string{"--session-secret", goodSecret, "--admin-operators", "alice=" + tok + ",alice=" + strings.Repeat("e", 32)})
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Errorf("a repeated name was accepted: %v", err)
	}
	_, err = Parse([]string{"--session-secret", goodSecret, "--admin-operators", "alice=" + tok + ",bob=" + tok})
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Errorf("a repeated token was accepted: %v", err)
	}
	// The legacy token is an operator too, so a named entry cannot reuse it.
	_, err = Parse([]string{"--session-secret", goodSecret, "--admin-token", tok, "--admin-operators", "alice=" + tok})
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Errorf("a named token colliding with the legacy token was accepted: %v", err)
	}
}
