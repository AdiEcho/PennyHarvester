package market

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// keysetBody wraps market JSON in the /markets/keyset envelope.
func keysetBody(markets string) string {
	return `{"markets":` + markets + `}`
}

func TestGammaClientParsesEncodedArrays(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events/keyset") {
			_, _ = w.Write([]byte(`{"events":[]}`))
			return
		}
		if r.URL.Path != "/markets/keyset" || r.URL.Query().Get("closed") != "false" {
			t.Errorf("unexpected request: %s", r.URL.String())
		}
		if r.URL.Query().Get("offset") != "" {
			t.Errorf("keyset rejects offset: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(keysetBody(`[{
			"id":"42","conditionId":"condition-42","question":"Will it resolve?","slug":"will-it-resolve",
			"outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.98\",\"0.02\"]",
			"version":"v1","clobTokenIds":"[\"token-yes\",\"token-no\"]","endDate":"2099-01-01T00:00:00Z",
			"liquidity":"25000","volume":"90000","active":true,"closed":false
		}]`)))
	}))
	defer server.Close()
	client := NewGammaClient(server.URL, nil)
	items, err := client.ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Outcomes[0].Price != 0.98 || items[0].Liquidity != 25000 {
		t.Fatalf("unexpected market: %+v", items)
	}
}

func TestGammaClientListOpenFollowsKeysetCursor(t *testing.T) {
	var mu sync.Mutex
	marketCursors := []string{}
	eventCursors := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("after_cursor")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events/keyset") {
			mu.Lock()
			eventCursors = append(eventCursors, cursor)
			mu.Unlock()
			if cursor == "" {
				_, _ = w.Write([]byte(`{"events":[{"id":"ev-1","slug":"btc-updown-5m-1","title":"Bitcoin Up or Down","markets":[]}],"next_cursor":"event-2"}`))
				return
			}
			_, _ = w.Write([]byte(`{"events":[]}`))
			return
		}
		mu.Lock()
		marketCursors = append(marketCursors, cursor)
		mu.Unlock()
		body := `[{"id":"1","outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.98\",\"0.02\"]","clobTokenIds":"[\"a\",\"b\"]","endDate":"2099-01-01T00:00:00Z","active":true}]`
		if cursor == "" {
			_, _ = w.Write([]byte(`{"markets":` + body + `,"next_cursor":"page-2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"markets":` + body + `}`))
	}))
	defer server.Close()

	client := NewGammaClient(server.URL, nil)
	if _, err := client.ListOpen(context.Background(), 30); err != nil {
		t.Fatal(err)
	}
	// Every view walks next_cursor and stops when the response has no cursor.
	// The market views take two pages each; the event view takes two as well.
	mu.Lock()
	defer mu.Unlock()
	if len(marketCursors) != 6 {
		t.Fatalf("expected 3 market views x 2 pages, got %d requests: %v", len(marketCursors), marketCursors)
	}
	followed := false
	for _, cursor := range marketCursors {
		if cursor == "page-2" {
			followed = true
		}
	}
	if !followed {
		t.Fatalf("the market views never followed their cursor: %v", marketCursors)
	}
	if len(eventCursors) != 2 || eventCursors[0] != "" || eventCursors[1] != "event-2" {
		t.Fatalf("the event view did not follow its cursor: %v", eventCursors)
	}
}

