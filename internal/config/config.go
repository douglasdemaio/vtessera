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
	"github.com/douglasdemaio/vtessera/internal/limits"
	"github.com/douglasdemaio/vtessera/internal/money"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/tokens"
)

type Config struct {
	Addr          string
	DatabaseURL   string
	SignerKeyPath string
	SessionSecret []byte
	// AdminToken authorises the operator routes that act on other agents:
	// retiring and restoring a listing. It is separate from the session secret
	// because it is a different kind of credential — one is issued to agents and
	// proves an agent is itself, the other is held by the operator and proves
	// the caller is the marketplace. Sharing one would let every agent retire
	// every other agent.
	//
	// Unset means those routes are absent rather than open. There is no default
	// token to guess and no way to mint one, so a deployment that has not chosen
	// to have this capability does not have it.
	AdminToken []byte
	// AdminOperators are named operator credentials. Each authorises the same
	// routes as AdminToken, but the name is a principal: the audit row records
	// the name bound to the token that was presented, not a header the caller
	// typed. Several can be configured at once, which is a rotation path — add
	// the new token, retire the old one, and the trail spans both.
	AdminOperators []AdminOperator
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
	// Spend bounds what one agent can commit through this marketplace. It is a
	// safety control, so it is configured at the top level rather than under
	// Solana: it applies to off-chain trades too, where no cluster exists.
	Spend Spend
	// AcceptTTL is how long an accepted trade may wait to be committed before it
	// can be cancelled. Zero means no deadline, which leaves the spending cap
	// unenforceable at the off-chain commit and is therefore only safe when caps
	// are off.
	AcceptTTL time.Duration
	// OpenTTL is how long a proposed or negotiating trade may sit untouched
	// before it is cancelled. Creating a trade reserves its amount against the
	// buyer's cap, so without this an offer nobody answers holds that budget
	// forever.
	OpenTTL time.Duration
	// ExpirySweepEvery is how often expired accepted trades are swept.
	ExpirySweepEvery time.Duration
	// ExpirySweepBatch bounds one sweep.
	ExpirySweepBatch int
	// OfferTTL is how long a published offer stays open before the sweeper closes
	// it. Zero means no deadline, which leaves a silent seller's listing
	// discoverable indefinitely — the state gap 4 exists to remove.
	OfferTTL time.Duration
	// RateLimit bounds how often one caller may hit the API. It is on by
	// default because the alternative the threat model recorded was no limit at
	// all; a zero burst disables one layer for an operator who wants to.
	RateLimit RateLimit
	// RequireOfferAttestation makes a seller-signed offer the only kind that can
	// be published. It is off by default because an agent that predates
	// attestations publishes unsigned offers, and turning this on is how an
	// operator says they have finished migrating their agents rather than a
	// change that should surprise them at deploy time.
	//
	// The marketplace still attests every card while this is off, and a buyer
	// reading an unsigned offer is told plainly that nobody but the seller has
	// vouched for the terms.
	RequireOfferAttestation bool
	// ProbeTimeout and ProbeMaxResponseBytes bound one capability probe. They are
	// configured rather than fixed because they are the only knobs standing between
	// an unreachable agent and a connection this service is holding open.
	ProbeTimeout          time.Duration
	ProbeMaxResponseBytes int64
	// Sandbox marks this deployment as one where no real value moves. It is a
	// behavioural switch, not a label: on-chain settlement is refused outright,
	// so an agent cannot mistake it for the real thing.
	Sandbox bool
	// Solana holds the on-chain settlement configuration. An empty RPCURL leaves
	// on-chain settlement unconfigured, and the service then refuses on-chain
	// trades outright rather than half-handling them.
	Solana Solana
}

// Spend holds the spending caps and the prices they are measured against.
type Spend struct {
	// PerTradeUSD and PerDayUSD are the caps an agent gets without asking. They
	// are USD decimal strings, not base units, because a cap is a figure an
	// operator reads and an agent argues with.
	PerTradeUSD money.Amount
	PerDayUSD   money.Amount
	// CeilingPerTradeUSD and CeilingPerDayUSD are the most any single agent may
	// raise its caps to. Unset means an agent cannot raise anything at all,
	// which is the safe direction: an opt-in with no ceiling is not a cap.
	CeilingPerTradeUSD money.Amount
	CeilingPerDayUSD   money.Amount
	// DailyWindow is the rolling period the daily cap is measured over.
	DailyWindow time.Duration
	// Rates are the declared prices, keyed by mint address. A governed mint
	// missing from this table makes the deployment refuse to start, because a cap
	// that cannot be priced is not a cap.
	Rates limits.Rates
}

