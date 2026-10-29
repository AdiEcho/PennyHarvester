package engine_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/store"
)

type fakeProvider struct {
	markets map[string]market.Market
	err     error
}

func (f *fakeProvider) Name() string { return "test" }
func (f *fakeProvider) ListOpen(context.Context, int) ([]market.Market, error) {
	if f.err != nil {
		return nil, f.err
	}
	items := []market.Market{}
	for _, item := range f.markets {
		if !item.Closed {
			items = append(items, item)
		}
	}
	return items, nil
}
func (f *fakeProvider) Get(_ context.Context, id string) (market.Market, error) {
	if f.err != nil {
		return market.Market{}, f.err
	}
	item, ok := f.markets[id]
	if !ok {
		return market.Market{}, errors.New("not found")
	}
	return item, nil
}

type fakeLive struct {
	balance float64
	fill    engine.LiveFill
	err     error
}

func (f *fakeLive) Configured() bool   { return true }
func (f *fakeLive) Signer() string     { return "0x0000000000000000000000000000000000000001" }
func (f *fakeLive) ReadyError() string { return "" }
func (f *fakeLive) Buy(context.Context, string, float64, float64) (engine.LiveFill, error) {
	return f.fill, f.err
}
func (f *fakeLive) Collateral(context.Context) (engine.CashSnapshot, error) {
	return engine.CashSnapshot{Amount: f.balance, At: time.Now().UTC()}, nil
}

func binary(id string, price, liquidity float64, days int) market.Market {
	return market.Market{
		ID: id, ConditionID: "condition-" + id, Question: "Question " + id,
		Slug: id, Active: true, EndDate: time.Now().UTC().AddDate(0, 0, days),
		Liquidity: liquidity, Volume: 50000,
		Outcomes: []market.Outcome{
			{Name: "Yes", TokenID: id + "-yes", Price: price},
			{Name: "No", TokenID: id + "-no", Price: 1 - price},
		},
	}
}

func newTestEngine(t *testing.T, provider market.Provider, live engine.LiveExecutor) (*engine.Engine, *store.SQLite) {
	t.Helper()
	repository, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	instance, err := engine.New(context.Background(), repository, provider, live)
	if err != nil {
		t.Fatal(err)
	}
	return instance, repository
}

func accountByMode(t *testing.T, instance *engine.Engine, mode engine.AccountMode) engine.Account {
	t.Helper()
	accounts, err := instance.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts.Items {
		if account.Mode == mode {
			return account
		}
	}
	t.Fatalf("no %s account found in %+v", mode, accounts.Items)
	return engine.Account{}
}

func boolPointer(value bool) *bool { return &value }

func TestDefaultAccountIsLiveAndPaperIsSeparate(t *testing.T) {
	instance, _ := newTestEngine(t, &fakeProvider{markets: map[string]market.Market{}}, nil)
	accounts, err := instance.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if accounts.Total < 2 {
		t.Fatalf("expected seeded live and paper accounts, got %+v", accounts.Items)
	}
	var live, paper int
	for _, account := range accounts.Items {
		if account.IsDefault {
			if account.Mode != engine.AccountModeLive {
				t.Fatalf("default account must be live, got %+v", account)
			}
			live++
		}
		if account.Mode == engine.AccountModePaper {
			paper++
		}
	}
	if live != 1 || paper < 1 {
		t.Fatalf("expected one default live and at least one paper account, got %+v", accounts.Items)
	}
}

func TestScanFiltersAndSettlesPaperPosition(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"eligible": binary("eligible", 0.98, 20000, 2),
		"cheap":    binary("cheap", 0.94, 20000, 2),
		"thin":     binary("thin", 0.98, 100, 2),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	opportunities := instance.Opportunities()
	if opportunities.Total != 1 || opportunities.Items[0].MarketID != "eligible" {
		t.Fatalf("unexpected opportunities: %+v", opportunities)
	}

	position, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{
		MarketID: "eligible", Outcome: "Yes", TokenID: "eligible-yes", Amount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if position.AccountID != paper.ID {
		t.Fatalf("position saved to wrong account: %+v", position)
	}
	if math.Abs(position.EntryPrice-0.98196) > 1e-9 {
		t.Fatalf("slippage not applied: %f", position.EntryPrice)
	}
	if position.Shares <= 100 || position.Status != "open" {
		t.Fatalf("invalid paper fill: %+v", position)
	}

	resolved := provider.markets["eligible"]
	resolved.Closed = true
	resolved.Active = false
	resolved.ResolutionStatus = "resolved"
	resolved.Outcomes[0].Price = 1
	resolved.Outcomes[1].Price = 0
	provider.markets["eligible"] = resolved
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err := instance.Positions(context.Background(), paper.ID, "won")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 || positions.Items[0].RealizedPNL <= 0 {
		t.Fatalf("position was not settled with a win: %+v", positions)
	}
	dashboard, err := instance.Dashboard(context.Background(), paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard.Metrics.Cash <= 10000 || dashboard.Metrics.OpenPositions != 0 || dashboard.Metrics.WinRate != 1 {
		t.Fatalf("unexpected metrics: %+v", dashboard.Metrics)
	}
}

func TestPaperOrderRiskAndSlippageRejection(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"at-max": binary("at-max", 0.99, 20000, 2),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	_, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{
		MarketID: "at-max", Outcome: "Yes", TokenID: "at-max-yes", Amount: 100,
	})
	if !errors.Is(err, engine.ErrInvalidOrder) {
		t.Fatalf("expected slippage rejection, got %v", err)
	}
	orders, err := instance.Orders(context.Background(), paper.ID, 10)
	if err != nil || orders.Total != 1 || orders.Items[0].Status != "rejected" {
		t.Fatalf("rejection was not logged: %+v, %v", orders, err)
	}
}

func TestAutoPaperTradeHonorsExposureAndDoesNotDuplicate(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"one": binary("one", 0.98, 20000, 2),
		"two": binary("two", 0.97, 20000, 3),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil); err != nil {
		t.Fatal(err)
	}
	settings := instance.Settings()
	settings.MaxExposure = 150
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := instance.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	positions, err := instance.Positions(context.Background(), paper.ID, "open")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("expected exactly one capped position, got %+v", positions.Items)
	}
}

