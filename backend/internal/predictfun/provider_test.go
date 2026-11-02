package predictfun

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildMarketJSON renders the subset of the market resource the provider reads.
func buildMarketJSON(t *testing.T, id int, slug, tradingStatus, conditionID, yesToken, noToken string, precision int, yieldBearing bool) string {
	t.Helper()
	payload := map[string]any{
		"id":               id,
		"title":            fmt.Sprintf("Market %d", id),
		"question":         fmt.Sprintf("Question %d?", id),
		"tradingStatus":    tradingStatus,
		"status":           "REGISTERED",
		"isVisible":        true,
		"isNegRisk":        false,
		"isYieldBearing":   yieldBearing,
		"feeRateBps":       200,
		"makerFeeBps":      0,
		"conditionId":      conditionID,
		"categorySlug":     slug,
		"decimalPrecision": precision,
		"stats":            map[string]any{"totalLiquidityUsd": 90.5, "volumeUsd": 12.25},
		"outcomes": []map[string]any{
			{"name": "Yes", "indexSet": 1, "onChainId": yesToken, "status": "OPEN", "bestBid": 0.48, "bestAsk": 0.5},
			{"name": "No", "indexSet": 2, "onChainId": noToken, "status": "OPEN", "bestBid": 0.5, "bestAsk": 0.52},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("build market json: %v", err)
	}
	return string(encoded)
}

type providerFixture struct {
	mu             sync.Mutex
	categoryCalls  map[string]int
	markets        []string
	categoryEndsAt map[string]string
}

func (f *providerFixture) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/markets", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		markets := append([]string(nil), f.markets...)
		f.mu.Unlock()
		writeJSON(w, `{"success":true,"cursor":"","data":[`+strings.Join(markets, ",")+`]}`)
	})
	mux.HandleFunc("/v1/markets/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/markets/")
		f.mu.Lock()
		markets := append([]string(nil), f.markets...)
		f.mu.Unlock()
		for _, market := range markets {
			var decoded struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal([]byte(market), &decoded); err == nil && fmt.Sprint(decoded.ID) == id {
				writeJSON(w, `{"success":true,"data":`+market+`}`)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/v1/categories/", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/v1/categories/")
		f.mu.Lock()
		f.categoryCalls[slug]++
		endsAt, ok := f.categoryEndsAt[slug]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, `{"success":true,"data":{"slug":"`+slug+`","endsAt":"`+endsAt+`","markets":[{"id":42}],"tags":[]}}`)
	})
	return mux
}

func (f *providerFixture) calls(slug string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.categoryCalls[slug]
}

func newProviderFixture(t *testing.T, clock *time.Time) (*Provider, *providerFixture) {
	t.Helper()
	fixture := &providerFixture{
		categoryCalls: map[string]int{},
		categoryEndsAt: map[string]string{
			"crypto": testEndsAt,
			"empty":  "",
		},
		markets: []string{
			buildMarketJSON(t, 42, "crypto", "OPEN", "0xcond42", "111", "222", 2, false),
			buildMarketJSON(t, 43, "crypto", "MATCHING_NOT_ENABLED", "0xcond43", "333", "444", 3, false),
			buildMarketJSON(t, 44, "crypto", "OPEN", "0xcond44", "555", "666", 2, true),
			buildMarketJSON(t, 45, "empty", "OPEN", "0xcond45", "777", "888", 2, false),
		},
	}
	client := newTestClient(t, fixture.handler())
	provider := NewProvider(client)
	provider.now = func() time.Time { return *clock }
	return provider, fixture
}