// RateLimit bounds request rates. The two layers are independent:
// authenticated requests are charged to the agent's key, and everything else to
// the client address the proxy supplies.
type RateLimit struct {
	AgentRPS   float64
	AgentBurst int
	IPRPS      float64
	IPBurst    int
	// IPHeader is the proxy header carrying the client address. Empty uses the
	// connection's own address, which behind a proxy is the proxy's, so
	// deployments behind one should name the header their proxy sets.
	IPHeader string
}

// AdminOperator is a named operator credential. The name is the principal the
// audit row records, so it has to come from the credential rather than from the
// request; see the threat model.
type AdminOperator struct {
	Name  string
	Token []byte
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
		adminTok  = fs.String("admin-token", os.Getenv("VTESSERA_ADMIN_TOKEN"), "operator token authorising agent retirement; unset removes the admin routes entirely")
		adminOps  = fs.String("admin-operators", env("VTESSERA_ADMIN_OPERATORS", ""), "named operator tokens as name=token entries separated by commas; the name is recorded in the audit row")
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

		spendPerTrade   = fs.String("spend-cap-per-trade", env("VTESSERA_SPEND_CAP_PER_TRADE", "10.00"), "USD cap a single trade may commit without an opt-in")
		spendPerDay     = fs.String("spend-cap-per-day", env("VTESSERA_SPEND_CAP_PER_DAY", "10.00"), "USD cap one agent may commit per rolling day without an opt-in")
		spendCeilTrade  = fs.String("spend-cap-max-per-trade", os.Getenv("VTESSERA_SPEND_CAP_MAX_PER_TRADE"), "most any one agent may raise its per-trade cap to; unset forbids raising it")
		spendCeilDay    = fs.String("spend-cap-max-per-day", os.Getenv("VTESSERA_SPEND_CAP_MAX_PER_DAY"), "most any one agent may raise its daily cap to; unset forbids raising it")
		spendWindow     = fs.String("spend-cap-window", env("VTESSERA_SPEND_CAP_WINDOW", "24h"), "rolling window the daily cap is measured over")
		spendRates      = fs.String("spend-rates", os.Getenv("VTESSERA_SPEND_RATES"), "extra USD rates as a comma-separated list of mint=usd@decimals; the governed stablecoins are built in at par")
		acceptTTL       = fs.String("trade-accept-ttl", env("VTESSERA_TRADE_ACCEPT_TTL", "72h"), "how long an accepted trade may wait to be committed before either party may cancel it")
		openTTL         = fs.String("trade-open-ttl", env("VTESSERA_TRADE_OPEN_TTL", "24h"), "how long a proposed or negotiating trade may sit untouched before it is cancelled and its reservation released")
		offerTTL        = fs.String("offer-ttl", env("VTESSERA_OFFER_TTL", "24h"), "how long a published offer stays open before the sweeper closes it")
		sweepEvery      = fs.String("trade-expiry-sweep-interval", env("VTESSERA_TRADE_EXPIRY_SWEEP_INTERVAL", "5m"), "how often to sweep expired trades and offers")
		sweepBatch      = fs.Int("trade-expiry-sweep-batch", 100, "how many expired trades or offers one sweep may close")
		probeTimeout    = fs.String("probe-timeout", env("VTESSERA_PROBE_TIMEOUT", "5s"), "how long one capability probe may take before it is abandoned")
		probeBodyBytes  = fs.Int64("probe-max-response-bytes", envInt64("VTESSERA_PROBE_MAX_RESPONSE_BYTES", 65536), "largest capability probe response this service will read")
		requireOfferSig = fs.Bool("require-offer-attestation", env("VTESSERA_REQUIRE_OFFER_ATTESTATION", "") == "1", "refuse to publish an offer the seller has not signed, so every live listing is verifiable")
		sandbox         = fs.Bool("sandbox", env("VTESSERA_SANDBOX", "") == "1", "refuse on-chain settlement and advertise this deployment as a sandbox where no real value moves")
		rateAgentRPS    = fs.Float64("rate-limit-agent-rps", envFloat("VTESSERA_RATE_LIMIT_AGENT_RPS", 30), "requests per second allowed to one authenticated agent; 0 means the bucket never refills")
		rateAgentBurst  = fs.Int("rate-limit-agent-burst", envInt("VTESSERA_RATE_LIMIT_AGENT_BURST", 60), "burst an authenticated agent may spend at once; 0 disables the per-agent limit")
		rateIPRPS       = fs.Float64("rate-limit-ip-rps", envFloat("VTESSERA_RATE_LIMIT_IP_RPS", 20), "requests per second allowed to one client address; 0 means the bucket never refills")
		rateIPBurst     = fs.Int("rate-limit-ip-burst", envInt("VTESSERA_RATE_LIMIT_IP_BURST", 40), "burst one client address may spend at once; 0 disables the per-address limit")
		rateIPHeader    = fs.String("rate-limit-ip-header", env("VTESSERA_RATE_LIMIT_IP_HEADER", "Fly-Client-IP"), "header the proxy sets to the client address; empty uses the connection address")
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
	spendCfg, err := parseSpend(*spendPerTrade, *spendPerDay, *spendCeilTrade, *spendCeilDay, *spendRates, *spendWindow)
	if err != nil {
		return Config{}, err
	}
	var adminToken []byte
	if tok := strings.TrimSpace(*adminTok); tok != "" {
		if len(tok) < 32 {
			return Config{}, errors.New("admin-token must be at least 32 characters: generate one with `openssl rand -hex 32`")
		}
		adminToken = []byte(tok)
	}
	adminOperators, err := parseAdminOperators(*adminOps, adminToken)
	if err != nil {
		return Config{}, err
	}
	acceptTTLDur, err := time.ParseDuration(unset(*acceptTTL))
	if err != nil {
		return Config{}, fmt.Errorf("trade-accept-ttl %q is not a duration: %w", *acceptTTL, err)
	}
	openTTLDur, err := time.ParseDuration(unset(*openTTL))
	if err != nil {
		return Config{}, fmt.Errorf("trade-open-ttl %q is not a duration: %w", *openTTL, err)
	}
	offerTTLDur, err := time.ParseDuration(unset(*offerTTL))
	if err != nil {
		return Config{}, fmt.Errorf("offer-ttl %q is not a duration: %w", *offerTTL, err)
	}
	probeTimeoutDur, err := time.ParseDuration(unset(*probeTimeout))
	if err != nil {
		return Config{}, fmt.Errorf("probe-timeout %q is not a duration: %w", *probeTimeout, err)
	}
	sweepEveryDur, err := time.ParseDuration(unset(*sweepEvery))
	if err != nil {
		return Config{}, fmt.Errorf("trade-expiry-sweep-interval %q is not a duration: %w", *sweepEvery, err)
	}
	if probeTimeoutDur <= 0 {
		return Config{}, fmt.Errorf("probe-timeout %q is not a positive duration", *probeTimeout)
	}
	if *probeBodyBytes <= 0 {
		return Config{}, fmt.Errorf("probe-max-response-bytes must be positive, got %d", *probeBodyBytes)
	}
	if *sweepBatch <= 0 {
		return Config{}, fmt.Errorf("trade-expiry-sweep-batch must be positive, got %d", *sweepBatch)
	}
	cfg := Config{
		AcceptTTL:        acceptTTLDur,
		OpenTTL:          openTTLDur,
		OfferTTL:         offerTTLDur,
		ExpirySweepEvery: sweepEveryDur,
		ExpirySweepBatch: *sweepBatch,

		RequireOfferAttestation: *requireOfferSig,

		ProbeTimeout:          probeTimeoutDur,
		ProbeMaxResponseBytes: *probeBodyBytes,
		Addr:                  *addr,
		DatabaseURL:           *dbURL,
		SignerKeyPath:         *signerKey,
		SessionSecret:         decoded,
		AdminToken:            adminToken,
		AdminOperators:        adminOperators,
		ChallengeTTL:          *challenge,
		SessionTTL:            *session,
		RequestTimeout:        *timeout,
		ShutdownGrace:         *grace,
		PublicBaseURL:         strings.TrimRight(*baseURL, "/"),
		Version:               *version,
		PreflightOnly:         *preflightOnly,
		Spend:                 spendCfg,
		Sandbox:               *sandbox,
		Solana:                solanaCfg,
		RateLimit: RateLimit{
			AgentRPS:   *rateAgentRPS,
			AgentBurst: *rateAgentBurst,
			IPRPS:      *rateIPRPS,
			IPBurst:    *rateIPBurst,
			IPHeader:   unset(*rateIPHeader),
		},
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

// parseSpend resolves the cap configuration. Every malformed figure is a startup
// error rather than a default: a cap that silently measures the wrong amount is
// worse than no cap, because it reads as a limit that is being enforced.
func parseSpend(rawPerTrade, rawPerDay, rawCeilTrade, rawCeilDay, rawRates, rawWindow string) (Spend, error) {
	window, err := time.ParseDuration(unset(rawWindow))
	if err != nil {
		return Spend{}, fmt.Errorf("spend-cap-window %q is not a duration: %w", rawWindow, err)
	}
	out := Spend{DailyWindow: window, Rates: limits.DefaultRates()}

	perTrade, err := parseCap("spend-cap-per-trade", rawPerTrade)
	if err != nil {
		return Spend{}, err
	}
	out.PerTradeUSD = perTrade

	perDay, err := parseCap("spend-cap-per-day", rawPerDay)
	if err != nil {
		return Spend{}, err
	}
	out.PerDayUSD = perDay

	// The ceilings use os.Getenv upstream, so an unset variable is genuinely
	// unset here. That distinction is load-bearing: unset means an agent cannot
	// raise its caps at all.
	if raw := unset(rawCeilTrade); raw != "" {
		parsed, err := parseCap("spend-cap-max-per-trade", raw)
		if err != nil {
			return Spend{}, err
		}
		out.CeilingPerTradeUSD = parsed
	}
	if raw := unset(rawCeilDay); raw != "" {
		parsed, err := parseCap("spend-cap-max-per-day", raw)
		if err != nil {
			return Spend{}, err
		}
		out.CeilingPerDayUSD = parsed
	}
	if window <= 0 {
		return Spend{}, fmt.Errorf("spend-cap-window %q must be positive", window)
	}

	for _, entry := range splitList(rawRates) {
		mint, rest, ok := strings.Cut(entry, "=")
		if !ok {
			return Spend{}, fmt.Errorf("spend-rates entry %q is not mint=usd@decimals", entry)
		}
		usd, decimals, ok := strings.Cut(rest, "@")
		if !ok {
			return Spend{}, fmt.Errorf("spend-rates entry %q is missing @decimals", entry)
		}
		parsed, err := parseCap("spend-rates", usd)
		if err != nil {
			return Spend{}, err
		}
		dp, err := strconv.ParseUint(unset(decimals), 10, 8)
		if err != nil {
			return Spend{}, fmt.Errorf("spend-rates entry %q: decimals %q is not a whole number", entry, decimals)
		}
		if err := out.Rates.Add(mint, limits.Rate{Decimals: uint8(dp), USD: parsed}); err != nil {
			return Spend{}, fmt.Errorf("spend-rates entry %q: %w", entry, err)
		}
	}
	return out, nil
}

// parseCap reads one USD cap. A zero cap is refused rather than accepted,
// because an agent whose cap is zero cannot trade at all, which is a different
// thing from an agent that is bounded.
func parseCap(name, raw string) (money.Amount, error) {
	trimmed := unset(raw)
	if trimmed == "" {
		return money.Amount{}, fmt.Errorf("%s is empty: a cap is a figure, not a blank", name)
	}
	amount, err := money.Parse(trimmed)
	if err != nil {
		return money.Amount{}, fmt.Errorf("%s %q is not a decimal amount: %w", name, raw, err)
	}
	if amount.IsZero() {
		return money.Amount{}, fmt.Errorf("%s %q: %w", name, raw, limits.ErrCapNotPositive)
	}
	return amount, nil
}

// Policy builds the cap policy this configuration describes. The per-agent
// opt-in is supplied by the caller because the values live in the store.
func (s Spend) Policy() limits.Policy {
	p := limits.DefaultPolicy(s.Rates).
		WithWindow(s.DailyWindow).
		WithCeilings(s.CeilingPerTradeUSD, s.CeilingPerDayUSD)
	p.PerTrade, p.PerDay = s.PerTradeUSD, s.PerDayUSD
	return p
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

// envInt64 reads an integer setting from the environment, falling back to the
// declared default when it is unset.
func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		// A malformed value is reported as the default here and then as a parse
		// failure by the flag below, so an operator who mistypes it finds out at
		// boot rather than running with a limit they did not choose.
		return -1
	}
	return fallback
}

// envInt reads an integer setting from the environment, falling back to the
// declared default when it is unset. A malformed value returns -1 so the
// operator finds out at boot rather than running with a limit they did not
// choose.
func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		return -1
	}
	return fallback
}

