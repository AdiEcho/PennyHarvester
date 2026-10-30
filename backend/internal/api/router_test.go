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

	"github.com/gin-gonic/gin"
)

type fixtureProvider struct {
	markets map[string]market.Market
}

func (f *fixtureProvider) Name() string { return "fixture" }

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
			EndDate: time.Now().UTC().AddDate(0, 0, 3),
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

func setupRouterWithLive(t *testing.T, live engine.LiveExecutor) (http.Handler, *engine.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repository, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	instance, err := engine.New(context.Background(), repository, testProvider(), live, nil)
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
	if response.Code != http.StatusServiceUnavailable {
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
	positions, err := instance.Positions(context.Background(), paperID, "open")
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
	positions, err = instance.Positions(context.Background(), paperID, "open")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("execute scan did not auto-trade: %+v", positions.Items)
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
