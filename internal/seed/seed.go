// Package seed keeps an empty marketplace stocked with open offers.
//
// A directory with no offers is a marketplace an agent cannot route to, so the
// first agent to arrive finds nothing and leaves. This package publishes a
// configurable spread of seller listings and refreshes them before the
// marketplace's offer TTL lapses, so discovery keeps answering with something
// real. It is a client of the public HTTP API and holds no key the marketplace
// trusts: it authenticates with its own Ed25519 identities exactly as any agent
// does.
//
// It does not fulfil trades. The marketplace has no endpoint a seller can poll
// for incoming work, so a seeded listing is a discovery signal, not a worker;
// who answers a routed trade is settled off-band, the same as any AGP route.
package seed

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/mr-tron/base58"
)

// version is the card version the seeded agents advertise. It is the seeder's
// own version, not the marketplace's.
const version = "0.1.0"

// Config is one seeding run.
type Config struct {
	BaseURL     string
	KeyringPath string
	Refresh     time.Duration
	Timeout     time.Duration
	Catalog     []Listing
	CloseOnExit bool
	// AllowMainnet acknowledges that the target settles real value. Without it,
	// a run that finds cluster=mainnet-beta at /healthz refuses to publish, so a
	// sandbox target cannot be swapped for mainnet by changing one URL.
	AllowMainnet bool
	Logger       *slog.Logger
}

// Seeder publishes and refreshes the catalog against one marketplace.
type Seeder struct {
	cfg    Config
	client *Client
	keys   *Keyring
	log    *slog.Logger

	mu   sync.Mutex
	open map[string]string
}

// New prepares a seeder.
func New(cfg Config) (*Seeder, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("base URL is required")
	}
	if cfg.KeyringPath == "" {
		return nil, errors.New("keyring path is required")
	}
	if cfg.Refresh <= 0 {
		return nil, errors.New("refresh interval must be positive")
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("timeout must be positive")
	}
	if len(cfg.Catalog) == 0 {
		return nil, errors.New("catalog lists no agents, so there is nothing to seed")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	keys, err := OpenKeyring(cfg.KeyringPath)
	if err != nil {
		return nil, err
	}
	return &Seeder{cfg: cfg, client: NewClient(cfg.BaseURL, cfg.Timeout), keys: keys, log: log, open: map[string]string{}}, nil
}

// Run publishes the catalog once, then refreshes it on the configured interval
// until the context is cancelled. A failed first pass ends the process, because
// a mistyped URL or a refused price should be seen at once; a failed later
// refresh is logged and retried, because a marketplace blip should not stop the
// process that is keeping the board stocked.
func (s *Seeder) Run(ctx context.Context) error {
	health, err := s.health(ctx)
	if err != nil {
		return err
	}
	s.log.Info("seeding offers",
		"marketplace", s.cfg.BaseURL,
		"sandbox", health.Sandbox,
		"cluster", health.Cluster,
		"verificationKey", health.VerificationKey,
		"listings", len(s.cfg.Catalog),
		"refresh", s.cfg.Refresh.String(),
	)
	if err := s.Pass(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.cfg.Refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if s.cfg.CloseOnExit {
				s.closeAll(context.WithoutCancel(ctx))
			}
			return nil
		case <-ticker.C:
			if err := s.Pass(ctx); err != nil {
				s.log.Error("offer refresh failed", "error", err)
			}
		}
	}
}

