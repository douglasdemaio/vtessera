package config

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/tokens"
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

func TestUnparseableBlockhashTTLIsRefusedRatherThanIgnored(t *testing.T) {
	// A mistyped setting that silently falls back to the default is an operator
	// who believes they set a window and did not. The default is safe, but
	// knowing you did not choose it is the point, so this fails at boot.
	t.Setenv("VTESSERA_BLOCKHASH_TTL", "ninety seconds")
	if _, err := Parse([]string{"-session-secret", goodSecret}); err == nil {
		t.Fatal("err = nil, want a refusal naming the unparseable value")
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
		"-cluster", "localnet",
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
	secret := []string{
		"-session-secret", strings.Repeat("a", 40),
		"-rpc-url", "http://127.0.0.1:8899",
		"-cluster", "localnet",
	}
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

func TestSolanaConfigRefusesAnEndpointWithoutACluster(t *testing.T) {
	// The dangerous direction: an endpoint with no declared cluster. Inferring
	// one from the URL would be exactly the guess this work exists to remove, so
	// this fails at boot and names the missing setting.
	_, err := Parse([]string{
		"-session-secret", strings.Repeat("a", 40),
		"-rpc-url", "https://api.devnet.solana.com",
	})
	if err == nil {
		t.Fatal("err = nil, want a refusal naming the missing cluster")
	}
	if !strings.Contains(err.Error(), "VTESSERA_CLUSTER") {
		t.Errorf("err = %v, want the message to name VTESSERA_CLUSTER", err)
	}
}

func TestSolanaConfigRefusesAClusterWithoutAnEndpoint(t *testing.T) {
	// The other direction is also refused: a declared cluster with no endpoint
	// is a half-finished configuration, and starting would look like settlement
	// was configured when it is not.
	t.Setenv("VTESSERA_CLUSTER", "devnet")
	if _, err := Parse([]string{"-session-secret", goodSecret}); err == nil {
		t.Fatal("err = nil, want a refusal naming the missing endpoint")
	}
}

func TestSolanaConfigRefusesTestnetAsUnsupported(t *testing.T) {
	_, err := Parse([]string{
		"-session-secret", strings.Repeat("a", 40),
		"-rpc-url", "https://api.testnet.solana.com",
		"-cluster", "testnet",
	})
	if err == nil {
		t.Fatal("err = nil, want testnet refused as unsupported")
	}
	if !errors.Is(err, cluster.ErrClusterUnsupported) {
		t.Errorf("err = %v, want cluster.ErrClusterUnsupported", err)
	}
}

func TestLocalnetRejectsANonLoopbackEndpoint(t *testing.T) {
	// A localnet cluster pointed at a public host is a misconfiguration that
	// would otherwise be discovered by the operator reading a transfer.
	_, err := Parse([]string{
		"-session-secret", strings.Repeat("a", 40),
		"-rpc-url", "https://api.devnet.solana.com",
		"-cluster", "localnet",
	})
	if err == nil {
		t.Fatal("err = nil, want localnet to refuse a public endpoint")
	}
}

func TestMainnetSettlementRequiresAnExplicitAcknowledgement(t *testing.T) {
	base := []string{
		"-session-secret", strings.Repeat("a", 40),
		"-rpc-url", "https://solana.publicnode.com/",
		"-cluster", "mainnet-beta",
	}
	_, err := Parse(base)
	if err == nil {
		t.Fatal("err = nil, want mainnet-beta to require an explicit acknowledgement")
	}
	cfg, err := Parse(append(base, "-mainnet-ack", "1"))
	if err != nil {
		t.Fatalf("err = %v, want the acknowledged configuration to parse", err)
	}
	if cfg.Solana.Cluster != cluster.MainnetBeta {
		t.Errorf("cluster = %q, want mainnet-beta", cfg.Solana.Cluster)
	}
	if !cfg.SettlementEnabled() {
		t.Error("an acknowledged mainnet-beta endpoint should enable settlement")
	}
}

func TestLocalnetMintsAreRefusedOffLocalnet(t *testing.T) {
	_, err := Parse([]string{
		"-session-secret", strings.Repeat("a", 40),
		"-rpc-url", "https://api.devnet.solana.com",
		"-cluster", "devnet",
		"-localnet-mints", "TUSD:11111111111111111111111111111111:6",
	})
	if err == nil {
		t.Fatal("err = nil, want localnet mints refused on devnet")
	}
}

func TestLocalnetMintsParseSuccessfullyOnLocalnet(t *testing.T) {
	cfg, err := Parse([]string{
		"-session-secret", strings.Repeat("a", 40),
		"-rpc-url", "http://127.0.0.1:8899",
		"-cluster", "localnet",
		"-localnet-mints", "11111111111111111111111111111111:TUSD:6",
	})
	if err != nil {
		t.Fatalf("err = %v, want a valid localnet mint to parse", err)
	}
	if len(cfg.Solana.LocalnetMints) != 1 || cfg.Solana.LocalnetMints[0].Symbol != "TUSD" {
		t.Errorf("LocalnetMints = %+v, want one TUSD entry", cfg.Solana.LocalnetMints)
	}
}

func TestLocalnetMintsRejectsAMalformedEntryViaParse(t *testing.T) {
	_, err := Parse([]string{
		"-session-secret", strings.Repeat("a", 40),
		"-rpc-url", "http://127.0.0.1:8899",
		"-cluster", "localnet",
		"-localnet-mints", "11111111111111111111111111111111:TUSD:not-a-number",
	})
	if err == nil {
		t.Fatal("err = nil, want a malformed localnet mint entry rejected")
	}
}

func TestParseLocalnetMintsAcceptsAddressSymbolDecimals(t *testing.T) {
	mints, err := parseLocalnetMints("11111111111111111111111111111111:TUSD:6")
	if err != nil {
		t.Fatalf("err = %v, want a valid mint to parse", err)
	}
	want := []tokens.Token{{
		Address:  "11111111111111111111111111111111",
		Symbol:   "TUSD",
		Decimals: 6,
		Enabled:  true,
	}}
	if !reflect.DeepEqual(mints, want) {
		t.Errorf("mints = %+v, want %+v", mints, want)
	}
}

func TestParseLocalnetMintsAcceptsAnOptionalMintAuthority(t *testing.T) {
	mints, err := parseLocalnetMints("11111111111111111111111111111111:TUSD:6:11111111111111111111111111111111")
	if err != nil {
		t.Fatalf("err = %v, want a mint with an authority to parse", err)
	}
	if len(mints) != 1 || mints[0].MintAuthority != "11111111111111111111111111111111" {
		t.Errorf("mints = %+v, want a mint authority recorded", mints)
	}
}

func TestParseLocalnetMintsAcceptsMultipleCommaSeparatedEntries(t *testing.T) {
	mints, err := parseLocalnetMints("11111111111111111111111111111111:TUSD:6, So11111111111111111111111111111111111111112:TSOL:9")
	if err != nil {
		t.Fatalf("err = %v, want two valid mints to parse", err)
	}
	if len(mints) != 2 {
		t.Fatalf("len(mints) = %d, want 2", len(mints))
	}
}

func TestParseLocalnetMintsOnBlankInputReturnsNoEntries(t *testing.T) {
	mints, err := parseLocalnetMints("   ")
	if err != nil {
		t.Fatalf("err = %v, want a blank value to parse cleanly", err)
	}
	if len(mints) != 0 {
		t.Errorf("mints = %+v, want no entries", mints)
	}
}

func TestParseLocalnetMintsRejectsTheWrongFieldCount(t *testing.T) {
	for _, raw := range []string{
		"11111111111111111111111111111111:TUSD",
		"11111111111111111111111111111111:TUSD:6:authority:extra",
	} {
		if _, err := parseLocalnetMints(raw); err == nil {
			t.Errorf("parseLocalnetMints(%q) err = nil, want a field-count error", raw)
		}
	}
}

func TestParseLocalnetMintsRejectsNonNumericDecimals(t *testing.T) {
	if _, err := parseLocalnetMints("11111111111111111111111111111111:TUSD:six"); err == nil {
		t.Error("err = nil, want non-numeric decimals rejected")
	}
}

func TestParseLocalnetMintsRejectsAnInvalidAddress(t *testing.T) {
	if _, err := parseLocalnetMints("not-a-real-address:TUSD:6"); err == nil {
		t.Error("err = nil, want an unparseable address rejected")
	}
}

func TestSplitList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ", nil},
		{"single value", "a", []string{"a"}},
		{"trims surrounding whitespace", "  a  ", []string{"a"}},
		{"splits and trims multiple values", "a, b ,  c", []string{"a", "b", "c"}},
		{"drops empty fields between commas", "a,,b", []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitList(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitList(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestAnAcceptanceDeadlineIsRequired(t *testing.T) {
	// Caps are always on, so a service with no acceptance deadline could not
	// refuse a buyer at the off-chain commit and would enforce the daily cap a
	// window early instead. That is a weaker cap than the operator configured,
	// reached by a setting they never touched, so it has to stop the boot.
	dir := t.TempDir()
	t.Setenv("VTESSERA_TRADE_ACCEPT_TTL", "0")
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err == nil {
		t.Fatalf("Load with no acceptance deadline = %+v, want an error", cfg)
	}
	if !strings.Contains(err.Error(), "trade-accept-ttl") {
		t.Errorf("error = %v, want it to name trade-accept-ttl", err)
	}
}

func TestAnAcceptanceDeadlineDefaultsToThreeDays(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VTESSERA_TRADE_ACCEPT_TTL", "")
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AcceptTTL != 72*time.Hour {
		t.Errorf("AcceptTTL = %s, want 72h", cfg.AcceptTTL)
	}
	if cfg.OfferTTL != 24*time.Hour {
		t.Errorf("OfferTTL = %s, want 24h", cfg.OfferTTL)
	}
	if cfg.ExpirySweepEvery != 5*time.Minute {
		t.Errorf("ExpirySweepEvery = %s, want 5m", cfg.ExpirySweepEvery)
	}
	if cfg.ExpirySweepBatch != 100 {
		t.Errorf("ExpirySweepBatch = %d, want 100", cfg.ExpirySweepBatch)
	}
}

func TestAnOpenDeadlineIsRequired(t *testing.T) {
	// A proposed trade reserves its buyer's budget from creation, so a service
	// with no open deadline could hold that reservation forever. Like the
	// acceptance deadline, that is refused at boot rather than left to a note.
	dir := t.TempDir()
	t.Setenv("VTESSERA_TRADE_OPEN_TTL", "0")
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err == nil {
		t.Fatalf("Load with no open deadline = %+v, want an error", cfg)
	}
	if !strings.Contains(err.Error(), "trade-open-ttl") {
		t.Errorf("error = %v, want it to name trade-open-ttl", err)
	}
}

func TestAnOpenDeadlineDefaultsToADay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VTESSERA_TRADE_OPEN_TTL", "")
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OpenTTL != 24*time.Hour {
		t.Errorf("OpenTTL = %s, want 24h", cfg.OpenTTL)
	}
}

func TestAnOfferDeadlineIsRequired(t *testing.T) {
	// A listing with no deadline is the gap this closes: a seller that has
	// stopped answering stays discoverable, and a buyer cannot tell it from a
	// live one. A zero TTL has to stop the boot.
	dir := t.TempDir()
	t.Setenv("VTESSERA_OFFER_TTL", "0")
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err == nil {
		t.Fatalf("Parse with no offer deadline = %+v, want an error", cfg)
	}
	if !strings.Contains(err.Error(), "offer-ttl") {
		t.Errorf("error = %v, want it to name offer-ttl", err)
	}
}

func TestTheExpirySweepMustRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VTESSERA_TRADE_EXPIRY_SWEEP_INTERVAL", "0")
	_, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err == nil {
		t.Fatal("Parse with a sweep interval of zero = no error, want one")
	}
	if !strings.Contains(err.Error(), "trade-expiry-sweep-interval") {
		t.Errorf("error = %v, want it to name trade-expiry-sweep-interval", err)
	}
}

// The requirement is the operator's to declare, so it is off unless they say so.
// Turning it on by default would answer every agent that predates attestations
// with a 409 the moment it deployed, which is a migration nobody asked for.
func TestOfferAttestationIsNotRequiredUnlessAnOperatorDeclaresIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VTESSERA_REQUIRE_OFFER_ATTESTATION", "")
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequireOfferAttestation {
		t.Error("offer attestation is required without an operator asking for it")
	}
}

// Both spellings are accepted, because a deployment's settings arrive as secrets
// on the live host and as flags elsewhere, and only one of them working would
// leave an operator believing they had enforced something.
func TestOfferAttestationIsRequiredWhenDeclaredByFlagOrEnvironment(t *testing.T) {
	t.Setenv("VTESSERA_REQUIRE_OFFER_ATTESTATION", "")
	dir := t.TempDir()
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db"), "--require-offer-attestation"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequireOfferAttestation {
		t.Error("--require-offer-attestation did not take effect")
	}

	t.Setenv("VTESSERA_REQUIRE_OFFER_ATTESTATION", "1")
	cfg, err = Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequireOfferAttestation {
		t.Error("VTESSERA_REQUIRE_OFFER_ATTESTATION=1 did not take effect")
	}
}

// A probe that could hang forever would hold a connection this service opened, so
// the timeout and the body cap are bounded values and a mistyped one is refused at
// boot rather than discovered on the first unreachable agent.
func TestProbeLimitsAreBoundedAndMistypedOnesAreRefusedAtBoot(t *testing.T) {
	t.Setenv("VTESSERA_PROBE_TIMEOUT", "")
	t.Setenv("VTESSERA_PROBE_MAX_RESPONSE_BYTES", "")
	dir := t.TempDir()
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProbeTimeout <= 0 || cfg.ProbeTimeout > time.Minute {
		t.Errorf("probe timeout = %s, want a short positive default", cfg.ProbeTimeout)
	}
	if cfg.ProbeMaxResponseBytes <= 0 {
		t.Errorf("probe max response bytes = %d, want a positive default", cfg.ProbeMaxResponseBytes)
	}

	if _, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "d.db"),
		"--probe-timeout", "not-a-duration"}); err == nil {
		t.Error("a probe timeout that is not a duration was accepted")
	}
	if _, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "e.db"),
		"--probe-timeout", "0s"}); err == nil {
		t.Error("a probe timeout of zero was accepted, which would abandon every probe immediately")
	}
	if _, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "f.db"),
		"--probe-max-response-bytes", "0"}); err == nil {
		t.Error("a zero response cap was accepted")
	}
	t.Setenv("VTESSERA_PROBE_TIMEOUT", "nonsense")
	if _, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "g.db")}); err == nil {
		t.Error("a mistyped VTESSERA_PROBE_TIMEOUT was accepted")
	}
	t.Setenv("VTESSERA_PROBE_TIMEOUT", "")
	t.Setenv("VTESSERA_PROBE_MAX_RESPONSE_BYTES", "not-a-number")
	if _, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "h.db")}); err == nil {
		t.Error("a mistyped VTESSERA_PROBE_MAX_RESPONSE_BYTES was accepted")
	}
}

