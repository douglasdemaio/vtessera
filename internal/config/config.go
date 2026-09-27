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

	"github.com/douglasdemaio/vtessera/internal/fees"
	"github.com/douglasdemaio/vtessera/internal/settlement"
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
	// Solana holds the on-chain settlement configuration. An empty RPCURL leaves
	// on-chain settlement unconfigured, and the service then refuses on-chain
	// trades outright rather than half-handling them.
	Solana Solana
}

// Solana configures non-custodial on-chain settlement.
type Solana struct {
	RPCURL      string
	FeeLamports uint64
	FeeWallet   string
	// BlockhashTTL is the advisory window during which an issued transaction is
	// still signable.
	BlockhashTTL time.Duration
}

// SettlementEnabled reports whether the deployment can honour on-chain
// settlement.
func (c Config) SettlementEnabled() bool { return strings.TrimSpace(c.Solana.RPCURL) != "" }

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

		rpcURL       = fs.String("rpc-url", os.Getenv("VTESSERA_RPC_URL"), "Solana JSON-RPC endpoint; empty disables on-chain settlement")
		feeLamports  = fs.String("fee-lamports", env("VTESSERA_FEE_LAMPORTS", ""), "on-chain settlement fee in lamports; empty uses the default")
		feeWallet    = fs.String("fee-wallet", env("VTESSERA_FEE_WALLET", fees.DefaultWallet), "Solana address that receives the settlement fee")
		blockhashTTL = fs.Duration("blockhash-ttl", envDuration("VTESSERA_BLOCKHASH_TTL", settlement.DefaultBlockhashTTL), "advisory window during which an issued settlement transaction stays signable")
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
	lamports := fees.DefaultLamports
	if raw := strings.TrimSpace(*feeLamports); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("fee-lamports %q is not a whole number of lamports: %w", raw, err)
		}
		lamports = parsed
	}
	decoded, err := decodeSecret(*secret)
	if err != nil {
		return Config{}, err
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
		Solana: Solana{
			RPCURL:       strings.TrimSpace(*rpcURL),
			FeeLamports:  lamports,
			FeeWallet:    strings.TrimSpace(*feeWallet),
			BlockhashTTL: *blockhashTTL,
		},
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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

// envDuration reads a duration setting, falling back when unset. A malformed
// value falls back too, so a typo cannot stop the service from starting; the
// flag's own parse then reports the value it actually used.
func envDuration(key string, fallback time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}