// The tail view exists because a settlement boundary ties hundreds of markets on
// endDate. A market-ordered page then spends every row on the tie group, and the
// five-minute crypto window that settles at the same instant never reaches the
// radar. Reading events first keeps it on page one.
func TestGammaClientListOpenKeepsTailMarketsThroughEventView(t *testing.T) {
	var mu sync.Mutex
	var tailQuery url.URL
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events/keyset") {
			mu.Lock()
			tailQuery = *r.URL
			mu.Unlock()
			// The nested market carries neither its event stamp nor its tags,
			// exactly like the upstream payload.
			_, _ = w.Write([]byte(`{"events":[{
				"id":"ev-btc","slug":"btc-updown-5m-1794078000","title":"Bitcoin Up or Down",
				"tags":[{"id":"21","label":"Crypto","slug":"crypto"},{"id":"235","label":"Bitcoin","slug":"bitcoin"}],
				"markets":[{
					"id":"6757729","conditionId":"condition-6757729",
					"question":"Bitcoin Up or Down - 1:55PM-2:00PM ET","slug":"btc-updown-5m-1794078000",
					"outcomes":"[\"Up\",\"Down\"]","outcomePrices":"[\"0.025\",\"0.975\"]",
					"clobTokenIds":"[\"up-token\",\"down-token\"]","endDate":"2099-01-01T00:00:00Z",
					"acceptingOrders":true,"active":true,"closed":false,"liquidityNum":8113.9
				}]
			}]}`))
			return
		}
		// The market views are entirely consumed by one endDate tie group, which
		// is what the tail view has to route around.
		_, _ = w.Write([]byte(keysetBody(`[{
			"id":"9","question":"Tie group","slug":"tie-group","outcomes":"[\"Yes\",\"No\"]",
			"outcomePrices":"[\"0.97\",\"0.03\"]","clobTokenIds":"[\"t1\",\"t2\"]",
			"endDate":"2099-01-01T00:00:00Z","active":true,"closed":false
		}]`)))
	}))
	defer server.Close()

	items, err := NewGammaClient(server.URL, nil).ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	var tail *Market
	for i := range items {
		if items[i].ID == "6757729" {
			tail = &items[i]
		}
	}
	if tail == nil {
		t.Fatalf("the crypto tail market was crowded out: %+v", items)
	}
	if tail.EventID != "ev-btc" || tail.EventTitle != "Bitcoin Up or Down" {
		t.Fatalf("nested market lost its event stamp: %+v", tail)
	}
	if len(tail.Tags) != 2 || tail.Tags[0].Slug != "crypto" || tail.Tags[1].Slug != "bitcoin" {
		t.Fatalf("nested market lost its event tags: %+v", tail.Tags)
	}
	mu.Lock()
	query := tailQuery.Query()
	mu.Unlock()
	if query.Get("order") != "endDate" || query.Get("ascending") != "true" {
		t.Fatalf("tail view must read the earliest events first: %v", query)
	}
	endMax, err := time.Parse(time.RFC3339, query.Get("end_date_max"))
	if err != nil {
		t.Fatalf("tail view end_date_max: %v", err)
	}
	if remaining := time.Until(endMax); remaining > tailHorizon+time.Minute || remaining < tailHorizon-time.Minute {
		t.Fatalf("tail view must cover %s, got %s", tailHorizon, remaining)
	}
}

func TestGammaClientParsesProtocolV2Market(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A Protocol V2 market trades position IDs, leaves clobTokenIds null and
		// reports its settlement through resolutionStatus.
		_, _ = w.Write([]byte(keysetBody(`[{
			"id":"77","conditionId":"condition-77","question":"Will it resolve?","slug":"v2-market",
			"outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.97\",\"0.03\"]",
			"version":"v2","clobTokenIds":null,"positionIds":["pos-yes","pos-no"],
			"resolutionStatus":"active","endDate":"2099-01-01T00:00:00Z",
			"acceptingOrders":false,"active":true,"closed":false
		}]`)))
	}))
	defer server.Close()

	items, err := NewGammaClient(server.URL, nil).ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("a V2 market must not be dropped: %+v", items)
	}
	item := items[0]
	if item.Outcomes[0].TokenID != "pos-yes" || item.Outcomes[1].TokenID != "pos-no" {
		t.Fatalf("position ids not used: %+v", item.Outcomes)
	}
	if !item.IsV2() {
		t.Fatalf("version not parsed: %q", item.Version)
	}
	if item.ResolutionStatus != "active" {
		t.Fatalf("resolutionStatus not parsed: %q", item.ResolutionStatus)
	}
	if item.TradeReady() {
		t.Fatal("acceptingOrders=false must not be trade ready")
	}
}

