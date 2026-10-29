package market

import (
	"context"
	"strings"
	"time"
)

// Outcome is one side of a binary market. Price is an indicative public-data
// price, not a quote that this application can execute against.
type Outcome struct {
	Name    string  `json:"name"`
	TokenID string  `json:"token_id"`
	Price   float64 `json:"price"`
}

type Market struct {
	ID               string    `json:"id"`
	ConditionID      string    `json:"condition_id"`
	Question         string    `json:"question"`
	Slug             string    `json:"slug"`
	Outcomes         []Outcome `json:"outcomes"`
	EndDate          time.Time `json:"end_date"`
	Liquidity        float64   `json:"liquidity"`
	Volume           float64   `json:"volume"`
	Active           bool      `json:"active"`
	Closed           bool      `json:"closed"`
	ResolutionStatus string    `json:"resolution_status"`
}

func (m Market) URL() string {
	if m.Slug == "" {
		return ""
	}
	return "https://polymarket.com/event/" + m.Slug
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