// Pass publishes one offer per listing and closes the one it published for that
// listing last time, so a refresh replaces a listing rather than piling a second
// one beside it. It attempts every listing even when one fails, so the error
// names all the listings that broke instead of only the first.
func (s *Seeder) Pass(ctx context.Context) error {
	var failures []string
	for _, listing := range s.cfg.Catalog {
		if err := s.publish(ctx, listing); err != nil {
			s.log.Error("listing did not publish", "listing", listing.Name, "error", err)
			failures = append(failures, fmt.Sprintf("%s: %v", listing.Name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d listings did not publish: %s", len(failures), len(s.cfg.Catalog), strings.Join(failures, "; "))
	}
	return nil
}

func (s *Seeder) publish(ctx context.Context, listing Listing) error {
	key, err := s.keys.Key(listing.Name)
	if err != nil {
		return err
	}
	agentID := base58.Encode(key.Public().(ed25519.PublicKey))
	token, err := s.client.Authenticate(ctx, agentID, key)
	if err != nil {
		return fmt.Errorf("authenticate %s: %w", agentID, err)
	}
	card := domain.AgentCard{
		Name:            listing.Name,
		Description:     listing.Description,
		Version:         version,
		URL:             listing.url(),
		PublicKey:       agentID,
		Capabilities:    listing.Capabilities,
		Currencies:      []string{listing.mint()},
		SettlementModes: listing.modes(),
	}
	if err := s.client.PutCard(ctx, agentID, token, card); err != nil {
		return fmt.Errorf("put card for %s: %w", agentID, err)
	}
	offer, err := s.client.PublishOffer(ctx, agentID, token, OfferRequest{
		Direction:       domain.DirectionAsk,
		Description:     listing.Offer,
		Capabilities:    listing.Capabilities,
		PriceAmount:     listing.PriceAmount,
		PriceMint:       listing.mint(),
		SettlementModes: listing.modes(),
	})
	if err != nil {
		return fmt.Errorf("publish offer for %s: %w", agentID, err)
	}
	// The marketplace sets the TTL, and an agent cannot read it before it has an
	// offer to read it from. The first offer is where the seeder learns whether
	// its refresh interval actually beats the TTL; a listing that would lapse
	// between refreshes is a failure, not a slow success.
	if ttl := offer.ExpiresAt.Sub(offer.CreatedAt); ttl > 0 && s.cfg.Refresh >= ttl {
		return fmt.Errorf("refresh interval %s is not shorter than the marketplace offer TTL %s, so %s would lapse before it is refreshed", s.cfg.Refresh, ttl, listing.Name)
	}
	if previous, ok := s.replaceOpen(listing.Name, offer.ID); ok && previous != offer.ID {
		if err := s.client.CloseOffer(ctx, token, previous); err != nil {
			s.log.Warn("could not close the offer being replaced", "listing", listing.Name, "offer", previous, "error", err)
		}
	}
	s.log.Info("listing published",
		"listing", listing.Name,
		"agent", agentID,
		"offer", offer.ID,
		"expiresAt", offer.ExpiresAt,
	)
	return nil
}

// closeAll withdraws the board the process opened, so a stopped seeder does not
// leave listings a buyer cannot tell from a seller that has gone quiet.
func (s *Seeder) closeAll(ctx context.Context) {
	for _, listing := range s.cfg.Catalog {
		offerID, ok := s.openOffer(listing.Name)
		if !ok || offerID == "" {
			continue
		}
		key, err := s.keys.Key(listing.Name)
		if err != nil {
			s.log.Warn("cannot close offer", "listing", listing.Name, "error", err)
			continue
		}
		agentID := base58.Encode(key.Public().(ed25519.PublicKey))
		token, err := s.client.Authenticate(ctx, agentID, key)
		if err != nil {
			s.log.Warn("cannot close offer", "listing", listing.Name, "error", err)
			continue
		}
		if err := s.client.CloseOffer(ctx, token, offerID); err != nil {
			s.log.Warn("cannot close offer", "listing", listing.Name, "offer", offerID, "error", err)
		}
	}
}

// replaceOpen records the offer now live for a listing and returns the one it
// supersedes.
func (s *Seeder) replaceOpen(name, id string) (previous string, had bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, had = s.open[name]
	s.open[name] = id
	return previous, had
}

func (s *Seeder) openOffer(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.open[name]
	return id, ok
}

// Once checks the marketplace and publishes one pass, for scripted seeding where
// leaving a process running is not wanted.
func (s *Seeder) Once(ctx context.Context) error {
	if _, err := s.health(ctx); err != nil {
		return err
	}
	return s.Pass(ctx)
}

// health reads the marketplace identity and refuses a mainnet target nobody
// acknowledged, so a seeder meant for the sandbox cannot advertise real supply on
// the live board through a mistyped URL. It is the same guard the service puts on
// settlement, applied to the thing that seeds it.
func (s *Seeder) health(ctx context.Context) (Health, error) {
	health, err := s.client.Health(ctx)
	if err != nil {
		return Health{}, fmt.Errorf("marketplace health: %w", err)
	}
	if health.Cluster == "mainnet-beta" && !s.cfg.AllowMainnet {
		return Health{}, fmt.Errorf("%s is cluster %q; seeding it advertises sellers on the live board. Pass --allow-mainnet to acknowledge that", s.cfg.BaseURL, health.Cluster)
	}
	return health, nil
}
