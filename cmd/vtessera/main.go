package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/douglasdemaio/vtessera/internal/auth"
	"github.com/douglasdemaio/vtessera/internal/config"
	"github.com/douglasdemaio/vtessera/internal/httpapi"
	"github.com/douglasdemaio/vtessera/internal/ledger"
	"github.com/douglasdemaio/vtessera/internal/preflight"
	"github.com/douglasdemaio/vtessera/internal/registry"
	"github.com/douglasdemaio/vtessera/internal/settlement"
	"github.com/douglasdemaio/vtessera/internal/store"
	"github.com/douglasdemaio/vtessera/internal/tokens"
	"github.com/douglasdemaio/vtessera/internal/trade"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, config.ErrHelp()) {
			os.Exit(0)
		}
		slog.Error("vtessera exited", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Parse(args)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Preflight runs before anything is opened or created. Its whole purpose is
	// to answer "is this endpoint the cluster that was declared", and a check
	// that first creates a signing key and a database cannot be run against a
	// host you are only inspecting: --preflight-only exists to be pointed at a
	// cluster before a deploy depends on it, not to provision one.
	var report preflight.Report
	if cfg.SettlementEnabled() {
		var err error
		if report, err = runPreflight(ctx, cfg); err != nil {
			return err
		}
		if cfg.PreflightOnly {
			printPreflight(report)
			return nil
		}
	}
	preflightGenesis := report.GenesisHash

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := os.MkdirAll(filepath.Dir(cfg.SignerKeyPath), 0o700); err != nil {
		return err
	}
	signer, created, err := ledger.LoadOrCreateSigner(cfg.SignerKeyPath)
	if err != nil {
		return err
	}
	logger.Info("marketplace signing key ready",
		"verificationKey", signer.PublicKeyBase58(),
		"generated", created,
		"path", cfg.SignerKeyPath,
	)

	authSvc, err := auth.New(db, cfg.SessionSecret,
		auth.WithChallengeTTL(cfg.ChallengeTTL),
		auth.WithSessionTTL(cfg.SessionTTL),
	)
	if err != nil {
		return err
	}
	// The governed mint set is scoped to a cluster, and there is no cluster when
	// settlement is unconfigured. Passing nil is the honest answer: this process
	// governs no mints, so it declines to check a price against a scale it does
	// not have rather than assuming mainnet's.
	var mints tokens.Registry
	registrySvc := registry.New(db, mints)
	led := ledger.New(db, signer)
	trades := trade.New(db, db, db, led)

	// On-chain settlement is only wired when a cluster and an endpoint are both
	// configured, and only after preflight has confirmed the endpoint is the
	// cluster that was declared. The service never holds an agent key: it builds
	// unsigned transactions and later proves settlement from the chain alone.
	if cfg.SettlementEnabled() {
		policy, err := cfg.FeePolicy()
		if err != nil {
			return err
		}
		mints, err = tokens.ForCluster(cfg.Solana.Cluster, cfg.Solana.LocalnetMints...)
		if err != nil {
			return err
		}
		// The registry governs the same set preflight checks, so an agent can
		// never advertise a currency the service would refuse to settle in.
		registrySvc = registry.New(db, mints)

		client := settlement.NewRPCClient(cfg.Solana.RPCURL)
		logger.Info("preflight passed",
			"cluster", report.Cluster,
			"genesisHash", report.GenesisHash,
			"mints", len(report.Mints),
			"feeWallet", report.FeeWallet,
			"feeBalance", report.FeeBalance,
			"rentMinimum", report.RentMinimum,
		)

		trades.WithSettlement(trade.SettlementDeps{
			Registry: mints,
			Policy:   policy,
			Builder:  settlement.NewBuilder(client).SetTTL(cfg.Solana.BlockhashTTL),
			Verifier: settlement.NewVerifier(),
			Chain:    client,
			Store:    db,
			Cluster:  cfg.Solana.Cluster,
			Genesis:  client,
			Mints: settlement.NewMintVerifier(client, cfg.Solana.Cluster).
				// Settlement keeps working through an authority rotation; the
				// operator has to hear about it. A drift is a governance change
				// on a token the marketplace is pricing, not a warning to log at
				// a level nobody reads.
				OnDrift(func(drift string) {
					logger.Warn("governed mint authority drifted from its pin",
						"cluster", cfg.Solana.Cluster, "drift", drift)
				}),
			TradeList: db,
		})
		logger.Info("on-chain settlement enabled",
			"cluster", cfg.Solana.Cluster,
			"rpcUrl", cfg.Solana.RPCURL,
			"feeLamports", policy.Lamports(),
			"feeWallet", policy.WalletAddress(),
			"blockhashTtl", cfg.Solana.BlockhashTTL.String(),
		)
		// A signature that was not visible when the buyer confirmed is settled
		// here, so an RPC hiccup at the wrong moment cannot strand a trade.
		go func() {
			trades.RunReconciler(ctx, trade.DefaultReconcilePolicy())
			logger.Info("settlement reconciler stopped")
		}()
	} else {
		logger.Warn("on-chain settlement disabled: no cluster or RPC endpoint, on-chain trades are refused")
	}

	if cfg.PublicBaseURL == "" {
		logger.Warn("no public base URL configured: the agent card omits its url, so agents cannot discover where to reach this gateway")
	}

	api := httpapi.New(httpapi.Options{
		Registry:      registrySvc,
		Trades:        trades,
		Auth:          authSvc,
		Ledger:        led,
		Tokens:        mints,
		Version:       cfg.Version,
		PublicBaseURL: cfg.PublicBaseURL,
		Cluster:       cfg.Solana.Cluster,
		GenesisHash:   preflightGenesis,
	})
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           timeout(cfg.RequestTimeout, api),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("vtessera listening",
			"addr", cfg.Addr,
			"agp", "agp/route_intent",
			"agentCard", "GET /.well-known/agent-card.json",
			"settlement", cfg.SettlementEnabled(),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down", "grace", cfg.ShutdownGrace.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		logger.Info("stopped cleanly")
		return nil
	}
}