func TestGammaClientUsesClobTokenIDsForV1(t *testing.T) {
	// Both identifier fields can be populated; the version decides which one is
	// authoritative, so a v1 market must keep using clobTokenIds.
	ids, err := gammaOutcomeIDs(gammaMarket{
		ID: "1", Version: "v1",
		ClobTokenIDs: json.RawMessage(`["token-yes","token-no"]`),
		PositionIDs:  json.RawMessage(`["pos-yes","pos-no"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "token-yes" {
		t.Fatalf("v1 must read clobTokenIds: %v", ids)
	}
}

func TestGammaClientRejectsUnsupportedVersion(t *testing.T) {
	if _, err := gammaOutcomeIDs(gammaMarket{ID: "1", Version: "v3", ClobTokenIDs: json.RawMessage(`["a","b"]`)}); err == nil {
		t.Fatal("an unsupported protocol version must not be traded on guesswork")
	}
}

func TestGammaClientParsesAcceptingOrders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(keysetBody(`[{
			"id":"1","outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.98\",\"0.02\"]",
			"clobTokenIds":"[\"a\",\"b\"]","endDate":"2099-01-01T00:00:00Z","active":true,
			"acceptingOrders":false
		}]`)))
	}))
	defer server.Close()
	items, err := NewGammaClient(server.URL, nil).ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].AcceptingOrders == nil || *items[0].AcceptingOrders {
		t.Fatalf("acceptingOrders not parsed: %+v", items)
	}
}

func TestGammaClientListOpenUsesBalancedViews(t *testing.T) {
	var mu sync.Mutex
	orders := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		orders[r.URL.Query().Get("order")] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(keysetBody(`[]`)))
	}))
	defer server.Close()
	client := NewGammaClient(server.URL, nil)
	if _, err := client.ListOpen(context.Background(), 30); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, order := range []string{"endDate", "volume24hr", "liquidity"} {
		if !orders[order] {
			t.Fatalf("ListOpen did not request the %s view: %v", order, orders)
		}
	}
}

func TestGammaClientListOpenCachesBroadViews(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	var tailRequests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events/keyset") {
			atomic.AddInt32(&tailRequests, 1)
			_, _ = w.Write([]byte(`{"events":[]}`))
			return
		}
		mu.Lock()
		counts[r.URL.Query().Get("order")]++
		mu.Unlock()
		_, _ = w.Write([]byte(keysetBody(`[]`)))
	}))
	defer server.Close()

	client := NewGammaClient(server.URL, nil)
	for i := 0; i < 2; i++ {
		if _, err := client.ListOpen(context.Background(), 30); err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if counts["endDate"] != 2 {
		t.Fatalf("the near-end view must be fresh on every scan, got %d requests", counts["endDate"])
	}
	for _, order := range []string{"volume24hr", "liquidity"} {
		if counts[order] != 1 {
			t.Fatalf("the %s view should be cached after its first scan, got %d requests", order, counts[order])
		}
	}
	if got := atomic.LoadInt32(&tailRequests); got != 2 {
		t.Fatalf("the tail view must be fresh on every scan, got %d requests", got)
	}
}

func TestGammaClientParsesCategorySignals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events/keyset") {
			// The tail view reads tags from the parent event, so the market view
			// is the only one that asks Gamma to inline them.
			_, _ = w.Write([]byte(`{"events":[]}`))
			return
		}
		if r.URL.Query().Get("include_tag") != "true" {
			t.Errorf("include_tag missing: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(keysetBody(`[{
			"id":"42","conditionId":"condition-42","question":"Will the Mavericks beat the Grizzlies?",
			"slug":"nba-dal-mem-2026-12-04","outcomes":["Yes","No"],"outcomePrices":["0.98","0.02"],
			"clobTokenIds":["token-yes","token-no"],"endDate":"2099-01-01T00:00:00Z",
			"gameStartTime":"2026-12-04 00:00:00+00","oneDayPriceChange":-0.04,
			"tags":[{"id":"1","label":"Sports","slug":"sports"},{"id":"28","label":"Basketball","slug":"basketball"}],
			"liquidity":"25000","volume":"90000","active":true,"closed":false,
			"events":[{"id":"event-42","slug":"nba-dal-mem-2026-12-04","title":"Mavericks vs. Grizzlies"}]
		}]`)))
	}))
	defer server.Close()
	client := NewGammaClient(server.URL, nil)
	items, err := client.ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("unexpected items: %+v", items)
	}
	item := items[0]
	if item.EventID != "event-42" || item.EventTitle != "Mavericks vs. Grizzlies" {
		t.Fatalf("event metadata not parsed: %+v", item)
	}
	if len(item.Tags) != 2 || item.Tags[0].Slug != "sports" || item.Tags[1].Slug != "basketball" {
		t.Fatalf("market tags not parsed: %+v", item.Tags)
	}
	if item.GameStartTime == nil || !item.GameStartTime.Equal(time.Date(2026, 12, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("game start time not parsed: %+v", item.GameStartTime)
	}
	if item.PriceChange24h == nil || *item.PriceChange24h != -0.04 {
		t.Fatalf("24h price change not parsed: %+v", item.PriceChange24h)
	}
}

func TestGammaClientEventTagsBatchesIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		ids := r.URL.Query()["id"]
		if len(ids) != 2 || ids[0] != "e1" || ids[1] != "e2" {
			t.Errorf("ids = %v, want [e1 e2]", ids)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"e1","tags":[{"id":"1","label":"Sports","slug":"sports"}]},
			{"id":"e2","tags":[{"id":"2","label":"Politics","slug":"politics"}]}
		]`))
	}))
	defer server.Close()
	client := NewGammaClient(server.URL, nil)
	tags, err := client.EventTags(context.Background(), []string{"e1", "e2", "e1", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags["e1"]) != 1 || tags["e1"][0].Slug != "sports" || len(tags["e2"]) != 1 || tags["e2"][0].Slug != "politics" {
		t.Fatalf("unexpected event tags: %+v", tags)
	}
}

