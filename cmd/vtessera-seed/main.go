// vtessera-seed keeps an empty marketplace stocked with open offers.
//
// It is a separate binary from the service on purpose: the seeder holds the
// Ed25519 identities of the agents it publishes, and the service must never hold
// an agent key. It talks to the marketplace over the public HTTP API, so it is
// exactly as privileged as any other agent.
//
// One pass and exit:
//
//	vtessera-seed --base-url https://vtessera.fly.dev --once
//
// Keep it running, refreshing before each offer TTL lapses:
//
//	vtessera-seed --base-url https://vtessera.fly.dev --refresh 12h
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/douglasdemaio/vtessera/internal/seed"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAILED: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("vtessera-seed", flag.ContinueOnError)
	baseURL := fs.String("base-url", envOr("VTESSERA_BASE_URL", "http://localhost:8080"), "marketplace to seed")
	keyring := fs.String("keyring", envOr("VTESSERA_SEED_KEYRING", "seed-keyring.json"), "path to the seed keyring, created on first use")
	catalog := fs.String("catalog", os.Getenv("VTESSERA_SEED_CATALOG"), "JSON file of listings; empty uses the built-in catalog")
	refresh := fs.Duration("refresh", 12*time.Hour, "how often to republish offers; must be shorter than the marketplace offer TTL")
	timeout := fs.Duration("timeout", 15*time.Second, "how long one marketplace call may take")
	once := fs.Bool("once", false, "publish one pass and exit")
	closeOnExit := fs.Bool("close-on-exit", false, "withdraw the offers this run opened when it stops")
	allowMainnet := fs.Bool("allow-mainnet", false, "acknowledge that the target settles on mainnet-beta, where real value can move")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	listings, err := seed.LoadCatalog(*catalog)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	seeder, err := seed.New(seed.Config{
		BaseURL:      *baseURL,
		KeyringPath:  *keyring,
		Refresh:      *refresh,
		Timeout:      *timeout,
		Catalog:      listings,
		CloseOnExit:  *closeOnExit,
		AllowMainnet: *allowMainnet,
		Logger:       logger,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		return seeder.Once(ctx)
	}
	return seeder.Run(ctx)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
