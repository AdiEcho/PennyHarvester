package market

import (
	"context"
	"strings"
	"time"
)

// Venue identifies the exchange a market, position or order belongs to. It is
// declared here, next to Market, so every package can carry the identity
// without importing the trading layer (which would be an import cycle).
type Venue string

const (
	VenuePolymarket Venue = "polymarket"
	VenuePredictFun Venue = "predictfun"
)

// VenueOrder is the presentation order the API and UI use when listing venues.
var VenueOrder = []Venue{VenuePolymarket, VenuePredictFun}

func (v Venue) Valid() bool {
	switch v {
	case VenuePolymarket, VenuePredictFun:
		return true
	}
	return false
}

// Label is the human-facing venue name.
func (v Venue) Label() string {
	switch v {
	case VenuePolymarket:
		return "Polymarket"
	case VenuePredictFun:
		return "Predict.fun"
	}
	return string(v)
}

// Currency is the settlement asset of a venue. It is deliberately part of the
// data model: balances and PnL from two venues are never summed.
type Currency string

const (
	CurrencyUSDC Currency = "USDC" // Polymarket collateral
	CurrencyUSDT Currency = "USDT" // Predict.fun collateral (BSC)
)

func (c Currency) Valid() bool {
	return c == CurrencyUSDC || c == CurrencyUSDT
}

// Outcome is one side of a binary market. Price is an indicative public-data
// price, not a quote that this application can execute against.
type Outcome struct {
	Name    string  `json:"name"`
	TokenID string  `json:"token_id"`
	Price   float64 `json:"price"`
}

type Market struct {
	ID               string     `json:"id"`
	ConditionID      string     `json:"condition_id"`
	Question         string     `json:"question"`
	GroupItemTitle   string     `json:"group_item_title"`
	EventTitle       string     `json:"event_title"`
	EventID          string     `json:"event_id"`
	Tags             []Tag      `json:"tags,omitempty"`
	Slug             string     `json:"slug"`
	Outcomes         []Outcome  `json:"outcomes"`
	EndDate          time.Time  `json:"end_date"`
	GameStartTime    *time.Time `json:"game_start_time,omitempty"`
	PriceChange24h   *float64   `json:"price_change_24h,omitempty"`
	Liquidity        float64    `json:"liquidity"`
	Volume           float64    `json:"volume"`
	Active           bool       `json:"active"`
	Closed           bool       `json:"closed"`
	ResolutionStatus string     `json:"resolution_status"`
	// Version is Polymarket's market protocol: "v1" for CTF markets, "v2" for
	// Polymarket Protocol V2 markets, whose outcome identifiers are position IDs
	// settled through ExchangeV3. Empty means the upstream did not report it.
	Version string `json:"version,omitempty"`
	// AcceptingOrders mirrors Gamma's acceptingOrders. Nil means "not reported",
	// so providers and tests that cannot supply it are never treated as paused.
	AcceptingOrders *bool `json:"accepting_orders,omitempty"`

	// Venue names the exchange this market lives on. Empty means Polymarket so
	// rows and fixtures written before venues existed keep working.
	Venue Venue `json:"venue,omitempty"`
	// Currency is the venue's settlement asset.
	Currency Currency `json:"currency,omitempty"`
	// TickSize is the smallest price increment the venue accepts (0.01 or
	// 0.001 on Predict.fun, 0.001 on Polymarket's penny band). Zero means the
	// provider did not report it and callers must use their own fallback.
	TickSize float64 `json:"tick_size,omitempty"`
	// MinNotional is the venue's minimum order value in Currency units.
	MinNotional float64 `json:"min_notional,omitempty"`
	// FeeRateBPS is the taker fee rate the venue reports for this market.
	FeeRateBPS float64 `json:"fee_rate_bps,omitempty"`
	// NegRisk reports whether the market belongs to a negative-risk (multi
	// outcome) family. Both venues route those through a different exchange
	// contract or signing domain.
	NegRisk bool `json:"neg_risk,omitempty"`
	// LinkURL is the canonical public page for the market when the provider
	// knows one. URL() falls back to the Polymarket path when it is empty.
	LinkURL string `json:"link_url,omitempty"`
	// Category is the display category the provider can name directly (for
	// example Predict.fun's categorySlug). Empty means the engine derives one
	// from tags and heuristics, which is what Polymarket markets rely on.
	Category string `json:"category,omitempty"`
}