// buildSettledJSON renders a terminal market using the exact shapes the live
// API returns (verified 2026-11-02 over 56 settled markets): `status` is
// RESOLVED, `tradingStatus` CLOSED, `resolution` repeats the winning outcome
// with status WON, and `outcomes[]` carry status WON / LOST.
//
// winner 0 means "closed but no verdict yet": resolution is null and both
// outcomes are still tagged OPEN.
func buildSettledJSON(t *testing.T, id int, status, tradingStatus string, winner int) string {
	t.Helper()
	yes := map[string]any{
		"name": "Yes", "indexSet": 1, "onChainId": "900", "positionId": "900",
		"status": "OPEN", "bestBid": 0.97, "bestAsk": 0.98,
	}
	no := map[string]any{
		"name": "No", "indexSet": 2, "onChainId": "901", "positionId": "901",
		"status": "OPEN", "bestBid": 0.02, "bestAsk": 0.03,
	}
	var resolution any
	switch winner {
	case 1:
		yes["status"] = "WON"
		no["status"] = "LOST"
		resolution = map[string]any{
			"name": "Yes", "indexSet": 1, "onChainId": "900", "positionId": "900", "status": "WON",
		}
	case 2:
		yes["status"] = "LOST"
		no["status"] = "WON"
		resolution = map[string]any{
			"name": "No", "indexSet": 2, "onChainId": "901", "positionId": "901", "status": "WON",
		}
	case 3:
		// Draw: both sides are tagged WON on chain with payoutNumerators [1,1]
		// and payoutDenominator 2, so each share pays 0.50.
		yes["status"] = "WON"
		no["status"] = "WON"
		resolution = map[string]any{
			"name": "Yes", "indexSet": 1, "onChainId": "900", "positionId": "900", "status": "WON",
		}
	}
	payload := map[string]any{
		"id":               id,
		"title":            fmt.Sprintf("Market %d", id),
		"tradingStatus":    tradingStatus,
		"status":           status,
		"isVisible":        true,
		"isNegRisk":        false,
		"feeRateBps":       200,
		"conditionId":      "0xsettled",
		"categorySlug":     "crypto",
		"decimalPrecision": 2,
		"resolution":       resolution,
		// The mids are deliberately decisive on the *wrong* side: a mapping that
		// trusted them instead of the published verdict would settle the losing
		// position as a win.
		"outcomes": []map[string]any{yes, no},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("build settled json: %v", err)
	}
	return string(encoded)
}

func newSettledProvider(t *testing.T, clock time.Time, body string) *Provider {
	t.Helper()
	fixture := &providerFixture{
		categoryCalls:  map[string]int{},
		categoryEndsAt: map[string]string{"crypto": testEndsAt},
		markets:        []string{body},
	}
	provider := NewProvider(newTestClient(t, fixture.handler()))
	provider.now = func() time.Time { return clock }
	return provider
}

func TestGetMapsPublishedWinnerOntoPayout(t *testing.T) {
	clock := time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC)
	provider := newSettledProvider(t, clock, buildSettledJSON(t, 46, "RESOLVED", "CLOSED", 2))

	item, err := provider.Get(context.Background(), "46")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !item.Closed || item.Active || item.ResolutionStatus != "resolved" {
		t.Fatalf("terminal mapping = %+v", item)
	}
	if item.Outcomes[0].Price != 0 || item.Outcomes[1].Price != 1 {
		t.Fatalf("payout must overwrite the mids, got %v/%v", item.Outcomes[0].Price, item.Outcomes[1].Price)
	}
	if price, resolved := item.SettlementPrice("901"); !resolved || price != 1 {
		t.Fatalf("winner payout = %v/%v, want 1/true", price, resolved)
	}
	if price, resolved := item.SettlementPrice("900"); !resolved || price != 0 {
		t.Fatalf("loser payout = %v/%v, want 0/true", price, resolved)
	}
}

func TestGetSettlesFromOutcomeTagsWhenResolutionIsMissing(t *testing.T) {
	clock := time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC)
	body := strings.Replace(
		buildSettledJSON(t, 48, "RESOLVED", "CLOSED", 1),
		`"resolution":{"indexSet":1,"name":"Yes","onChainId":"900","positionId":"900","status":"WON"}`,
		`"resolution":null`, 1)
	if !strings.Contains(body, `"resolution":null`) {
		t.Fatalf("fixture did not drop the resolution object: %s", body)
	}
	provider := newSettledProvider(t, clock, body)
	item, err := provider.Get(context.Background(), "48")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.ResolutionStatus != "resolved" || item.Outcomes[0].Price != 1 || item.Outcomes[1].Price != 0 {
		t.Fatalf("the WON tag alone must settle market: %+v", item.Outcomes)
	}
}

