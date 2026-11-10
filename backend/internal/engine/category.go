package engine

import (
	"context"
	"fmt"
	"time"

	"pennyharvester/backend/internal/market"
)

// eventTagTimeout bounds the fallback tag lookup. Tags are already included in
// the market payload for Polymarket; this path only runs when a venue omits
// them, and it must not consume the scan's trading budget.
const eventTagTimeout = 2 * time.Second

// classifyOpportunities enriches candidates with their category and sports
// risk flags, then applies the configured sports policy. A venue normally
// includes tags in the market payload; a batched event-tag fallback is used
// only when that venue's provider omits them. Local heuristics still identify
// game markets through their start time and title if tags are unavailable.
//
// markets is keyed by marketKey so two venues' ids can never collide, and the
// tag fallback is resolved through the provider that owns each opportunity.
func (e *Engine) classifyOpportunities(ctx context.Context, opportunities []Opportunity, markets map[string]market.Market, settings Settings) []Opportunity {
	eventIDsByVenue := map[market.Venue][]string{}
	seenByVenue := map[market.Venue]map[string]bool{}
	for _, opportunity := range opportunities {
		v := opportunity.EffectiveVenue()
		item := markets[marketKey(v, opportunity.MarketID)]
		if len(item.Tags) > 0 || item.EventID == "" {
			continue
		}
		if seenByVenue[v] == nil {
			seenByVenue[v] = map[string]bool{}
		}
		if seenByVenue[v][item.EventID] {
			continue
		}
		seenByVenue[v][item.EventID] = true
		eventIDsByVenue[v] = append(eventIDsByVenue[v], item.EventID)
	}
	tagsByVenue := map[market.Venue]map[string][]market.Tag{}
	for v, eventIDs := range eventIDsByVenue {
		provider, ok := e.data(v).(market.EventTagProvider)
		if !ok || provider == nil || len(eventIDs) == 0 {
			continue
		}
		tagCtx, cancel := context.WithTimeout(ctx, eventTagTimeout)
		tags, err := provider.EventTags(tagCtx, eventIDs)
		cancel()
		if err == nil {
			tagsByVenue[v] = tags
		}
	}

	filtered := make([]Opportunity, 0, len(opportunities))
	for _, opportunity := range opportunities {
		v := opportunity.EffectiveVenue()
		item := markets[marketKey(v, opportunity.MarketID)]
		tags := item.Tags
		if len(tags) == 0 {
			tags = tagsByVenue[v][item.EventID]
		}
		classification := market.Classify(item, tags)
		opportunity.Category = string(classification.Category)
		opportunity.SportsGame = classification.SportsGame
		opportunity.OvertimeRisk = classification.OvertimeRisk
		if sportsPolicyExcludes(classification, settings.SportsPolicy) {
			continue
		}
		filtered = append(filtered, opportunity)
	}
	return filtered
}

func sportsPolicyExcludes(classification market.Classification, policy string) bool {
	switch policy {
	case SportsPolicyExcludeAll:
		return classification.Category == market.CategorySports
	case SportsPolicyExcludeGames:
		return classification.SportsGame
	default:
		return false
	}
}

func (e *Engine) classifyMarket(ctx context.Context, item market.Market) market.Classification {
	tags := item.Tags
	if len(tags) == 0 {
		if provider, ok := e.data(item.EffectiveVenue()).(market.EventTagProvider); ok && item.EventID != "" {
			tagCtx, cancel := context.WithTimeout(ctx, eventTagTimeout)
			byEvent, err := provider.EventTags(tagCtx, []string{item.EventID})
			cancel()
			if err == nil {
				tags = byEvent[item.EventID]
			}
		}
	}
	return market.Classify(item, tags)
}

func enforceSportsPolicy(classification market.Classification, policy string) error {
	if !sportsPolicyExcludes(classification, policy) {
		return nil
	}
	if classification.SportsGame {
		return fmt.Errorf("%w: 体育赛事盘口可能包含加时/延长赛，已在参数设置中排除", ErrInvalidOrder)
	}
	return fmt.Errorf("%w: 体育分类已在参数设置中排除", ErrInvalidOrder)
}