func TestGammaClientMarketsLocaleRequestsTranslatedText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets/keyset" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("locale"); got != "zh" {
			t.Errorf("locale = %q, want zh", got)
		}
		if ids := r.URL.Query()["id"]; len(ids) != 2 || ids[0] != "42" || ids[1] != "43" {
			t.Errorf("ids = %v, want [42 43]", ids)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(keysetBody(`[{
			"id":"42","conditionId":"condition-42","question":"会结算吗？","groupItemTitle":"O/U 0.5","slug":"will-it-resolve",
			"outcomes":"[\"是\",\"否\"]","outcomePrices":"[\"0.98\",\"0.02\"]",
			"clobTokenIds":"[\"token-yes\",\"token-no\"]","endDate":"2099-01-01T00:00:00Z",
			"liquidity":"25000","volume":"90000","active":true,"closed":false,
			"events":[{"slug":"will-it-resolve","title":"会结算吗 -更多市场"}]
		}]`)))
	}))
	defer server.Close()
	client := NewGammaClient(server.URL, nil)
	items, err := client.MarketsLocale(context.Background(), []string{"42", "43"}, "zh")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Question != "会结算吗？" || items[0].Outcomes[0].Name != "是" {
		t.Fatalf("unexpected localized market: %+v", items)
	}
	if items[0].GroupItemTitle != "O/U 0.5" || items[0].EventTitle != "会结算吗 -更多市场" {
		t.Fatalf("localized context not parsed: %+v", items[0])
	}
}

func TestWinnerRequiresTerminalResolution(t *testing.T) {
	item := binaryForTest(0.99, 0.01)
	item.Closed = true
	item.ResolutionStatus = "resolved"
	if _, ok := item.Winner(); ok {
		t.Fatal("non-terminal prices must not settle a position")
	}
	item.Outcomes[0].Price = 1
	item.Outcomes[1].Price = 0
	winner, ok := item.Winner()
	if !ok || winner.TokenID != "yes" {
		t.Fatalf("expected YES winner: %+v, %v", winner, ok)
	}
	item.ResolutionStatus = "proposed"
	if _, ok := item.Winner(); ok {
		t.Fatal("proposed resolution must not be treated as final")
	}
}

func TestSettlementPriceHandlesEqualSplit(t *testing.T) {
	item := binaryForTest(0.5, 0.5)
	item.Closed = true
	item.ResolutionStatus = "resolved"
	price, ok := item.SettlementPrice("yes")
	if !ok || price != 0.5 {
		t.Fatalf("expected 50/50 paper settlement, got %f, %v", price, ok)
	}
	if _, ok := item.Winner(); ok {
		t.Fatal("equal split must not have a winner")
	}
}

