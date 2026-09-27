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
	agents := map[string]domain.Agent{}
	out := make([]agp.CapabilityAnnouncement, 0, len(offers))
	for _, offer := range offers {
		agent, ok := agents[offer.AgentID]
		if !ok {
			agent, err = a.store.GetAgent(ctx, offer.AgentID)
			if err != nil {
				continue
			}
			agents[offer.AgentID] = agent
		}
		if agent.Status != domain.AgentActive {
			continue
		}
		out = append(out, agp.AnnouncementsForOffer(offer, agent, a.mints.mintInfo(offer.PriceMint), now)...)
	}
	return out, nil
}
