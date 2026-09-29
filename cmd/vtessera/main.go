package main

import (
	"context"
	"errors"
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
	mints := tokens.Default()
	registrySvc := registry.New(db, registry.WithMints(mints))
	led := ledger.New(db, signer)
	trades := trade.New(db, db, db, led)

	// On-chain settlement is only wired when an RPC endpoint is configured. The
	// service never holds an agent key: it builds unsigned transactions and later
	// proves settlement from the chain alone.
	if cfg.SettlementEnabled() {
		policy, err := cfg.FeePolicy()
		if err != nil {
			return err
		}
		client := settlement.NewRPCClient(cfg.Solana.RPCURL)
		trades.WithSettlement(trade.SettlementDeps{
			Registry:  mints,
			Policy:    policy,
			Builder:   settlement.NewBuilder(client).SetTTL(cfg.Solana.BlockhashTTL),
			Verifier:  settlement.NewVerifier(),
			Chain:     client,
			Store:     db,
			TradeList: db,
		})
		logger.Info("on-chain settlement enabled",
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
		logger.Warn("on-chain settlement disabled: no RPC endpoint, on-chain trades are refused")
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
