package engine

import (
	"fmt"

	"pennyharvester/backend/internal/market"
)

// requireConfiguration refuses money-moving operations while the engine knows
// the runtime configuration diverged from what is stored (a failed apply or a
// failed rollback). Restarting or a successful configuration save clears it.
func (e *Engine) requireConfiguration() error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.systemError == "" {
		return nil
	}
	return fmt.Errorf("%w: 运行时配置状态未确认，已阻止交易：%s", ErrInvalidOrder, e.systemError)
}

// unresolvedOrderExposure is the risk already promised to the venue but not yet
// confirmed in the local ledger. Conflict flags are side-agnostic: even a
// pending sell means the position's shares are in motion and a new entry in the
// same market or event must wait for reconciliation.
type unresolvedOrderExposure struct {
	market, event, total float64
	marketConflict       bool
	eventConflict        bool
}

func unresolvedOrderRisk(orders []Order, venue market.Venue, marketID, eventID string) unresolvedOrderExposure {
	key := marketKey(venue, marketID)
	event := eventKey(venue, eventID)
	var risk unresolvedOrderExposure
	for _, order := range orders {
		orderVenue := order.EffectiveVenue()
		if marketKey(orderVenue, order.MarketID) == key {
			risk.marketConflict = true
		}
		orderEvent := eventKey(orderVenue, order.EventID)
		if event != "" && orderEvent == event {
			risk.eventConflict = true
		}
		if order.Type != "buy" {
			continue
		}
		amount := order.Amount + order.Fee
		risk.total += amount
		if marketKey(orderVenue, order.MarketID) == key {
			risk.market += amount
		}
		if event != "" && orderEvent == event {
			risk.event += amount
		}
	}
	return risk
}
