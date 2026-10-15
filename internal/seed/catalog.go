package seed

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/douglasdemaio/vtessera/internal/domain"
)

// usdc is Circle's Solana USDC. It is the one mint governed on both public
// clusters this seeder targets, so a listing priced in it is tradeable on
// either. The mint is a term the buyer can verify; it is not configuration.
const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// defaultCardURL is where a seeded listing points when its catalog entry names
// no url. A card without a URL is refused by the schema, and a seeded listing
// has no service of its own to name, so it points at the marketplace it is
// listed on. An operator with a real seller sets this per listing.
const defaultCardURL = "https://vtessera.fly.dev/"

// Listing is one seeded seller: an agent, its card, and the offer it keeps open.
// The fields map to the published card and offer rather than to the request
// bodies, so a catalog file reads as the listing an operator wants on the board.
type Listing struct {
	Name            string                  `json:"name"`
	Description     string                  `json:"description"`
	URL             string                  `json:"url,omitempty"`
	Capabilities    []string                `json:"capabilities"`
	Offer           string                  `json:"offer"`
	PriceAmount     string                  `json:"priceAmount"`
	PriceMint       string                  `json:"priceMint,omitempty"`
	SettlementModes []domain.SettlementMode `json:"settlementModes,omitempty"`
}

func (l Listing) mint() string {
	if l.PriceMint != "" {
		return l.PriceMint
	}
	return usdc
}

func (l Listing) url() string {
	if l.URL != "" {
		return l.URL
	}
	return defaultCardURL
}

func (l Listing) modes() []domain.SettlementMode {
	if len(l.SettlementModes) > 0 {
		return l.SettlementModes
	}
	return []domain.SettlementMode{domain.SettlementOffchain}
}

// LoadCatalog reads a catalog file, or returns the built-in catalog when path is
// empty. A present-but-empty catalog is refused: a seeder that registers nothing
// while reporting success is the failure this tool exists to prevent.
func LoadCatalog(path string) ([]Listing, error) {
	if path == "" {
		return DefaultCatalog(), nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read seed catalog %s: %w", path, err)
	}
	var listings []Listing
	if err := json.Unmarshal(body, &listings); err != nil {
		return nil, fmt.Errorf("decode seed catalog %s: %w", path, err)
	}
	if len(listings) == 0 {
		return nil, fmt.Errorf("seed catalog %s lists no agents", path)
	}
	for _, listing := range listings {
		if listing.Name == "" {
			return nil, fmt.Errorf("seed catalog %s has an entry with no name", path)
		}
		if listing.Offer == "" {
			return nil, fmt.Errorf("seed catalog %s: listing %q has no offer description", path, listing.Name)
		}
		if listing.PriceAmount == "" {
			return nil, fmt.Errorf("seed catalog %s: listing %q has no priceAmount", path, listing.Name)
		}
	}
	return listings, nil
}

// DefaultCatalog is the built-in spread of capabilities an empty marketplace is
// missing. Each is off-chain and priced at or under a dollar, so a buyer can
// exercise discovery and a full trade without approaching a spending cap. These
// are reference listings published by the operator: they make discovery answer
// with something real rather than empty, and who fulfils a routed trade is
// settled off-band, exactly as an AGP route is.
func DefaultCatalog() []Listing {
	return []Listing{
		{
			Name:         "vtessera-seed-summarize",
			Description:  "Summarizes a document into a short, faithful abstract.",
			Capabilities: []string{"summarize:document"},
			Offer:        "Condense a text document into a short summary.",
			PriceAmount:  "0.25",
		},
		{
			Name:         "vtessera-seed-translate",
			Description:  "Translates short text between natural languages.",
			Capabilities: []string{"translate:text"},
			Offer:        "Translate a short text between two languages.",
			PriceAmount:  "0.50",
		},
		{
			Name:         "vtessera-seed-extract",
			Description:  "Extracts structured entities from unstructured text.",
			Capabilities: []string{"extract:entities"},
			Offer:        "Extract people, organisations and places from a text.",
			PriceAmount:  "0.50",
		},
		{
			Name:         "vtessera-seed-research",
			Description:  "Answers a research question with a short, sourced write-up.",
			Capabilities: []string{"research:topic"},
			Offer:        "Research a topic and return a short, sourced brief.",
			PriceAmount:  "1.00",
		},
		{
			Name:         "vtessera-seed-code-review",
			Description:  "Reviews a code change and reports defects and risks.",
			Capabilities: []string{"code:review"},
			Offer:        "Review a patch and report defects.",
			PriceAmount:  "0.50",
		},
		// code:explain is deliberately absent. The standing seller
		// (vtessera-seller) publishes it with a real, auto-accepting endpoint,
		// and a seeded listing here would shadow the one listing a buyer can
		// actually route to and finish.
		{
			Name:         "vtessera-seed-transcribe",
			Description:  "Transcribes a short audio clip to text.",
			Capabilities: []string{"transcribe:audio"},
			Offer:        "Transcribe a short audio clip.",
			PriceAmount:  "0.75",
		},
		{
			Name:         "vtessera-seed-classify",
			Description:  "Classifies text into the caller's categories.",
			Capabilities: []string{"classify:text"},
			Offer:        "Classify a piece of text into supplied categories.",
			PriceAmount:  "0.10",
		},
	}
}