// envFloat reads a decimal setting from the environment, falling back to the
// declared default when it is unset.
func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		return -1
	}
	return fallback
}

// parseAdminOperators reads the name=token list and refuses a misconfiguration
// at boot rather than an operator discovering at the moment of an incident that
// the credential they are holding carries no name. It also checks the named
// tokens against the legacy single token so the two forms cannot collide.
func parseAdminOperators(raw string, legacyToken []byte) ([]AdminOperator, error) {
	names := map[string]bool{}
	tokens := map[string]bool{}
	if len(legacyToken) > 0 {
		names["operator"] = true
		tokens[string(legacyToken)] = true
	}
	var ops []AdminOperator
	for _, entry := range splitList(raw) {
		name, token, ok := strings.Cut(entry, "=")
		name, token = strings.TrimSpace(name), strings.TrimSpace(token)
		if !ok || name == "" || token == "" {
			return nil, fmt.Errorf("admin-operators entry %q must be name=token", entry)
		}
		if len(token) < 32 {
			return nil, fmt.Errorf("admin-operators token for %q must be at least 32 characters", name)
		}
		if names[name] {
			return nil, fmt.Errorf("admin-operators names must be unique: %q is configured twice", name)
		}
		if tokens[token] {
			return nil, fmt.Errorf("admin-operators tokens must be unique: the token for %q is already configured", name)
		}
		names[name] = true
		tokens[token] = true
		ops = append(ops, AdminOperator{Name: name, Token: []byte(token)})
	}
	return ops, nil
}

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
	if err := c.Spend.Policy().Validate(); err != nil {
		return fmt.Errorf("spending caps: %w", err)
	}
	// Sandbox and an on-chain endpoint contradict each other, and silently
	// dropping the endpoint would be the worst way to resolve it: an operator who
	// set both would get a deployment that looked configured and refused every
	// settlement for a reason nothing in the output said.
	if c.Sandbox && c.SettlementEnabled() {
		return errors.New("sandbox mode refuses on-chain settlement, but an rpc-url and cluster are set: unset VTESSERA_RPC_URL and VTESSERA_CLUSTER together, or unset VTESSERA_SANDBOX")
	}
	// Caps are always on, so a missing acceptance deadline would silently leave
	// the daily cap unenforced at the off-chain commit. That is a weaker cap than
	// the one the operator configured, reached by a setting they never touched, so
	// it is a startup error rather than a note in a log.
	if c.AcceptTTL <= 0 {
		return errors.New("trade-accept-ttl must be positive: an accepted trade with no deadline cannot be cancelled, and a trade that cannot be cancelled means the daily spending cap cannot be enforced when the buyer commits")
	}
	if c.OpenTTL <= 0 {
		return errors.New("trade-open-ttl must be positive: a proposed trade with no deadline holds its buyer's reservation forever, which means the daily spending cap cannot be enforced")
	}
	// Offers are discoverable listings. With no deadline a seller that has
	// stopped answering stays on the board indefinitely, which is gap 4: a buyer
	// has no way to tell a live listing from an abandoned one.
	if c.OfferTTL <= 0 {
		return errors.New("offer-ttl must be positive: an offer with no deadline stays discoverable after its seller has stopped answering")
	}
	// Zero would disable the sweep, which leaves an accepted trade that nobody
	// acts on holding its buyer's budget until someone notices. That is a weaker
	// guarantee than the operator configured, reached by setting an interval to
	// nothing, so it is refused rather than treated as a request to turn the sweep
	// off.
	if c.ExpirySweepEvery <= 0 {
		return errors.New("trade-expiry-sweep-interval must be positive: a sweep that never runs leaves an expired trade holding its buyer's budget")
	}
	if c.RequestTimeout <= 0 {
		return errors.New("request-timeout must be positive")
	}
	if c.RateLimit.AgentRPS < 0 || c.RateLimit.IPRPS < 0 {
		return errors.New("rate-limit rps must not be negative")
	}
	if c.RateLimit.AgentBurst < 0 || c.RateLimit.IPBurst < 0 {
		return errors.New("rate-limit burst must not be negative")
	}
	// A rate with no tokens is a limit that never allows anything, which is not
	// what an operator setting a rate means. A burst with no rate is a fixed
	// quota rather than a rate, which is a deliberate configuration and allowed.
	if c.RateLimit.AgentRPS > 0 && c.RateLimit.AgentBurst == 0 {
		return errors.New("rate-limit-agent-burst must be positive when rate-limit-agent-rps is set")
	}
	if c.RateLimit.IPRPS > 0 && c.RateLimit.IPBurst == 0 {
		return errors.New("rate-limit-ip-burst must be positive when rate-limit-ip-rps is set")
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
