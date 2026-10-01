package agp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/douglasdemaio/vtessera/internal/domain"
	"github.com/douglasdemaio/vtessera/internal/money"
)

const defaultCapabilityDomain = "general"

const squadPrefix = "Squad_Marketplace"

type CostFacts struct {
	Amount   string `json:"amount"`
	Mint     string `json:"mint"`
	Decimals int    `json:"decimals"`
	Units    uint64 `json:"units"`
}

type CapabilityAnnouncement struct {
	Capability string         `json:"capability"`
	Version    string         `json:"version"`
	Cost       float64        `json:"cost"`
	Policy     map[string]any `json:"policy"`

	CostAmount     string `json:"cost_amount,omitempty"`
	CostMint       string `json:"cost_mint,omitempty"`
	CostDecimals   int    `json:"cost_decimals,omitempty"`
	SquadPath      string `json:"path"`
	OfferID        string `json:"offer_id,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	AnnouncedAt    string `json:"announced_at,omitempty"`
	AnnouncementID string `json:"announcement_id,omitempty"`
}

func (a CapabilityAnnouncement) Validate() error {
	if err := ValidateCapability(a.Capability); err != nil {
		return err
	}
	if strings.TrimSpace(a.Version) == "" {
		return fmt.Errorf("%w: announcement version is required", ErrInvalidIntent)
	}
	if strings.TrimSpace(a.SquadPath) == "" {
		return fmt.Errorf("%w: announcement path is required", ErrInvalidIntent)
	}
	if a.CostAmount != "" {
		if _, err := money.Parse(a.CostAmount); err != nil {
			return fmt.Errorf("%w: costAmount %q: %v", ErrInvalidIntent, a.CostAmount, err)
		}
		if a.CostMint == "" {
			return fmt.Errorf("%w: costAmount requires costMint", ErrInvalidIntent)
		}
	}
	return nil
}

func EntryFor(a CapabilityAnnouncement) (RouteEntry, error) {
	if err := a.Validate(); err != nil {
		return RouteEntry{}, err
	}
	cost := a.Cost
	if a.CostAmount != "" {
		parsed, err := strconv.ParseFloat(a.CostAmount, 64)
		if err != nil {
			return RouteEntry{}, fmt.Errorf("%w: costAmount %q is not representable: %v", ErrInvalidIntent, a.CostAmount, err)
		}
		cost = parsed
	}
	policy := a.Policy
	if policy == nil {
		policy = map[string]any{}
	}
	announcement := a
	entry := RouteEntry{
		Path:         a.SquadPath,
		Cost:         cost,
		Policy:       policy,
		CostAmount:   a.CostAmount,
		CostMint:     a.CostMint,
		OfferID:      a.OfferID,
		AgentID:      a.AgentID,
		Direction:    directionOf(policy),
		Announcement: &announcement,
	}
	if a.CostAmount != "" {
		if parsed, err := money.Parse(a.CostAmount); err == nil {
			entry.parsedCost = parsed
			entry.hasParsedCost = true
		}
	}
	return entry, nil
}

func SquadPath(agentID, offerID string) string {
	return fmt.Sprintf("%s/%s/offer/%s", squadPrefix, agentID, offerID)
}

func directionOf(policy map[string]any) string {
	if v, ok := policy[PolicyDirection]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func latestAnnouncement(announcements []CapabilityAnnouncement) string {
	latest := ""
	for _, a := range announcements {
		if a.AnnouncedAt > latest {
			latest = a.AnnouncedAt
		}
	}
	return latest
}

func CapabilitiesForOffer(o domain.Offer) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(o.Capabilities)+1)
	add := func(capability string) {
		normalized := NormalizeCapability(capability)
		if normalized == "" || seen[normalized] {
			return
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	for _, declared := range o.Capabilities {
		declared = strings.TrimSpace(declared)
		if declared == "" {
			continue
		}
		if strings.Contains(declared, CapabilitySeparator) {
			add(declared)
			continue
		}
		domainSlug := slug(declared)
		if domainSlug == "" {
			continue
		}
		add(domainSlug + CapabilitySeparator + actionSlug(o.Description))
	}
	if len(out) == 0 {
		add(defaultCapabilityDomain + CapabilitySeparator + actionSlug(o.Description))
	}
	return out
}

func AnnouncementsForOffer(o domain.Offer, a domain.Agent, mint domain.MintInfo, now time.Time) []CapabilityAnnouncement {
	modes := make([]string, 0, len(o.SettlementModes))
	for _, m := range o.SettlementModes {
		modes = append(modes, string(m))
	}
	policy := map[string]any{
		PolicySettlementModes: modes,
		PolicyCurrencies:      []string{o.PriceMint},
		PolicyDirection:       string(o.Direction),
		"agent":               a.ID,
	}
	var decimals int
	if mint.Known {
		units, err := o.PriceAmount.BaseUnits(mint.Decimals)
		if err == nil {
			policy[PolicyMint] = mint
			policy[PolicyCost] = CostFacts{Amount: o.PriceAmount.String(), Mint: o.PriceMint, Decimals: mint.Decimals, Units: units}
			decimals = mint.Decimals
		}
	}
	capabilities := CapabilitiesForOffer(o)
	out := make([]CapabilityAnnouncement, 0, len(capabilities))
	for _, capability := range capabilities {
		out = append(out, CapabilityAnnouncement{
			Capability:     capability,
			Version:        cardVersion(a),
			Cost:           costAsFloat(o.PriceAmount),
			Policy:         policy,
			CostAmount:     o.PriceAmount.String(),
			CostMint:       o.PriceMint,
			CostDecimals:   decimals,
			SquadPath:      SquadPath(a.ID, o.ID),
			OfferID:        o.ID,
			AgentID:        a.ID,
			AnnouncedAt:    now.UTC().Format(time.RFC3339),
			AnnouncementID: announcementID(capability, a.ID, o.ID),
		})
	}
	return out
}

func cardVersion(a domain.Agent) string {
	if strings.TrimSpace(a.Card.Version) != "" {
		return a.Card.Version
	}
	return "1.0"
}

func costAsFloat(a money.Amount) float64 {
	f, err := strconv.ParseFloat(a.String(), 64)
	if err != nil {
		return 0
	}
	return f
}

func announcementID(capability, agentID, offerID string) string {
	return "ann_" + sha256Hex(strings.Join([]string{capability, agentID, offerID}, "|"))[:16]
}

func slug(s string) string {
	var b strings.Builder
	previousDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			previousDash = false
		default:
			if !previousDash && b.Len() > 0 {
				b.WriteByte('-')
				previousDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func actionSlug(description string) string {
	words := strings.FieldsFunc(strings.ToLower(description), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	const maxWords = 4
	parts := make([]string, 0, maxWords)
	for _, word := range words {
		if len(parts) == maxWords {
			break
		}
		parts = append(parts, word)
	}
	action := slug(strings.Join(parts, "-"))
	if action == "" {
		return "offer"
	}
	return action
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