func timeout(d time.Duration, next http.Handler) http.Handler {
	return http.TimeoutHandler(next, d, `{"error":"request timed out","code":"TIMEOUT"}`)
}

// runPreflight confirms the endpoint is the cluster that was declared and that
// every governed mint is the token it claims to be.
//
// It is deliberately the first thing that happens, before the database is opened
// and before a signing key exists, so that --preflight-only can be pointed at a
// cluster to inspect it without provisioning anything on that host.
func runPreflight(ctx context.Context, cfg config.Config) (preflight.Report, error) {
	policy, err := cfg.FeePolicy()
	if err != nil {
		return preflight.Report{}, err
	}
	mints, err := tokens.ForCluster(cfg.Solana.Cluster, cfg.Solana.LocalnetMints...)
	if err != nil {
		return preflight.Report{}, err
	}
	report, err := preflight.Check(ctx, preflight.Deps{
		Cluster:    cfg.Solana.Cluster,
		Endpoint:   cfg.Solana.RPCURL,
		RPC:        settlement.NewRPCClient(cfg.Solana.RPCURL),
		Mints:      mints.List(),
		FeePolicy:  policy,
		AllowHosts: cfg.Solana.LocalnetAllowHost,
	})
	if err != nil {
		// Fail closed. A configured endpoint that is not the configured cluster,
		// or a governed mint that is not the token it claims to be, is a reason
		// not to serve rather than a warning to log and ignore.
		return preflight.Report{}, fmt.Errorf("preflight failed, refusing to serve: %w", err)
	}
	return report, nil
}

// printPreflight writes the report a human reads before deciding to deploy, one
// value per line rather than a struct dump.
func printPreflight(report preflight.Report) {
	fmt.Printf("preflight ok\n")
	fmt.Printf("  cluster      %s\n", report.Cluster)
	fmt.Printf("  genesis      %s\n", report.GenesisHash)
	fmt.Printf("  mints        %d governed\n", len(report.Mints))
	for _, mint := range report.Mints {
		note := "verified"
		if !mint.AuthoritiesMatch {
			note = "AUTHORITY DRIFT: " + mint.Drift
		}
		fmt.Printf("    %-44s %-6s %s\n", mint.Address, mint.Symbol, note)
	}
	fmt.Printf("  fee wallet   %s\n", report.FeeWallet)
	fmt.Printf("  fee balance  %d lamports\n", report.FeeBalance)
	fmt.Printf("  rent minimum %d lamports\n", report.RentMinimum)
}