func TestGetMapsDrawToFiftyFifty(t *testing.T) {
	clock := time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC)
	provider := newSettledProvider(t, clock, buildSettledJSON(t, 49, "RESOLVED", "CLOSED", 3))

	item, err := provider.Get(context.Background(), "49")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.ResolutionStatus != "resolved" {
		t.Fatalf("a draw is settled: %+v", item)
	}
	for _, outcome := range item.Outcomes {
		if outcome.Price != 0.5 {
			t.Fatalf("draw must pay 0.50 on both sides, got %+v", item.Outcomes)
		}
		price, resolved := item.SettlementPrice(outcome.TokenID)
		if !resolved || price != 0.5 {
			t.Fatalf("draw payout = %v/%v, want 0.5/true", price, resolved)
		}
	}
}

func TestGetRefusesToSettleClosedMarketWithoutWinner(t *testing.T) {
	clock := time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC)
	provider := newSettledProvider(t, clock, buildSettledJSON(t, 47, "CLOSED", "CLOSED", 0))

	item, err := provider.Get(context.Background(), "47")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !item.Closed || item.ResolutionStatus == "resolved" {
		t.Fatalf("closed-but-unsettled must not look resolved: %+v", item)
	}
	// The deliberate 0.97/0.02 mids must not be mistaken for a settlement price.
	if price, resolved := item.SettlementPrice("900"); resolved {
		t.Fatalf("must not settle on stale mids, got %v", price)
	}
}

func TestListOpenJoinsCategoryEndDateAndCaches(t *testing.T) {
	clock := time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC)
	provider, fixture := newProviderFixture(t, &clock)

	markets, err := provider.ListOpen(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(markets) != 2 {
		t.Fatalf("markets = %d, want 2 (yield-bearing and unknown-end skipped)", len(markets))
	}
	if fixture.calls("crypto") != 1 {
		t.Fatalf("crypto category calls = %d, want 1 for two markets", fixture.calls("crypto"))
	}
	if fixture.calls("empty") != 1 {
		t.Fatalf("empty category calls = %d, want 1", fixture.calls("empty"))
	}

	first := markets[0]
	if first.ID != "42" || first.ConditionID != "0xcond42" {
		t.Fatalf("market = %+v", first)
	}
	wantEnd, _ := time.Parse(time.RFC3339, testEndsAt)
	if !first.EndDate.Equal(wantEnd) {
		t.Fatalf("end date = %s, want %s", first.EndDate, wantEnd)
	}
	if first.TickSize != 0.01 || first.MinNotional != 1 || first.FeeRateBPS != 200 {
		t.Fatalf("tick/min/fee = %v/%v/%v", first.TickSize, first.MinNotional, first.FeeRateBPS)
	}
	if first.Venue != "predictfun" || first.Currency != "USDT" || first.Category != "Crypto" {
		t.Fatalf("venue/currency/category = %s/%s/%s", first.Venue, first.Currency, first.Category)
	}
	if !first.Active || first.Closed || first.Liquidity != 90.5 || first.Volume != 12.25 {
		t.Fatalf("flags/liquidity = %+v", first)
	}
	if len(first.Outcomes) != 2 || first.Outcomes[0].TokenID != "111" || first.Outcomes[0].Price != 0.49 {
		t.Fatalf("outcomes = %+v", first.Outcomes)
	}
	second := markets[1]
	if second.TickSize != 0.001 {
		t.Fatalf("precision 3 tick = %v, want 0.001", second.TickSize)
	}
	if second.Active {
		t.Fatal("MATCHING_NOT_ENABLED must not be Active")
	}

	// A second scan inside the TTL must not re-read the category.
	if _, err := provider.ListOpen(context.Background(), 0); err != nil {
		t.Fatalf("second ListOpen: %v", err)
	}
	if fixture.calls("crypto") != 1 {
		t.Fatalf("crypto category re-read inside TTL: %d calls", fixture.calls("crypto"))
	}

	// Past the TTL the category is refreshed once more.
	clock = clock.Add(endsAtTTL + time.Minute)
	if _, err := provider.ListOpen(context.Background(), 0); err != nil {
		t.Fatalf("third ListOpen: %v", err)
	}
	if fixture.calls("crypto") != 2 {
		t.Fatalf("crypto category calls after TTL = %d, want 2", fixture.calls("crypto"))
	}
}