// IsV2 reports whether the market trades on Polymarket Protocol V2. Those
// markets use position IDs, the ExchangeV3 signing domain and pUSD approvals,
// which this build cannot sign for yet, so live orders must refuse them.
func (m Market) IsV2() bool {
	return strings.EqualFold(strings.TrimSpace(m.Version), "v2")
}

// TradeReady reports whether Gamma says the order book accepts new orders. A
// market can stay active and unclosed while its book is paused, so trading
// gates must consult this in addition to Active and Closed.
func (m Market) TradeReady() bool {
	return m.AcceptingOrders == nil || *m.AcceptingOrders
}

func (m Market) URL() string {
	if m.LinkURL != "" {
		return m.LinkURL
	}
	if m.Slug == "" {
		return ""
	}
	if m.EffectiveVenue() == VenuePolymarket {
		return "https://polymarket.com/event/" + m.Slug
	}
	return ""
}

// EffectiveVenue defaults an unset venue to Polymarket. Every row that predates
// multi-venue support was a Polymarket row, so this is the only safe default.
func (m Market) EffectiveVenue() Venue {
	if m.Venue == "" {
		return VenuePolymarket
	}
	return m.Venue
}

// EffectiveCurrency defaults an unset currency to the venue's collateral.
func (m Market) EffectiveCurrency() Currency {
	if m.Currency != "" {
		return m.Currency
	}
	if m.EffectiveVenue() == VenuePredictFun {
		return CurrencyUSDT
	}
	return CurrencyUSDC
}

func (m Market) OutcomeByToken(tokenID string) (Outcome, bool) {
	for _, outcome := range m.Outcomes {
		if outcome.TokenID == tokenID {
			return outcome, true
		}
	}
	return Outcome{}, false
}

// Winner deliberately requires a closed market and a terminal 0/1 price. When
// Gamma supplies a resolution status, it must also say "resolved". This is
// reference-data settlement for paper positions only.
func (m Market) Winner() (Outcome, bool) {
	if !m.finalResolutionData() {
		return Outcome{}, false
	}
	var winner Outcome
	found := false
	for _, outcome := range m.Outcomes {
		if outcome.Price >= 0.999 {
			if found {
				return Outcome{}, false
			}
			winner, found = outcome, true
		} else if outcome.Price > 0.001 {
			return Outcome{}, false
		}
	}
	return winner, found
}

// SettlementPrice also handles binary markets explicitly resolved 50/50, for
// example after a cancellation. A 50/50 result is not a winning YES or NO.
func (m Market) SettlementPrice(tokenID string) (float64, bool) {
	if !m.finalResolutionData() {
		return 0, false
	}
	if _, ok := m.OutcomeByToken(tokenID); !ok {
		return 0, false
	}
	if m.Outcomes[0].Price >= 0.499 && m.Outcomes[0].Price <= 0.501 &&
		m.Outcomes[1].Price >= 0.499 && m.Outcomes[1].Price <= 0.501 {
		return 0.5, true
	}
	winner, ok := m.Winner()
	if !ok {
		return 0, false
	}
	if winner.TokenID == tokenID {
		return 1, true
	}
	return 0, true
}

func (m Market) finalResolutionData() bool {
	if !m.Closed || len(m.Outcomes) != 2 {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(m.ResolutionStatus))
	return status == "" || status == "resolved"
}

type Provider interface {
	ListOpen(ctx context.Context, maxDays int) ([]Market, error)
	Get(ctx context.Context, id string) (Market, error)
	Name() string
}

// BatchProvider is implemented by providers that can resolve several market
// ids in one round trip. A position book full of markets that already ended
// but are still open upstream never appears in the open-market scan, so the
// engine marks them from a batch call instead of one GET per position. The
// fallback to Get keeps providers without a batch route working.
type BatchProvider interface {
	GetMany(ctx context.Context, ids []string) (map[string]Market, error)
}

// Tag is a Polymarket event tag. Categories are derived from these tags when
// the provider can supply them; otherwise the engine falls back to local
// heuristics (game start time, question text and slug).
type Tag struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Slug  string `json:"slug"`
}

// EventTagProvider is implemented by providers that can resolve event tags in
// batches. It is optional so read-only providers and tests keep working when
// they cannot supply category metadata.
type EventTagProvider interface {
	EventTags(ctx context.Context, eventIDs []string) (map[string][]Tag, error)
}

// LocalizedProvider is implemented by providers that can return the same
// markets with Polymarket's localized question and outcome labels. Locale
// values follow Polymarket's API convention, for example "zh" or "en".
type LocalizedProvider interface {
	MarketsLocale(ctx context.Context, ids []string, locale string) ([]Market, error)
}