func TestDecodeFloatArraySupportsNumericJSON(t *testing.T) {
	var values []float64
	if err := decodeFloatArray(json.RawMessage(`[0.95,"0.99"]`), &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0] != 0.95 || values[1] != 0.99 {
		t.Fatalf("unexpected values: %v", values)
	}
}

func TestGammaClientListMarketsKeepsFirstPageOnCursorTimeout(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"markets":[{"id":"42","conditionId":"condition-42","question":"Will it resolve?","slug":"will-it-resolve","outcomes":["Yes","No"],"outcomePrices":["0.98","0.02"],"clobTokenIds":["token-yes","token-no"],"endDate":"2099-01-01T00:00:00Z","liquidity":"25000","active":true,"closed":false}],"next_cursor":"next"}`))
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewGammaClient(server.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	items, err := client.listMarkets(ctx, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().AddDate(0, 0, 1).Format(time.RFC3339), "endDate", "true", 2)
	if err != nil {
		t.Fatalf("a timed-out cursor page should keep the first page: %v", err)
	}
	if len(items) != 1 || items[0].ID != "42" {
		t.Fatalf("first page lost: %+v", items)
	}
}

func TestGammaClientGetManyBatchesIDs(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/markets" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			return
		}
		requests++
		ids := r.URL.Query()["id"]
		if len(ids) != 2 || ids[0] != "one" || ids[1] != "two" {
			t.Errorf("batch ids not forwarded: %v", ids)
		}
		_, _ = w.Write([]byte(`[{
			"id":"one","conditionId":"c-one","question":"Ended market one?","slug":"one",
			"outcomes":["Yes","No"],"outcomePrices":["0.98","0.02"],
			"clobTokenIds":["one-yes","one-no"],"endDate":"2026-11-08T04:59:00Z",
			"liquidity":"25000","volume":"90000","active":true,"closed":false
		},{
			"id":"two","conditionId":"c-two","question":"Ended market two?","slug":"two",
			"outcomes":["Yes","No"],"outcomePrices":["0.4","0.6"],
			"clobTokenIds":["two-yes","two-no"],"endDate":"2026-11-08T04:59:00Z",
			"liquidity":"25000","volume":"90000","active":true,"closed":false
		}]`))
	}))
	defer server.Close()
	client := NewGammaClient(server.URL, nil)
	items, err := client.GetMany(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("two ids must cost one request, got %d", requests)
	}
	if len(items) != 2 || items["one"].Outcomes[0].Price != 0.98 || items["two"].ID != "two" {
		t.Fatalf("batch not parsed: %+v", items)
	}
}

func TestMarketRejectsInvalidEndDate(t *testing.T) {
	raw := gammaMarket{ID: "1", Version: "v1", Outcomes: json.RawMessage(`["Yes","No"]`), OutcomePrices: json.RawMessage(`[0.9,0.1]`), ClobTokenIDs: json.RawMessage(`["a","b"]`), EndDate: time.Time{}.Format(time.RFC3339)}
	// Zero time is valid RFC3339, so this verifies parsing rather than silently
	// replacing malformed values; malformed values must return an error.
	if _, err := parseGammaMarket(raw); err != nil {
		t.Fatal(err)
	}
	raw.EndDate = "not-a-date"
	if _, err := parseGammaMarket(raw); err == nil {
		t.Fatal("expected invalid end date error")
	}
}

func TestMarketTradeReadyDefaultsToAvailable(t *testing.T) {
	// A provider that cannot report acceptingOrders must never look paused.
	if !binaryForTest(0.98, 0.02).TradeReady() {
		t.Fatal("an unreported acceptingOrders must count as trade ready")
	}
	available := true
	if !(Market{AcceptingOrders: &available}).TradeReady() {
		t.Fatal("acceptingOrders=true must be trade ready")
	}
	paused := false
	if (Market{AcceptingOrders: &paused}).TradeReady() {
		t.Fatal("acceptingOrders=false must not be trade ready")
	}
}

func binaryForTest(yes, no float64) Market {
	return Market{Active: true, Outcomes: []Outcome{{Name: "Yes", TokenID: "yes", Price: yes}, {Name: "No", TokenID: "no", Price: no}}}
}