// The declared limits are what the runner is built with, so an operator's setting
// reaches the request rather than only the config struct.
func TestDeclaredProbeLimitsReachTheConfiguration(t *testing.T) {
	t.Setenv("VTESSERA_PROBE_TIMEOUT", "")
	t.Setenv("VTESSERA_PROBE_MAX_RESPONSE_BYTES", "")
	dir := t.TempDir()
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db"),
		"--probe-timeout", "2s", "--probe-max-response-bytes", "4096"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProbeTimeout != 2*time.Second {
		t.Errorf("probe timeout = %s, want 2s", cfg.ProbeTimeout)
	}
	if cfg.ProbeMaxResponseBytes != 4096 {
		t.Errorf("probe max response bytes = %d, want 4096", cfg.ProbeMaxResponseBytes)
	}
}

func TestRequestRateLimitsAreOnByDefault(t *testing.T) {
	// The threat model recorded no rate limiting at all, so the deployment that
	// has not been tuned is the one that most needs the default.
	dir := t.TempDir()
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimit.AgentBurst <= 0 || cfg.RateLimit.IPBurst <= 0 {
		t.Errorf("rate limit = %+v, want both layers on by default", cfg.RateLimit)
	}
	if cfg.RateLimit.IPHeader != "Fly-Client-IP" {
		t.Errorf("ip header = %q, want the header Fly sets", cfg.RateLimit.IPHeader)
	}
}

func TestARateWithNoBurstIsRefused(t *testing.T) {
	// A rate with no tokens never allows anything, which is not what an
	// operator setting a rate meant; it is refused at boot rather than shipping
	// a limit that silently blocks the route.
	dir := t.TempDir()
	t.Setenv("VTESSERA_RATE_LIMIT_AGENT_RPS", "10")
	t.Setenv("VTESSERA_RATE_LIMIT_AGENT_BURST", "0")
	cfg, err := Parse([]string{"--session-secret", goodSecret, "--db", filepath.Join(dir, "c.db")})
	if err == nil {
		t.Fatalf("Parse = %+v, want an error", cfg)
	}
	if !strings.Contains(err.Error(), "rate-limit-agent-burst") {
		t.Errorf("error = %v, want it to name rate-limit-agent-burst", err)
	}
}
