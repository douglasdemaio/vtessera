package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/douglasdemaio/vtessera/internal/cluster"
	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/tokens"
)

type Config struct {
	Addr           string
	DatabaseURL    string
	SignerKeyPath  string
	SessionSecret  []byte
	ChallengeTTL   time.Duration
	SessionTTL     time.Duration
	RequestTimeout time.Duration
	ShutdownGrace  time.Duration
	PublicBaseURL  string
	Version        string
	// PreflightOnly runs the startup checks and exits without serving. It exists
	// so an operator can ask "is this endpoint actually the cluster I declared"
	// without a listener bound, a port claimed, or a signing key created.
	PreflightOnly bool
	// Solana holds the on-chain settlement configuration. An empty RPCURL leaves
	// on-chain settlement unconfigured, and the service then refuses on-chain
	// trades outright rather than half-handling them.
	Solana Solana
}

// Solana configures non-custodial on-chain settlement.
type Solana struct {
	// Cluster is the chain this deployment settles against. It is required
	// whenever RPCURL is set: an endpoint alone does not identify a chain, and a
	// service that guessed would price trades in tokens that do not exist where
	// the money would move.
	Cluster cluster.Cluster
	RPCURL  string
	// FeeLamports and FeeWallet are resolved values. The unresolvable question
	// of whether an operator set them is answered during Parse, because on
	// mainnet-beta the fee policy is pinned to the spec defaults and any
	// override is a startup error.
	FeeLamports uint64
	FeeWallet   string
	// BlockhashTTL is the advisory window during which an issued transaction is
	// still signable.
	BlockhashTTL time.Duration
	// LocalnetMints are operator-supplied mints, permitted only on localnet,
	// where the governed set is empty. Each is "address:symbol:decimals[:mintAuthority]".
	LocalnetMints []tokens.Token
	// LocalnetAllowHost lists hosts exempt from the localnet loopback rule, for
	// a validator in a separate container. Rejected on every other cluster.
	LocalnetAllowHost []string
}

// SettlementEnabled reports whether the deployment can honour on-chain
// settlement. Both an endpoint and a cluster are required: one without the
// other is a startup error, so reaching this method means both are present.
func (c Config) SettlementEnabled() bool {
	return strings.TrimSpace(c.Solana.RPCURL) != "" && c.Solana.Cluster != ""
}

type flagError struct{ msg string }

func (e *flagError) Error() string { return e.msg }