func TestAutoTradeRejectionDoesNotAbortScan(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"one": binary("one", 0.98, 20000, 2),
		"two": binary("two", 0.97, 20000, 3),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil); err != nil {
		t.Fatal(err)
	}
	settings := instance.Settings()
	settings.InitialBankroll = 50
	settings.OrderSize = 100
	settings.MaxPosition = 250
	settings.MaxExposure = 1000
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatalf("risk rejections must not fail the scan: %v", err)
	}
	positions, _ := instance.Positions(context.Background(), paper.ID, "open")
	if positions.Total != 0 {
		t.Fatalf("expected no positions without cash, got %+v", positions.Items)
	}
	orders, _ := instance.Orders(context.Background(), paper.ID, 10)
	if orders.Total == 0 {
		t.Fatal("risk rejections should still be logged")
	}
	for _, order := range orders.Items {
		if order.Status != "rejected" || order.Reason == "" {
			t.Fatalf("unexpected order: %+v", order)
		}
	}
}

func TestUpstreamFailureKeepsPositionsOpen(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	_, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100})
	if err != nil {
		t.Fatal(err)
	}
	provider.err = errors.New("offline")
	if err := instance.Scan(context.Background()); !errors.Is(err, engine.ErrUpstream) {
		t.Fatalf("expected upstream error, got %v", err)
	}
	positions, _ := instance.Positions(context.Background(), paper.ID, "open")
	if positions.Total != 1 {
		t.Fatal("an upstream failure must not close positions")
	}
}

func TestLiveOrderRefusedWithoutCredentials(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"one": binary("one", 0.98, 20000, 2),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	liveAccount := accountByMode(t, instance, engine.AccountModeLive)
	_, err := instance.PlaceOrder(context.Background(), liveAccount.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	})
	if !errors.Is(err, engine.ErrLiveUnavailable) {
		t.Fatalf("expected live unavailable, got %v", err)
	}
	orders, err := instance.Orders(context.Background(), liveAccount.ID, 10)
	if err != nil || orders.Total != 1 || orders.Items[0].Status != "rejected" {
		t.Fatalf("live rejection was not logged: %+v, %v", orders, err)
	}
}

func TestLiveOrderRecordsExchangeFill(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"one": binary("one", 0.98, 20000, 2),
	}}
	executor := &fakeLive{
		balance: 1000,
		fill: engine.LiveFill{
			OrderID: "clob-order-1", Shares: 101.5, Cost: 99.5, Price: 0.9803, Status: "matched",
		},
	}
	instance, _ := newTestEngine(t, provider, executor)
	liveAccount := accountByMode(t, instance, engine.AccountModeLive)
	position, err := instance.PlaceOrder(context.Background(), liveAccount.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if position.AccountID != liveAccount.ID || math.Abs(position.Shares-101.5) > 1e-9 {
		t.Fatalf("unexpected live position: %+v", position)
	}
	orders, err := instance.Orders(context.Background(), liveAccount.ID, 10)
	if err != nil || orders.Total != 1 || orders.Items[0].ExternalID != "clob-order-1" {
		t.Fatalf("live fill was not logged: %+v, %v", orders, err)
	}
	dashboard, err := instance.Dashboard(context.Background(), liveAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard.Metrics.WalletCash != 1000 || dashboard.Metrics.OpenPositions != 1 || dashboard.Metrics.Cash <= 0 {
		t.Fatalf("unexpected live metrics: %+v", dashboard.Metrics)
	}
}
