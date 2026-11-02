package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"pennyharvester/backend/internal/api"
	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/store"
	"pennyharvester/backend/internal/venue"

	"github.com/gin-gonic/gin"
)

type fixtureProvider struct {
	markets map[string]market.Market
}

func (f *fixtureProvider) Name() string { return "fixture" }

func (f *fixtureProvider) Venue() market.Venue { return market.VenuePolymarket }

func (f *fixtureProvider) ListOpen(context.Context, int) ([]market.Market, error) {
	items := []market.Market{}
	for _, item := range f.markets {
		if !item.Closed {
			items = append(items, item)
		}
	}
	return items, nil
}

func (f *fixtureProvider) Get(_ context.Context, id string) (market.Market, error) {
	item, ok := f.markets[id]
	if !ok {
		return market.Market{}, errors.New("market not found")
	}
	return item, nil
}

func testProvider() *fixtureProvider {
	return &fixtureProvider{markets: map[string]market.Market{
		"test-1": {
			ID: "test-1", ConditionID: "condition-test-1", Question: "API test market",
			Slug: "api-test-market", Active: true, Liquidity: 25000, Volume: 90000,
			EndDate: time.Now().UTC().Add(10 * time.Minute),
			Outcomes: []market.Outcome{
				{Name: "Yes", TokenID: "yes", Price: 0.98},
				{Name: "No", TokenID: "no", Price: 0.02},
			},
		},
	}}
}

func setupRouter(t *testing.T) (http.Handler, *engine.Engine) {
	return setupRouterWithLive(t, nil)
}

// apiTestRegistry registers the fixture provider under Polymarket, matching the
// production wiring (market data always registered, executor maybe nil).
func apiTestRegistry(t *testing.T, provider *fixtureProvider, live engine.LiveExecutor) *venue.Registry {
	t.Helper()
	registry := venue.NewRegistry()
	caps := venue.Capabilities{
		Venue: market.VenuePolymarket, Label: "Polymarket", Currency: market.CurrencyUSDC,
		QuoteDecimals: 6, OrderTypes: []string{"FAK"}, SupportsSettlement: true,
	}
	if err := registry.Register(caps, provider, live); err != nil {
		t.Fatal(err)
	}
	return registry
}