func TestListOpenSkipsMarketsOutsideWindow(t *testing.T) {
	clock := time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC)
	provider, _ := newProviderFixture(t, &clock)
	// The fixture's end date is 2030-01-01, 31 days after the clock.
	markets, err := provider.ListOpen(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(markets) != 0 {
		t.Fatalf("markets outside the 10 day window = %d", len(markets))
	}
	if _, err := provider.ListOpen(context.Background(), 60); err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
}

func TestGetRejectsMarketWithoutEndDate(t *testing.T) {
	clock := time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC)
	provider, _ := newProviderFixture(t, &clock)
	_, err := provider.Get(context.Background(), "45")
	if err == nil || !strings.Contains(err.Error(), "到期时间") {
		t.Fatalf("error = %v, want a missing end date error", err)
	}
}

func TestMarketMetaIndexesTokensAndConditions(t *testing.T) {
	clock := time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC)
	provider, _ := newProviderFixture(t, &clock)
	if _, err := provider.ListOpen(context.Background(), 0); err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	meta, err := provider.marketMeta(context.Background(), "222", "0xcond42")
	if err != nil {
		t.Fatalf("marketMeta: %v", err)
	}
	if meta.YesTokenID != "111" || meta.NoTokenID != "222" || meta.Tick != 0.01 {
		t.Fatalf("meta = %+v", meta)
	}
	if outcome, ok := meta.outcomeFor("222"); !ok || outcome != OutcomeNo {
		t.Fatalf("outcome for No token = %v (%v)", outcome, ok)
	}
	if _, err := provider.marketMeta(context.Background(), "222", "0xother"); err == nil {
		t.Fatal("mismatched condition id must not resolve")
	}
	if _, err := provider.marketMeta(context.Background(), "999", ""); err == nil {
		t.Fatal("unknown token must not resolve")
	}
}

func TestTickFromPrecision(t *testing.T) {
	cases := []struct {
		precision Number
		want      float64
	}{
		{"2", 0.01},
		{"3", 0.001},
		{"0", 1},
		{"", 0},
		{"12", 0},
	}
	for _, testCase := range cases {
		if got := tickFromPrecision(testCase.precision); math.Abs(got-testCase.want) > 1e-12 {
			t.Fatalf("tick(%q) = %v, want %v", testCase.precision, got, testCase.want)
		}
	}
}

func TestOutcomeMidUsesBothSidesThenFallback(t *testing.T) {
	mid := outcomeMid(MarketOutcome{BestBid: Quote{Price: "0.4"}, BestAsk: Quote{Price: "0.5"}}, 0.01)
	if math.Abs(mid-0.45) > 1e-9 {
		t.Fatalf("mid = %v, want 0.45", mid)
	}
	if got := outcomeMid(MarketOutcome{BestBid: Quote{Price: "0.4"}}, 0.01); got != 0.4 {
		t.Fatalf("bid-only price = %v", got)
	}
	if got := outcomeMid(MarketOutcome{BestAsk: Quote{Price: "0.6"}}, 0.01); got != 0.6 {
		t.Fatalf("ask-only price = %v", got)
	}
	if got := outcomeMid(MarketOutcome{}, 0.01); got != 0 {
		t.Fatalf("empty price = %v", got)
	}
}
