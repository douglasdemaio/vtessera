package registry

import (
	"context"

	"github.com/douglasdemaio/vtessera/internal/agp"
	"github.com/douglasdemaio/vtessera/internal/domain"
)

func (a *AnnouncementSource) Announcements(ctx context.Context) ([]agp.CapabilityAnnouncement, error) {
	offers, err := a.store.ListOpenOffers(ctx)
	if err != nil {
		return nil, err
	}
	now := a.now().UTC()

	ids := make([]string, 0, len(offers))
	seen := map[string]bool{}
	for _, offer := range offers {
		if !seen[offer.AgentID] {
			seen[offer.AgentID] = true
			ids = append(ids, offer.AgentID)
		}
	}
	agents, err := a.store.ListAgentsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]agp.CapabilityAnnouncement, 0, len(offers))
	for _, offer := range offers {
		agent, ok := agents[offer.AgentID]
		if !ok || agent.Status != domain.AgentActive {
			continue
		}
		out = append(out, agp.AnnouncementsForOffer(offer, agent, a.mints.mintInfo(offer.PriceMint), now)...)
	}
	return out, nil
}