func setupRouterWithLive(t *testing.T, live engine.LiveExecutor) (http.Handler, *engine.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repository, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	registry := apiTestRegistry(t, testProvider(), live)
	instance, err := engine.New(context.Background(), repository, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewRouter(instance, "", api.NewAuthenticator(repository)), instance
}

// quoteLive exercises the read-only quote path without any wallet credentials.
type quoteLive struct{}

func (quoteLive) Configured() bool   { return false }
func (quoteLive) Signer() string     { return "" }
func (quoteLive) Maker() string      { return "" }
func (quoteLive) ReadyError() string { return "未配置 POLYMARKET_PRIVATE_KEY" }
func (quoteLive) Buy(context.Context, string, string, float64, float64, func(string) error) (engine.LiveFill, error) {
	return engine.LiveFill{}, errors.New("live execution is not configured")
}
func (quoteLive) Sell(context.Context, string, string, float64, func(string) error) (engine.LiveFill, error) {
	return engine.LiveFill{}, engine.ErrInvalidOrder
}
func (quoteLive) LookupOrder(context.Context, string, string) (engine.LiveFill, bool, error) {
	return engine.LiveFill{}, false, nil
}
func (quoteLive) EstimateBuyFee(context.Context, string, float64, float64) (float64, error) {
	return 0, errors.New("live execution is not configured")
}
func (quoteLive) Quote(context.Context, string, string, float64) (engine.LiveQuote, error) {
	return engine.LiveQuote{
		BestAsk: 0.979, LimitPrice: 0.981, FillPrice: 0.9805, Shares: 101.98,
		Notional: 99.99, Fee: 0.09, FeeRate: 0.05, AvailableNotional: 5000, FullyFilled: true,
	}, nil
}
func (quoteLive) Collateral(context.Context) (engine.CashSnapshot, error) {
	return engine.CashSnapshot{}, nil
}

func accountID(t *testing.T, handler http.Handler, mode engine.AccountMode) int64 {
	t.Helper()
	response := request(t, handler, http.MethodGet, "/api/v1/accounts", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("accounts failed: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Items []engine.Account `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, account := range payload.Items {
		if account.Mode == mode {
			return account.ID
		}
	}
	t.Fatalf("no %s account in %s", mode, response.Body.String())
	return 0
}

func request(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestHealthAndPaperOrderAPI(t *testing.T) {
	handler, _ := setupRouter(t)
	health := request(t, handler, http.MethodGet, "/api/v1/health", nil)
	if health.Code != http.StatusOK || !bytes.Contains(health.Body.Bytes(), []byte(`"default_account_mode":"live"`)) {
		t.Fatalf("unexpected health response: %d %s", health.Code, health.Body.String())
	}
	paperID := accountID(t, handler, engine.AccountModePaper)
	order := request(t, handler, http.MethodPost, "/api/v1/accounts/"+itoa(paperID)+"/orders", map[string]any{
		"market_id": "test-1", "outcome": "Yes", "token_id": "yes", "amount": 100,
	})
	if order.Code != http.StatusCreated {
		t.Fatalf("paper order failed: %d %s", order.Code, order.Body.String())
	}
	dashboard := request(t, handler, http.MethodGet, "/api/v1/dashboard?account_id="+itoa(paperID), nil)
	if dashboard.Code != http.StatusOK || !bytes.Contains(dashboard.Body.Bytes(), []byte(`"open_positions":1`)) {
		t.Fatalf("unexpected dashboard: %d %s", dashboard.Code, dashboard.Body.String())
	}
	if !bytes.Contains(dashboard.Body.Bytes(), []byte(`"fees_paid":0.15`)) {
		t.Fatalf("paper fee was not reported by the dashboard: %s", dashboard.Body.String())
	}
	orders := request(t, handler, http.MethodGet, "/api/v1/orders?account_id="+itoa(paperID)+"&status=all", nil)
	if orders.Code != http.StatusOK || !bytes.Contains(orders.Body.Bytes(), []byte(`"fee":0.15`)) {
		t.Fatalf("order fee was not exposed by the API: %d %s", orders.Code, orders.Body.String())
	}
}

func TestOpportunityQuoteAPI(t *testing.T) {
	handler, _ := setupRouterWithLive(t, quoteLive{})
	scan := request(t, handler, http.MethodPost, "/api/v1/scan", nil)
	if scan.Code != http.StatusOK {
		t.Fatalf("scan failed: %d %s", scan.Code, scan.Body.String())
	}
	response := request(t, handler, http.MethodPost, "/api/v1/opportunities/quote", map[string]any{"token_id": "yes"})
	if response.Code != http.StatusOK {
		t.Fatalf("quote failed: %d %s", response.Code, response.Body.String())
	}
	var quote engine.OpportunityQuote
	if err := json.Unmarshal(response.Body.Bytes(), &quote); err != nil {
		t.Fatal(err)
	}
	if quote.Amount != 100 || quote.Fee != 0.09 || quote.LimitPrice != 0.981 || quote.NetYield <= 0 {
		t.Fatalf("unexpected quote payload: %+v", quote)
	}
	// An unknown token cannot be quoted: the radar is the source of truth for
	// which condition IDs are eligible.
	missing := request(t, handler, http.MethodPost, "/api/v1/opportunities/quote", map[string]any{"token_id": "missing"})
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("unknown token should be rejected: %d %s", missing.Code, missing.Body.String())
	}
}

func TestToggleAcceptsFalseAndRejectsBadSettings(t *testing.T) {
	handler, _ := setupRouter(t)
	toggle := request(t, handler, http.MethodPost, "/api/v1/engine/toggle", map[string]any{"running": false})
	if toggle.Code != http.StatusOK || !bytes.Contains(toggle.Body.Bytes(), []byte(`"running":false`)) {
		t.Fatalf("unexpected toggle response: %d %s", toggle.Code, toggle.Body.String())
	}
	settings := engine.DefaultSettings()
	settings.MaxPrice = 1
	response := request(t, handler, http.MethodPut, "/api/v1/settings", settings)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid settings accepted: %d %s", response.Code, response.Body.String())
	}
}

func TestRejectsMismatchedToken(t *testing.T) {
	handler, _ := setupRouter(t)
	paperID := accountID(t, handler, engine.AccountModePaper)
	response := request(t, handler, http.MethodPost, "/api/v1/accounts/"+itoa(paperID)+"/orders", map[string]any{
		"market_id": "test-1", "outcome": "No", "token_id": "yes", "amount": 100,
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("mismatched outcome accepted: %d %s", response.Code, response.Body.String())
	}
}

func TestLiveOrderRequiresConfiguration(t *testing.T) {
	handler, _ := setupRouter(t)
	liveID := accountID(t, handler, engine.AccountModeLive)
	response := request(t, handler, http.MethodPost, "/api/v1/accounts/"+itoa(liveID)+"/orders", map[string]any{
		"market_id": "test-1", "outcome": "Yes", "token_id": "yes", "amount": 100,
	})
	if response.Code != http.StatusConflict {
		t.Fatalf("expected live unavailable, got %d %s", response.Code, response.Body.String())
	}
}

func TestAccountCreationAndDefaultSwitch(t *testing.T) {
	handler, _ := setupRouter(t)
	created := request(t, handler, http.MethodPost, "/api/v1/accounts", map[string]any{
		"name": "策略测试", "mode": "paper",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create account failed: %d %s", created.Code, created.Body.String())
	}
	var account engine.Account
	if err := json.Unmarshal(created.Body.Bytes(), &account); err != nil {
		t.Fatal(err)
	}
	updated := request(t, handler, http.MethodPatch, "/api/v1/accounts/"+itoa(account.ID), map[string]any{
		"name": "策略测试 2", "auto_trade": true, "is_default": true,
	})
	if updated.Code != http.StatusOK || !bytes.Contains(updated.Body.Bytes(), []byte(`"is_default":true`)) {
		t.Fatalf("update account failed: %d %s", updated.Code, updated.Body.String())
	}
	secondLive := request(t, handler, http.MethodPost, "/api/v1/accounts", map[string]any{
		"name": "第二个实盘", "mode": "live",
	})
	if secondLive.Code != http.StatusConflict {
		t.Fatalf("second live account should conflict: %d %s", secondLive.Code, secondLive.Body.String())
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestScanAPIDefaultsToScanOnly(t *testing.T) {
	handler, instance := setupRouter(t)
	paperID := accountID(t, handler, engine.AccountModePaper)
	update := request(t, handler, http.MethodPatch, "/api/v1/accounts/"+itoa(paperID), map[string]any{"auto_trade": true})
	if update.Code != http.StatusOK {
		t.Fatalf("enable auto trade failed: %d %s", update.Code, update.Body.String())
	}
	scan := request(t, handler, http.MethodPost, "/api/v1/scan", nil)
	if scan.Code != http.StatusOK {
		t.Fatalf("scan failed: %d %s", scan.Code, scan.Body.String())
	}
	positions, err := instance.Positions(context.Background(), paperID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 0 {
		t.Fatalf("scan-only API placed an order: %+v", positions.Items)
	}
	execute := request(t, handler, http.MethodPost, "/api/v1/scan?execute=true", nil)
	if execute.Code != http.StatusOK {
		t.Fatalf("execute scan failed: %d %s", execute.Code, execute.Body.String())
	}
	positions, err = instance.Positions(context.Background(), paperID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("execute scan did not auto-trade: %+v", positions.Items)
	}
}

func TestScanAPIExecutesSelectedAccountWithoutAutoTradeSwitch(t *testing.T) {
	handler, instance := setupRouter(t)
	paperID := accountID(t, handler, engine.AccountModePaper)
	response := request(t, handler, http.MethodPost, "/api/v1/scan?execute=true&account_id="+itoa(paperID), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("execute scan failed: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		AutoTrade engine.ScanSummary `json:"auto_trade"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AutoTrade.AccountID != paperID || !payload.AutoTrade.Execute || payload.AutoTrade.OrdersPlaced != 1 {
		t.Fatalf("unexpected scan summary: %s", response.Body.String())
	}
	positions, err := instance.Positions(context.Background(), paperID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("execute scan should auto-trade the selected account: %+v", positions.Items)
	}
}

func TestOrdersAPIReportsTotalAndSupportsFilters(t *testing.T) {
	handler, _ := setupRouter(t)
	paperID := accountID(t, handler, engine.AccountModePaper)
	for i := 0; i < 3; i++ {
		order := request(t, handler, http.MethodPost, "/api/v1/accounts/"+itoa(paperID)+"/orders", map[string]any{
			"market_id": "test-1", "outcome": "Yes", "token_id": "yes", "amount": 50,
		})
		if order.Code != http.StatusCreated {
			t.Fatalf("paper order failed: %d %s", order.Code, order.Body.String())
		}
	}
	response := request(t, handler, http.MethodGet, "/api/v1/orders?account_id="+itoa(paperID)+"&status=filled&limit=2&offset=0", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("orders failed: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Items []engine.Order `json:"items"`
		Total int            `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 3 || len(payload.Items) != 2 {
		t.Fatalf("unexpected orders page: total=%d rows=%d", payload.Total, len(payload.Items))
	}
	rejected := request(t, handler, http.MethodGet, "/api/v1/orders?account_id="+itoa(paperID)+"&status=rejected", nil)
	if rejected.Code != http.StatusOK {
		t.Fatalf("rejected filter failed: %d %s", rejected.Code, rejected.Body.String())
	}
	if !bytes.Contains(rejected.Body.Bytes(), []byte(`"total":0`)) {
		t.Fatalf("unexpected rejected response: %s", rejected.Body.String())
	}
}

func TestVenuesEndpointAndInvalidVenueFilters(t *testing.T) {
	handler, _ := setupRouter(t)
	venues := request(t, handler, http.MethodGet, "/api/v1/venues", nil)
	if venues.Code != http.StatusOK || !bytes.Contains(venues.Body.Bytes(), []byte(`"venue":"polymarket"`)) ||
		!bytes.Contains(venues.Body.Bytes(), []byte(`"currency":"USDC"`)) ||
		!bytes.Contains(venues.Body.Bytes(), []byte(`"supports_settlement":true`)) {
		t.Fatalf("unexpected venues response: %d %s", venues.Code, venues.Body.String())
	}
	for _, path := range []string{
		"/api/v1/positions?venue=binance",
		"/api/v1/orders?venue=binance",
		"/api/v1/opportunities?venue=binance",
	} {
		response := request(t, handler, http.MethodGet, path, nil)
		if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte("未知场所")) {
			t.Fatalf("%s must reject an unknown venue: %d %s", path, response.Code, response.Body.String())
		}
	}
	scan := request(t, handler, http.MethodPost, "/api/v1/scan?venue=binance", nil)
	if scan.Code != http.StatusBadRequest || !bytes.Contains(scan.Body.Bytes(), []byte("未知场所")) {
		t.Fatalf("scan must reject an unknown venue: %d %s", scan.Code, scan.Body.String())
	}
	liveTest := request(t, handler, http.MethodPost, "/api/v1/system/live/test?venue=binance", map[string]any{})
	if liveTest.Code != http.StatusBadRequest || !bytes.Contains(liveTest.Body.Bytes(), []byte("未知场所")) {
		t.Fatalf("live probe must reject an unknown venue: %d %s", liveTest.Code, liveTest.Body.String())
	}
	relayerTest := request(t, handler, http.MethodPost, "/api/v1/system/relayer/test?venue=binance", map[string]any{})
	if relayerTest.Code != http.StatusBadRequest || !bytes.Contains(relayerTest.Body.Bytes(), []byte("未知场所")) {
		t.Fatalf("relayer probe must reject an unknown venue: %d %s", relayerTest.Code, relayerTest.Body.String())
	}
	// "all" and an absent filter both mean no filter.
	all := request(t, handler, http.MethodGet, "/api/v1/opportunities?venue=all", nil)
	if all.Code != http.StatusOK {
		t.Fatalf("venue=all must be treated as no filter: %d %s", all.Code, all.Body.String())
	}
}

func TestVenueFilterAppliesBeforeOrderTotals(t *testing.T) {
	handler, _ := setupRouter(t)
	paperID := accountID(t, handler, engine.AccountModePaper)
	for i := 0; i < 3; i++ {
		order := request(t, handler, http.MethodPost, "/api/v1/accounts/"+itoa(paperID)+"/orders", map[string]any{
			"market_id": "test-1", "outcome": "Yes", "token_id": "yes", "amount": 50,
		})
		if order.Code != http.StatusCreated {
			t.Fatalf("paper order failed: %d %s", order.Code, order.Body.String())
		}
	}
	var payload struct {
		Items []engine.Order `json:"items"`
		Total int            `json:"total"`
	}
	filtered := request(t, handler, http.MethodGet,
		"/api/v1/orders?account_id="+itoa(paperID)+"&venue=polymarket&limit=2", nil)
	if filtered.Code != http.StatusOK {
		t.Fatalf("venue-filtered orders failed: %d %s", filtered.Code, filtered.Body.String())
	}
	if err := json.Unmarshal(filtered.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 3 || len(payload.Items) != 2 {
		t.Fatalf("filtered total must match the venue, got total=%d rows=%d", payload.Total, len(payload.Items))
	}
	for _, order := range payload.Items {
		if order.Venue != market.VenuePolymarket || order.Currency != market.CurrencyUSDC {
			t.Fatalf("order is missing its venue/currency: %+v", order)
		}
	}
	other := request(t, handler, http.MethodGet,
		"/api/v1/orders?account_id="+itoa(paperID)+"&venue=predictfun", nil)
	payload = struct {
		Items []engine.Order `json:"items"`
		Total int            `json:"total"`
	}{}
	if err := json.Unmarshal(other.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 0 || len(payload.Items) != 0 {
		t.Fatalf("a different venue must return no rows: %d %s", other.Code, other.Body.String())
	}
}

func TestAccountVenueCreationAndImmutability(t *testing.T) {
	handler, _ := setupRouter(t)
	created := request(t, handler, http.MethodPost, "/api/v1/accounts", map[string]any{
		"name": "Predict.fun 模拟", "mode": "paper", "venue": "predictfun",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("creating a predictfun account failed: %d %s", created.Code, created.Body.String())
	}
	var account engine.Account
	if err := json.Unmarshal(created.Body.Bytes(), &account); err != nil {
		t.Fatal(err)
	}
	if account.Venue != market.VenuePredictFun || account.Currency != market.CurrencyUSDT {
		t.Fatalf("unexpected account venue/currency: %+v", account)
	}
	patch := request(t, handler, http.MethodPatch, "/api/v1/accounts/"+itoa(account.ID), map[string]any{
		"venue": "polymarket",
	})
	if patch.Code != http.StatusBadRequest || !bytes.Contains(patch.Body.Bytes(), []byte("账户所属场所不能修改")) {
		t.Fatalf("changing the venue must be rejected: %d %s", patch.Code, patch.Body.String())
	}
	secondPM := request(t, handler, http.MethodPost, "/api/v1/accounts", map[string]any{
		"name": "第二个 Polymarket 实盘", "mode": "live", "venue": "polymarket",
	})
	if secondPM.Code != http.StatusConflict {
		t.Fatalf("a second Polymarket live account must conflict: %d %s", secondPM.Code, secondPM.Body.String())
	}
	pfLive := request(t, handler, http.MethodPost, "/api/v1/accounts", map[string]any{
		"name": "Predict.fun 实盘", "mode": "live", "venue": "predictfun",
	})
	if pfLive.Code != http.StatusCreated {
		t.Fatalf("a Predict.fun live account may coexist: %d %s", pfLive.Code, pfLive.Body.String())
	}
}

func TestSettingsVenueOverridesSurviveLegacyPayloads(t *testing.T) {
	handler, instance := setupRouter(t)
	settings := instance.Settings()
	settings.Venues[market.VenuePredictFun] = engine.VenueSettings{
		Enabled: true, OrderSize: 3, MaxPosition: 15, MaxExposure: 45, FeeBPS: 200,
	}
	update := request(t, handler, http.MethodPut, "/api/v1/settings", settings)
	if update.Code != http.StatusOK {
		t.Fatalf("saving venue overrides failed: %d %s", update.Code, update.Body.String())
	}
	// An old client sends only the flat fields. The venue map must be preserved
	// by the read-modify-write path instead of being dropped.
	legacy := instance.Settings()
	legacy.OrderSize = 42
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	delete(payload, "venues")
	response := request(t, handler, http.MethodPut, "/api/v1/settings", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("legacy settings update failed: %d %s", response.Code, response.Body.String())
	}
	stored := instance.Settings()
	if stored.OrderSize != 42 {
		t.Fatalf("flat field was not applied: %+v", stored)
	}
	if override := stored.Venues[market.VenuePredictFun]; !override.Enabled || override.OrderSize != 3 || override.MaxExposure != 45 {
		t.Fatalf("legacy payload erased venue overrides: %+v", stored.Venues)
	}
	invalid := instance.Settings()
	invalid.Venues[market.VenuePredictFun] = engine.VenueSettings{
		Enabled: true, OrderSize: 10, MaxPosition: 5, MaxExposure: 20,
	}
	rejected := request(t, handler, http.MethodPut, "/api/v1/settings", invalid)
	if rejected.Code != http.StatusBadRequest || !bytes.Contains(rejected.Body.Bytes(), []byte("Predict.fun")) {
		t.Fatalf("invalid venue caps must be rejected with the venue name: %d %s", rejected.Code, rejected.Body.String())
	}
}

func TestOpportunitiesResponseCarriesVenues(t *testing.T) {
	handler, _ := setupRouter(t)
	scan := request(t, handler, http.MethodPost, "/api/v1/scan", nil)
	if scan.Code != http.StatusOK {
		t.Fatalf("scan failed: %d %s", scan.Code, scan.Body.String())
	}
	var payload struct {
		Items  []engine.Opportunity `json:"items"`
		Venues []market.Venue       `json:"venues"`
	}
	if err := json.Unmarshal(scan.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 || payload.Items[0].Venue != market.VenuePolymarket {
		t.Fatalf("unexpected opportunities: %s", scan.Body.String())
	}
	if len(payload.Venues) != 1 || payload.Venues[0] != market.VenuePolymarket {
		t.Fatalf("response must report contributing venues: %s", scan.Body.String())
	}
	if payload.Items[0].Currency != market.CurrencyUSDC {
		t.Fatalf("opportunity must carry its currency: %+v", payload.Items[0])
	}
}

func TestOrderRejectsVenueMismatchAndUnconfiguredLive(t *testing.T) {
	handler, _ := setupRouter(t)
	created := request(t, handler, http.MethodPost, "/api/v1/accounts", map[string]any{
		"name": "PF paper", "mode": "paper", "venue": "predictfun",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("creating predictfun account failed: %d %s", created.Code, created.Body.String())
	}
	var account engine.Account
	if err := json.Unmarshal(created.Body.Bytes(), &account); err != nil {
		t.Fatal(err)
	}
	mismatch := request(t, handler, http.MethodPost, "/api/v1/accounts/"+itoa(account.ID)+"/orders", map[string]any{
		"market_id": "test-1", "outcome": "Yes", "token_id": "yes", "amount": 10, "venue": "polymarket",
	})
	if mismatch.Code != http.StatusBadRequest || !bytes.Contains(mismatch.Body.Bytes(), []byte("该机会不属于此账户的场所")) {
		t.Fatalf("cross-venue order must be a 400: %d %s", mismatch.Code, mismatch.Body.String())
	}

	// An executor that exists but has no credentials must return the venue's
	// own readiness message with a 409, not a generic 500/503.
	unconfigured, _ := setupRouterWithLive(t, quoteLive{})
	liveID := accountID(t, unconfigured, engine.AccountModeLive)
	response := request(t, unconfigured, http.MethodPost, "/api/v1/accounts/"+itoa(liveID)+"/orders", map[string]any{
		"market_id": "test-1", "outcome": "Yes", "token_id": "yes", "amount": 10,
	})
	if response.Code != http.StatusConflict ||
		!bytes.Contains(response.Body.Bytes(), []byte("实盘未配置")) ||
		!bytes.Contains(response.Body.Bytes(), []byte("未配置 POLYMARKET_PRIVATE_KEY")) {
		t.Fatalf("unconfigured live must be a 409 with the readiness text: %d %s", response.Code, response.Body.String())
	}
}
