package engine

import (
	"context"
	"fmt"

	"pennyharvester/backend/internal/market"
)

// classifyOpportunities enriches candidates with their category and sports
// risk flags, then applies the configured sports policy. Gamma normally
// includes tags in the market payload; a batched event-tag fallback is used
// only when a provider omits them. Local heuristics still identify game markets
// through their start time and title if tags are unavailable.
func (e *Engine) classifyOpportunities(ctx context.Context, opportunities []Opportunity, markets map[string]market.Market, settings Settings) []Opportunity {
	tagsByEvent := map[string][]market.Tag{}
	if provider, ok := e.provider.(market.EventTagProvider); ok {
		eventIDs := make([]string, 0, len(opportunities))
		seen := make(map[string]bool, len(opportunities))
		for _, opportunity := range opportunities {
			item := markets[opportunity.MarketID]
			if len(item.Tags) > 0 {
				continue
			}
			eventID := item.EventID
			if eventID == "" || seen[eventID] {
				continue
			}
			seen[eventID] = true
			eventIDs = append(eventIDs, eventID)
		}
		if len(eventIDs) > 0 {
			if tags, err := provider.EventTags(ctx, eventIDs); err == nil {
				tagsByEvent = tags
			}
		}
	}

	filtered := make([]Opportunity, 0, len(opportunities))
	for _, opportunity := range opportunities {
		item := markets[opportunity.MarketID]
		tags := item.Tags
		if len(tags) == 0 {
			tags = tagsByEvent[item.EventID]
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
		if provider, ok := e.provider.(market.EventTagProvider); ok && item.EventID != "" {
			if byEvent, err := provider.EventTags(ctx, []string{item.EventID}); err == nil {
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
