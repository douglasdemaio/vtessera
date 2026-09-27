package config

import (
	"strings"

	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/settlement"
)

const goodSecret = "0123456789abcdef0123456789abcdef"

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]string{"--session-secret", goodSecret})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("addr = %s", cfg.Addr)
	}
	if cfg.ChallengeTTL != 5*time.Minute {
		t.Errorf("challenge ttl = %s", cfg.ChallengeTTL)
	}
	if string(cfg.SessionSecret) != goodSecret {
		t.Errorf("secret = %q", cfg.SessionSecret)
	}
}

func TestParseRejectsMissingSecret(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Fatal("want an error when no session secret is provided")
	}
}

func TestParseRejectsShortSecret(t *testing.T) {
	if _, err := Parse([]string{"--session-secret", "too-short"}); err == nil {
		t.Fatal("want an error for a secret under 32 bytes")
	}
}

func TestParseAcceptsHexAndBase64Secrets(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	cfg, err := Parse([]string{"--session-secret", hex})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SessionSecret) != 32 {
		t.Errorf("hex secret decoded to %d bytes, want 32", len(cfg.SessionSecret))
	}
	b64 := "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	cfg, err = Parse([]string{"--session-secret", b64})
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.SessionSecret) != "0123456789abcdef0123456789abcdef" {
		t.Errorf("base64 secret decoded to %q", cfg.SessionSecret)
	}
}

func TestParseRejectsStrayArgument(t *testing.T) {
	if _, err := Parse([]string{"--session-secret", goodSecret, "serve"}); err == nil {
		t.Fatal("want an error for an unexpected positional argument")
	}
}

func TestParseRejectsBadDurations(t *testing.T) {
	if _, err := Parse([]string{"--session-secret", goodSecret, "--session-ttl", "0s"}); err == nil {
		t.Fatal("want an error for a zero session ttl")
	}
	if _, err := Parse([]string{"--session-secret", goodSecret, "--request-timeout", "-1s"}); err == nil {
		t.Fatal("want an error for a negative request timeout")
	}
}

func TestPublicBaseURLIsTrimmed(t *testing.T) {
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--public-base-url", "https://x.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicBaseURL != "https://x.example.com" {
		t.Errorf("base url = %q", cfg.PublicBaseURL)
	}
}

func TestEnvFallback(t *testing.T) {
	t.Setenv("VTESSERA_ADDR", ":9999")
	cfg, err := Parse([]string{"--session-secret", goodSecret})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("addr = %s, want the environment value", cfg.Addr)
	}
}

func TestBlockhashTTLComesFromTheEnvironment(t *testing.T) {
	t.Setenv("VTESSERA_BLOCKHASH_TTL", "45s")
	cfg, err := Parse([]string{"-session-secret", goodSecret})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Solana.BlockhashTTL != 45*time.Second {
		t.Errorf("blockhashTtl = %s, want the environment value 45s", cfg.Solana.BlockhashTTL)
	}
}

func TestUnparseableBlockhashTTLFallsBackToTheDefault(t *testing.T) {
	// A typo must not stop the service from starting; the default is safe.
	t.Setenv("VTESSERA_BLOCKHASH_TTL", "ninety seconds")
	cfg, err := Parse([]string{"-session-secret", goodSecret})
	if err != nil {
		t.Fatalf("err = %v, want a startable configuration", err)
	}
	if cfg.Solana.BlockhashTTL != settlement.DefaultBlockhashTTL {
		t.Errorf("blockhashTtl = %s, want the default %s", cfg.Solana.BlockhashTTL, settlement.DefaultBlockhashTTL)
	}
}

func TestSolanaConfigDisablesSettlementWhenNoRPCURL(t *testing.T) {
	base := []string{"-session-secret", strings.Repeat("a", 40)}
	cfg, err := Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SettlementEnabled() {
		t.Error("settlement must be off without an RPC endpoint")
	}
	policy, err := cfg.FeePolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Lamports() != fees.DefaultLamports || policy.WalletAddress() != fees.DefaultWallet {
		t.Errorf("policy = %d lamports to %s, want the spec defaults", policy.Lamports(), policy.WalletAddress())
	}
}

func TestSolanaConfigEnablesSettlement(t *testing.T) {
	cfg, err := Parse([]string{
		"-session-secret", strings.Repeat("a", 40),
		"-rpc-url", "http://127.0.0.1:8899",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SettlementEnabled() {
		t.Error("settlement should be enabled with an RPC endpoint")
	}
	policy, err := cfg.FeePolicy()
	if err != nil {
		t.Fatal(err)
	}
	if policy.Lamports() != fees.DefaultLamports {
		t.Errorf("fee = %d, want the default %d", policy.Lamports(), fees.DefaultLamports)
	}
}

func TestSolanaConfigRejectsAnUnusableFeePolicy(t *testing.T) {
	secret := []string{"-session-secret", strings.Repeat("a", 40), "-rpc-url", "http://127.0.0.1:8899"}
	cases := map[string][]string{
		"zero fee":     {"-fee-lamports", "0"},
		"bad wallet":   {"-fee-wallet", "not-a-solana-address"},
		"not a number": {"-fee-lamports", "half a sol"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(append(append([]string{}, secret...), extra...)); err == nil {
				t.Error("expected a fee policy that cannot be settled to be refused at startup")
			}
		})
	}
}

func TestSolanaConfigRejectsAZeroBlockhashTTL(t *testing.T) {
	if _, err := Parse([]string{
		"-session-secret", strings.Repeat("a", 40),
		"-blockhash-ttl", "0s",
	}); err == nil {
		t.Error("expected a non-positive blockhash ttl to be refused")
	}
}