func ErrHelp() error { return &flagError{msg: "help requested"} }

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("vtessera", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		addr      = fs.String("addr", env("VTESSERA_ADDR", ":8080"), "listen address")
		dbURL     = fs.String("db", env("VTESSERA_DB", "file:vtessera.db"), "SQLite database DSN (Postgres is a planned target, not yet supported)")
		signerKey = fs.String("signer-key", env("VTESSERA_SIGNER_KEY", "data/signer.key"), "path to the Ed25519 marketplace signing key")
		secret    = fs.String("session-secret", os.Getenv("VTESSERA_SESSION_SECRET"), "session signing secret, at least 32 bytes (hex or base64)")
		challenge = fs.Duration("challenge-ttl", 5*time.Minute, "auth challenge lifetime")
		session   = fs.Duration("session-ttl", 24*time.Hour, "session token lifetime")
		timeout   = fs.Duration("request-timeout", 30*time.Second, "per request timeout")
		grace     = fs.Duration("shutdown-grace", 15*time.Second, "graceful shutdown budget")
		baseURL   = fs.String("public-base-url", os.Getenv("VTESSERA_PUBLIC_BASE_URL"), "public base URL advertised in the agent card")
		version   = fs.String("version", env("VTESSERA_VERSION", "0.1.0-dev"), "version advertised in the agent card")

		rpcURL        = fs.String("rpc-url", os.Getenv("VTESSERA_RPC_URL"), "Solana JSON-RPC endpoint; empty disables on-chain settlement")
		clusterName   = fs.String("cluster", os.Getenv("VTESSERA_CLUSTER"), "Solana cluster to settle against: mainnet-beta, devnet or localnet; required with an RPC endpoint")
		mainnetAck    = fs.String("mainnet-ack", os.Getenv("VTESSERA_MAINNET_ACK"), "must be 1 on mainnet-beta: an explicit acknowledgement that real value moves")
		feeLamports   = fs.String("fee-lamports", os.Getenv("VTESSERA_FEE_LAMPORTS"), "settlement fee in lamports; must be unset on mainnet-beta")
		feeWallet     = fs.String("fee-wallet", os.Getenv("VTESSERA_FEE_WALLET"), "Solana address that receives the settlement fee; must be unset on mainnet-beta")
		blockhashTTL  = fs.String("blockhash-ttl", os.Getenv("VTESSERA_BLOCKHASH_TTL"), "advisory window during which an issued settlement transaction stays signable")
		localnetMints = fs.String("localnet-mints", os.Getenv("VTESSERA_LOCALNET_MINTS"), "localnet only: comma-separated address:symbol:decimals[:mintAuthority]")
		localnetHosts = fs.String("localnet-allow-host", os.Getenv("VTESSERA_LOCALNET_ALLOW_HOST"), "localnet only: comma-separated hosts exempt from the loopback rule")
		preflightOnly = fs.Bool("preflight-only", false, "run the settlement preflight, print the report, and exit without serving")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "vtessera: A2A marketplace gateway with AGP routing and virtual tessera receipts\n\n")
		fmt.Fprintf(os.Stderr, "Usage: vtessera [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, ErrHelp()
		}
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// A malformed fee is a hard error rather than a silent fallback to the
	// default: quietly charging the wrong amount is worse than refusing to start.
	// The per-cluster table is enforced in parseSolana, which sees the raw
	// values and so can tell an unset variable from a default that happens to
	// equal one.
	decoded, err := decodeSecret(*secret)
	if err != nil {
		return Config{}, err
	}
	// A blockhash TTL that cannot be parsed is a startup error on every cluster.
	// Phase 2 fell back to the default, which meant a typo silently shortened or
	// lengthened the window a buyer has to sign; the rest of settlement is
	// fail-closed, so this was the one place that should have been too.
	ttl := settlement.DefaultBlockhashTTL
	if raw := unset(*blockhashTTL); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("blockhash-ttl %q is not a duration: %w", raw, err)
		}
		ttl = parsed
	}
	solanaCfg, err := parseSolana(*rpcURL, *clusterName, *mainnetAck, *feeLamports, *feeWallet, ttl, *localnetMints, *localnetHosts)
	if err != nil {
		return Config{}, err
	}
	// --preflight-only is a request to check the chain, so asking for it without
	// a chain to check is a contradiction. Failing here rather than serving
	// normally would be worse: an operator who asked for a check and silently
	// got a marketplace instead has no reason to look at the output again.
	if *preflightOnly && strings.TrimSpace(*rpcURL) == "" {
		return Config{}, fmt.Errorf("--preflight-only needs --rpc-url: there is no chain to check")
	}
	cfg := Config{
		Addr:           *addr,
		DatabaseURL:    *dbURL,
		SignerKeyPath:  *signerKey,
		SessionSecret:  decoded,
		ChallengeTTL:   *challenge,
		SessionTTL:     *session,
		RequestTimeout: *timeout,
		ShutdownGrace:  *grace,
		PublicBaseURL:  strings.TrimRight(*baseURL, "/"),
		Version:        *version,
		PreflightOnly:  *preflightOnly,
		Solana:         solanaCfg,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseSolana resolves the settlement settings and enforces the per-cluster
// table. The table is asymmetric on purpose: "mainnet-beta" and "devnet" are
// one keystroke apart in a string an operator types, so the configuration that
// lets value move must be the one that cannot be reached by accident.
//
// One rule for emptiness applies throughout: absent, present-and-empty, and
// whitespace-only are all the same thing. A deploy template that exports an
// empty string must not fail to boot.
func parseSolana(rpcURL, clusterName, mainnetAck, rawLamports, rawWallet string, ttl time.Duration, rawMints, rawHosts string) (Solana, error) {
	cfg := Solana{
		RPCURL:            unset(rpcURL),
		BlockhashTTL:      ttl,
		LocalnetMints:     []tokens.Token{},
		LocalnetAllowHost: splitList(rawHosts),
	}
	if cfg.RPCURL == "" {
		// Settlement stays off. A cluster without an endpoint is not a
		// half-configured deploy, it is an off-chain marketplace, and it is the
		// only state in which the 501 is the correct answer.
		if unset(clusterName) != "" {
			return Solana{}, fmt.Errorf("VTESSERA_CLUSTER is set to %q but VTESSERA_RPC_URL is not; on-chain settlement needs both or neither", clusterName)
		}
		return cfg, nil
	}
	// An endpoint without a cluster is the dangerous direction: everything
	// downstream would have to assume one, and the assumption would be invisible
	// at the call site.
	if unset(clusterName) == "" {
		return Solana{}, errors.New("VTESSERA_RPC_URL is set but VTESSERA_CLUSTER is not; name the cluster explicitly rather than having it inferred from the endpoint")
	}
	resolved, err := cluster.Parse(clusterName)
	if err != nil {
		return Solana{}, err
	}
	cfg.Cluster = resolved

	if resolved.IsProduction() {
		if unset(mainnetAck) != "1" {
			return Solana{}, fmt.Errorf("VTESSERA_MAINNET_ACK must be 1 on %s: settlement moves real value and this is the acknowledgement that you mean it", resolved)
		}
		// The fee policy is pinned to the spec defaults here. An override is not
		// a smaller version of the same thing, it is a different marketplace.
		if unset(rawLamports) != "" {
			return Solana{}, fmt.Errorf("VTESSERA_FEE_LAMPORTS must be unset on %s; the fee is fixed at %d lamports", resolved, fees.DefaultLamports)
		}
		if unset(rawWallet) != "" {
			return Solana{}, fmt.Errorf("VTESSERA_FEE_WALLET must be unset on %s; the fee destination is fixed at %s", resolved, fees.DefaultWallet)
		}
	} else {
		if unset(mainnetAck) != "" {
			return Solana{}, fmt.Errorf("VTESSERA_MAINNET_ACK is only meaningful on %s, not %s", cluster.MainnetBeta, resolved)
		}
	}
	// The localnet escape hatches are rejected off localnet by the same rule as
	// the settings themselves, so neither can widen anything in production.
	if !resolved.AllowsExtraMints() {
		if unset(rawMints) != "" {
			return Solana{}, fmt.Errorf("VTESSERA_LOCALNET_MINTS is only permitted on %s, not %s", cluster.Localnet, resolved)
		}
		if len(cfg.LocalnetAllowHost) > 0 {
			return Solana{}, fmt.Errorf("VTESSERA_LOCALNET_ALLOW_HOST is only permitted on %s, not %s", cluster.Localnet, resolved)
		}
	}

	policy, err := resolveFeePolicy(unset(rawLamports), unset(rawWallet))
	if err != nil {
		return Solana{}, err
	}
	cfg.FeeLamports = policy.Lamports()
	cfg.FeeWallet = policy.WalletAddress()
	if resolved.AllowsExtraMints() {
		mints, err := parseLocalnetMints(rawMints)
		if err != nil {
			return Solana{}, err
		}
		cfg.LocalnetMints = mints
	}
	// The endpoint host rule is checked here as well as at preflight: a
	// containerised validator is a legitimate localnet endpoint, and a
	// non-loopback mainnet endpoint is worth refusing before any RPC call.
	if err := resolved.CheckEndpoint(cfg.RPCURL, cfg.LocalnetAllowHost); err != nil {
		return Solana{}, err
	}
	return cfg, nil
}

func resolveFeePolicy(rawLamports, rawWallet string) (fees.Policy, error) {
	if rawLamports == "" && rawWallet == "" {
		return fees.Default(), nil
	}
	lamports := fees.DefaultLamports
	if rawLamports != "" {
		parsed, err := strconv.ParseUint(rawLamports, 10, 64)
		if err != nil {
			return fees.Policy{}, fmt.Errorf("fee-lamports %q is not a whole number of lamports: %w", rawLamports, err)
		}
		lamports = parsed
	}
	wallet := fees.DefaultWallet
	if rawWallet != "" {
		wallet = rawWallet
	}
	return fees.New(lamports, wallet)
}

// parseLocalnetMints reads "address:symbol:decimals[:mintAuthority]" entries.
// The mint authority is optional because a test fixture may have none, and an
// absent authority is a fact worth pinning as much as a present one.
func parseLocalnetMints(raw string) ([]tokens.Token, error) {
	entries := []tokens.Token{}
	for _, field := range splitList(raw) {
		parts := strings.Split(field, ":")
		if len(parts) < 3 || len(parts) > 4 {
			return nil, fmt.Errorf("localnet mint %q must be address:symbol:decimals[:mintAuthority]", field)
		}
		decimals, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil, fmt.Errorf("localnet mint %q has non-numeric decimals %q: %w", field, parts[2], err)
		}
		entry := tokens.Token{
			Address:  parts[0],
			Symbol:   parts[1],
			Decimals: decimals,
			Enabled:  true,
		}
		if len(parts) == 4 {
			entry.MintAuthority = parts[3]
		}
		if _, err := tokens.New(cluster.Localnet, []tokens.Token{entry}); err != nil {
			return nil, fmt.Errorf("localnet mint %q: %w", field, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// unset normalises the three ways a variable can be absent into one.
func unset(v string) string { return strings.TrimSpace(v) }

func splitList(v string) []string {
	raw := unset(v)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return errors.New("addr must not be empty")
	}
	if strings.TrimSpace(c.DatabaseURL) == "" {
		return errors.New("db must not be empty")
	}
	if len(c.SessionSecret) < 32 {
		return errors.New("session-secret must be at least 32 bytes: generate one with `openssl rand -hex 32`")
	}
	if c.ChallengeTTL <= 0 || c.SessionTTL <= 0 {
		return errors.New("challenge-ttl and session-ttl must be positive")
	}
	if c.RequestTimeout <= 0 {
		return errors.New("request-timeout must be positive")
	}
	if c.ShutdownGrace < 0 {
		return errors.New("shutdown-grace must not be negative")
	}
	if c.Solana.BlockhashTTL <= 0 {
		return errors.New("blockhash-ttl must be positive")
	}
	// An unconfigured deployment still validates: on-chain settlement is simply
	// refused. A configured one must be able to build a real settlement.
	if c.SettlementEnabled() {
		if _, err := fees.New(c.Solana.FeeLamports, c.Solana.FeeWallet); err != nil {
			return fmt.Errorf("fee policy: %w", err)
		}
	}
	return nil
}

// FeePolicy returns the validated settlement fee policy.
func (c Config) FeePolicy() (fees.Policy, error) {
	if c.Solana.FeeLamports == 0 && c.Solana.FeeWallet == "" {
		return fees.Default(), nil
	}
	return fees.New(c.Solana.FeeLamports, c.Solana.FeeWallet)
}

func decodeSecret(secret string) ([]byte, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("session-secret is required")
	}
	if len(secret) >= 64 {
		if decoded, err := hex.DecodeString(secret); err == nil {
			return decoded, nil
		}
	}
	if decoded, err := base64.StdEncoding.DecodeString(secret); err == nil && len(decoded) >= 32 {
		return decoded, nil
	}
	return []byte(secret), nil
}

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}
