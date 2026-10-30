package engine_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
	"pennyharvester/backend/internal/store"
)

type fakeProvider struct {
	markets   map[string]market.Market
	localized map[string]market.Market
	err       error
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

func (f *fakeProvider) MarketsLocale(_ context.Context, ids []string, locale string) ([]market.Market, error) {
	if locale != "zh" || f.localized == nil {
		return nil, nil
	}
	items := make([]market.Market, 0, len(ids))
	for _, id := range ids {
		if item, ok := f.localized[id]; ok {
			items = append(items, item)
		}
	}
	return items, nil
}

type fakeLive struct {
	balance   float64
	allowance float64
	fee       float64
	fill      engine.LiveFill
	quote     engine.LiveQuote
	quoteErr  error
	err       error
	lookup    map[string]engine.LiveFill
	reloads   int
}

func (f *fakeLive) ReloadProxy() { f.reloads++ }

func (f *fakeLive) Configured() bool   { return true }
func (f *fakeLive) Signer() string     { return "0x0000000000000000000000000000000000000001" }
func (f *fakeLive) Maker() string      { return "0x0000000000000000000000000000000000000002" }
func (f *fakeLive) ReadyError() string { return "" }
func (f *fakeLive) Buy(_ context.Context, _, _ string, _, _ float64, onSigned func(string) error) (engine.LiveFill, error) {
	orderID := f.fill.OrderID
	if orderID == "" {
		orderID = "fake-order"
	}
	if onSigned != nil {
		if err := onSigned(orderID); err != nil {
			return engine.LiveFill{}, err
		}
	}
	return f.fill, f.err
}
func (f *fakeLive) LookupOrder(_ context.Context, orderID, _ string) (engine.LiveFill, bool, error) {
	fill, ok := f.lookup[orderID]
	return fill, ok, nil
}
func (f *fakeLive) EstimateBuyFee(context.Context, string, float64, float64) (float64, error) {
	return f.fee, nil
}
func (f *fakeLive) Quote(context.Context, string, string, float64) (engine.LiveQuote, error) {
	return f.quote, f.quoteErr
}
func (f *fakeLive) Collateral(context.Context) (engine.CashSnapshot, error) {
	return engine.CashSnapshot{Amount: f.balance, Allowance: f.allowance, At: time.Now().UTC()}, nil
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
	instance, err := engine.New(context.Background(), repository, provider, live, nil)
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

func TestQuoteOpportunityUsesRealBookAndFee(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"eligible": binary("eligible", 0.98, 20000, 2),
	}}
	live := &fakeLive{quote: engine.LiveQuote{
		BestAsk: 0.981, LimitPrice: 0.982, FillPrice: 0.982, Shares: 101.8329, Notional: 99.9999,
		Fee: 0.09, FeeRate: 0.05, AvailableNotional: 50000, FullyFilled: true,
	}}
	instance, _ := newTestEngine(t, provider, live)
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}

	quote, err := instance.QuoteOpportunity(context.Background(), "eligible-yes", 0)
	if err != nil {
		t.Fatal(err)
	}
	if quote.Amount != 100 {
		t.Fatalf("an empty amount should use the configured order size, got %f", quote.Amount)
	}
	if quote.FillPrice != 0.982 || quote.Fee != 0.09 || quote.FeeRate != 0.05 {
		t.Fatalf("real book and fee were not carried through: %+v", quote)
	}
	wantCost := 99.9999 + 0.09
	wantProfit := 101.8329 - wantCost
	if math.Abs(quote.TotalCost-wantCost) > 1e-9 || math.Abs(quote.NetProfit-wantProfit) > 1e-9 {
		t.Fatalf("net math must include the taker fee: %+v", quote)
	}
	if math.Abs(quote.NetYield-wantProfit/wantCost) > 1e-9 {
		t.Fatalf("unexpected net yield: %f", quote.NetYield)
	}
	if quote.ExceedsMaxPrice {
		t.Fatalf("0.982 is inside max_price: %+v", quote)
	}

	// A real fill above the strategy's max_price must be surfaced, because the
	// live order would be rejected even though the reference price passed.
	live.quote.FillPrice = 0.995
	live.quote.LimitPrice = 0.995
	quote, err = instance.QuoteOpportunity(context.Background(), "eligible-yes", 50)
	if err != nil {
		t.Fatal(err)
	}
	if !quote.ExceedsMaxPrice || quote.Amount != 50 {
		t.Fatalf("a fill above max_price must be flagged: %+v", quote)
	}

	if _, err := instance.QuoteOpportunity(context.Background(), "missing-token", 0); !errors.Is(err, engine.ErrInvalidOrder) {
		t.Fatalf("an unknown token must be rejected, got %v", err)
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

func TestScanAddsPolymarketChineseText(t *testing.T) {
	en := binary("eligible", 0.98, 20000, 2)
	zh := binary("eligible", 0.98, 20000, 2)
	zh.Question = "会如期结算吗？"
	zh.GroupItemTitle = "O/U 0.5"
	zh.EventTitle = "中文赛事 -更多市场"
	zh.Outcomes[0].Name = "是"
	zh.Outcomes[1].Name = "否"
	provider := &fakeProvider{
		markets:   map[string]market.Market{"eligible": en},
		localized: map[string]market.Market{"eligible": zh},
	}
	instance, _ := newTestEngine(t, provider, nil)
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	opportunities := instance.Opportunities()
	if opportunities.Total != 1 {
		t.Fatalf("unexpected opportunities: %+v", opportunities)
	}
	item := opportunities.Items[0]
	if item.QuestionZH != zh.Question {
		t.Fatalf("question_zh = %q, want %q", item.QuestionZH, zh.Question)
	}
	if item.OutcomeZH != "是" || item.Outcome != "Yes" {
		t.Fatalf("unexpected outcomes: canonical=%q zh=%q", item.Outcome, item.OutcomeZH)
	}
	if item.EventTitleZH != zh.EventTitle || item.GroupItemTitle != zh.GroupItemTitle {
		t.Fatalf("missing localized context: %+v", item)
	}
}

func TestPaperBuyBooksFeeAndReportsIt(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	settings := instance.Settings()
	if settings.FeeBPS <= 0 {
		t.Fatalf("paper accounts must charge a fee by default, got %+v", settings)
	}
	position, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantFee := 100 * settings.FeeBPS / 10000
	if math.Abs(position.Fee-wantFee) > 1e-9 {
		t.Fatalf("position fee = %v, want %v", position.Fee, wantFee)
	}
	if math.Abs(position.Cost-(100+wantFee)) > 1e-9 {
		t.Fatalf("fee was not added to cost: %+v", position)
	}
	if math.Abs(position.EntryPrice-0.98196) > 1e-9 {
		t.Fatalf("entry price must stay the execution price: %+v", position)
	}
	orders, err := instance.Orders(context.Background(), paper.ID, "all", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if orders.Total != 1 || math.Abs(orders.Items[0].Fee-wantFee) > 1e-9 {
		t.Fatalf("order fee was not recorded: %+v", orders.Items)
	}
	if math.Abs(orders.Items[0].Amount-position.Cost) > 1e-9 {
		t.Fatalf("a filled order amount should be the cash spent: %+v", orders.Items[0])
	}
	dashboard, err := instance.Dashboard(context.Background(), paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(dashboard.Metrics.FeesPaid-wantFee) > 1e-9 {
		t.Fatalf("dashboard fees_paid = %v, want %v", dashboard.Metrics.FeesPaid, wantFee)
	}
}

func TestUpdateSettingsForcesFeeModelVersion(t *testing.T) {
	instance, _ := newTestEngine(t, &fakeProvider{markets: map[string]market.Market{}}, nil)
	settings := instance.Settings()
	settings.FeeModelVersion = 0
	settings.FeeBPS = 0
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	stored := instance.Settings()
	if stored.FeeModelVersion != engine.FeeModelVersion {
		t.Fatalf("fee model version must be server managed: %+v", stored)
	}
	if stored.FeeBPS != 0 {
		t.Fatalf("an explicit zero fee must be kept: %+v", stored)
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
	orders, err := instance.Orders(context.Background(), paper.ID, "all", 10, 0)
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

func TestAutoTradeWithInsufficientCashSkipsWithoutSpam(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"one": binary("one", 0.98, 20000, 2),
		"two": binary("two", 0.97, 20000, 3),
	}}
	instance, repository := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateAccountBaseline(context.Background(), paper.ID, 50); err != nil {
		t.Fatal(err)
	}
	settings := instance.Settings()
	settings.OrderSize = 100
	settings.MaxPosition = 250
	settings.MaxExposure = 1000
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatalf("insufficient cash must not fail the scan: %v", err)
	}
	positions, _ := instance.Positions(context.Background(), paper.ID, "open")
	if positions.Total != 0 {
		t.Fatalf("expected no positions without cash, got %+v", positions.Items)
	}
	orders, _ := instance.Orders(context.Background(), paper.ID, "all", 10, 0)
	if orders.Total != 0 {
		t.Fatalf("automatic cash rejection should be skipped before logging: %+v", orders.Items)
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
	orders, err := instance.Orders(context.Background(), liveAccount.ID, "all", 10, 0)
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
	orders, err := instance.Orders(context.Background(), liveAccount.ID, "all", 10, 0)
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

func TestPaperBaselineIsStableWhenDefaultChanges(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := instance.Dashboard(context.Background(), paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	settings := instance.Settings()
	settings.InitialBankroll = 20000
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	after, err := instance.Dashboard(context.Background(), paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(after.Metrics.Cash-before.Metrics.Cash) > 1e-9 {
		t.Fatalf("changing the default bankroll rewrote an existing paper account: before=%+v after=%+v", before.Metrics, after.Metrics)
	}
	if math.Abs(after.Metrics.InitialBankroll-before.Metrics.InitialBankroll) > 1e-9 {
		t.Fatalf("paper baseline changed: before=%v after=%v", before.Metrics.InitialBankroll, after.Metrics.InitialBankroll)
	}
}

func TestSplitSettlementUsesSplitStatusAndManualKind(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	}); err != nil {
		t.Fatal(err)
	}
	item := provider.markets["one"]
	item.Closed = true
	item.Active = false
	item.ResolutionStatus = "resolved"
	item.Outcomes[0].Price = 0.5
	item.Outcomes[1].Price = 0.5
	provider.markets["one"] = item
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err := instance.Positions(context.Background(), paper.ID, "all")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 || positions.Items[0].Status != "split" {
		t.Fatalf("expected split position, got %+v", positions.Items)
	}
	dashboard, err := instance.Dashboard(context.Background(), paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard.Metrics.SplitPositions != 1 || dashboard.Metrics.SettledPositions != 1 || dashboard.Metrics.WinRate != 0 {
		t.Fatalf("unexpected split metrics: %+v", dashboard.Metrics)
	}
	orders, err := instance.Orders(context.Background(), paper.ID, "settled", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if orders.Total != 1 || orders.Items[0].Kind != "manual" {
		t.Fatalf("settlement must retain the original order kind: %+v", orders.Items)
	}
}

func TestScanOnlyDoesNotAutoTrade(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil); err != nil {
		t.Fatal(err)
	}
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err := instance.Positions(context.Background(), paper.ID, "open")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 0 {
		t.Fatalf("scan-only must not place orders: %+v", positions.Items)
	}
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err = instance.Positions(context.Background(), paper.ID, "open")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("full scan should auto-trade: %+v", positions.Items)
	}
}

func TestOrdersTotalAndStatusFilter(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	for i := 0; i < 3; i++ {
		if _, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{
			MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 50,
		}); err != nil {
			t.Fatal(err)
		}
	}
	orders, err := instance.Orders(context.Background(), paper.ID, "all", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if orders.Total != 3 || len(orders.Items) != 2 {
		t.Fatalf("expected total 3 and two rows, got total=%d rows=%d", orders.Total, len(orders.Items))
	}
	filled, err := instance.Orders(context.Background(), paper.ID, "filled", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if filled.Total != 3 {
		t.Fatalf("expected 3 filled orders, got %d", filled.Total)
	}
	rejected, err := instance.Orders(context.Background(), paper.ID, "rejected", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Total != 0 {
		t.Fatalf("expected no rejected orders, got %d", rejected.Total)
	}
}

func TestLiveAllowanceIsCheckedBeforeSubmit(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	executor := &fakeLive{
		balance: 1000, allowance: 50,
		fill: engine.LiveFill{OrderID: "should-not-submit", Shares: 100, Cost: 98, Price: 0.98, Status: "matched"},
	}
	instance, _ := newTestEngine(t, provider, executor)
	liveAccount := accountByMode(t, instance, engine.AccountModeLive)
	_, err := instance.PlaceOrder(context.Background(), liveAccount.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	})
	if !errors.Is(err, engine.ErrInvalidOrder) {
		t.Fatalf("expected allowance rejection, got %v", err)
	}
	orders, err := instance.Orders(context.Background(), liveAccount.ID, "all", 10, 0)
	if err != nil || orders.Total != 1 || orders.Items[0].Status != "rejected" {
		t.Fatalf("allowance rejection was not logged: %+v, %v", orders, err)
	}
	if !strings.Contains(orders.Items[0].Reason, "授权") {
		t.Fatalf("unexpected rejection reason: %s", orders.Items[0].Reason)
	}
}

func TestUnknownLiveOrderIsReconciled(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	executor := &fakeLive{
		balance: 1000, allowance: 1000,
		fill: engine.LiveFill{OrderID: "pending-1", Status: "unknown"},
		err:  errors.New("timeout"),
	}
	instance, _ := newTestEngine(t, provider, executor)
	liveAccount := accountByMode(t, instance, engine.AccountModeLive)
	_, err := instance.PlaceOrder(context.Background(), liveAccount.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	})
	if !errors.Is(err, engine.ErrUpstream) {
		t.Fatalf("expected upstream error for an unknown live order, got %v", err)
	}
	orders, err := instance.Orders(context.Background(), liveAccount.ID, "unresolved", 10, 0)
	if err != nil || orders.Total != 1 || orders.Items[0].Status != "unknown" {
		t.Fatalf("unknown order was not persisted: %+v, %v", orders, err)
	}
	executor.err = nil
	executor.fill = engine.LiveFill{OrderID: "pending-1", Shares: 100, Cost: 98, Price: 0.98, Status: "matched"}
	executor.lookup = map[string]engine.LiveFill{"pending-1": executor.fill}
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err := instance.Positions(context.Background(), liveAccount.ID, "open")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 || math.Abs(positions.Items[0].Shares-100) > 1e-9 {
		t.Fatalf("reconciliation did not create the position: %+v", positions.Items)
	}
	orders, err = instance.Orders(context.Background(), liveAccount.ID, "all", 10, 0)
	if err != nil || orders.Total != 1 || orders.Items[0].Status != "filled" || orders.Items[0].ExternalID != "pending-1" {
		t.Fatalf("reconciliation did not finalize the order: %+v, %v", orders, err)
	}
}

func TestUpdateSystemSettingsAppliesProxyAndReloadsLive(t *testing.T) {
	ctx := context.Background()
	executor := &fakeLive{}
	instance, repository := newTestEngine(t, &fakeProvider{markets: map[string]market.Market{}}, executor)

	if instance.SystemSettings().Proxy.Mode != netproxy.ModeEnvironment {
		t.Fatalf("default system settings should follow the environment")
	}

	custom := engine.SystemSettings{Proxy: netproxy.Config{
		Mode: netproxy.ModeCustom, URL: "http://127.0.0.1:7890", NoProxy: "localhost",
	}}
	if err := instance.UpdateSystemSettings(ctx, custom); err != nil {
		t.Fatal(err)
	}
	if got := instance.SystemSettings(); got.Proxy.URL != custom.Proxy.URL || got.Proxy.Mode != netproxy.ModeCustom {
		t.Fatalf("system settings were not applied: %+v", got)
	}
	if executor.reloads != 1 {
		t.Fatalf("live executor reloads = %d, want 1", executor.reloads)
	}
	stored, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Proxy.URL != custom.Proxy.URL {
		t.Fatalf("stored proxy = %q", stored.Proxy.URL)
	}

	if err := instance.UpdateSystemSettings(ctx, engine.SystemSettings{Proxy: netproxy.Config{Mode: "sometimes"}}); err == nil {
		t.Fatalf("an invalid proxy mode must be rejected")
	}
	if executor.reloads != 1 {
		t.Fatalf("a rejected update must not reload the live executor")
	}
}

type runtimeProvider struct {
	*fakeProvider
	baseURL    string
	defaultURL string
}

func (p *runtimeProvider) SetBaseURL(value string) error {
	if value == "" {
		value = p.defaultURL
	}
	p.baseURL = value
	return nil
}

func (p *runtimeProvider) BaseURL() string        { return p.baseURL }
func (p *runtimeProvider) DefaultBaseURL() string { return p.defaultURL }

type runtimeLive struct {
	*fakeLive
	clobURL     string
	defaultCLOB string
	settings    engine.LiveSettings
	probeURL    string
	probeErr    error
	relayerKey  string
}

func (l *runtimeLive) UpdateSettings(settings engine.LiveSettings, clobURL string) error {
	if clobURL == "" {
		clobURL = l.defaultCLOB
	}
	l.clobURL = clobURL
	l.settings = settings
	return nil
}

func (l *runtimeLive) EffectiveCLOBURL() string { return l.clobURL }

func (l *runtimeLive) Environment() engine.LiveEnvironment {
	return engine.LiveEnvironment{
		CLOBBaseURL:   l.defaultCLOB,
		PrivateKeySet: true,
		SignatureType: 0,
	}
}

func intPointer(value int) *int { return &value }

func TestUpdateSystemSettingsAppliesEndpointsAndLiveOverrides(t *testing.T) {
	ctx := context.Background()
	provider := &runtimeProvider{
		fakeProvider: &fakeProvider{markets: map[string]market.Market{}},
		baseURL:      "https://default-gamma.example",
		defaultURL:   "https://default-gamma.example",
	}
	live := &runtimeLive{
		fakeLive:    &fakeLive{},
		clobURL:     "https://default-clob.example",
		defaultCLOB: "https://default-clob.example",
	}
	instance, repository := newTestEngine(t, provider, live)

	if provider.BaseURL() != provider.DefaultBaseURL() || live.EffectiveCLOBURL() != live.defaultCLOB {
		t.Fatalf("engine startup should inherit defaults: gamma=%q clob=%q", provider.BaseURL(), live.EffectiveCLOBURL())
	}

	key := "0x" + strings.Repeat("1", 64)
	settings := engine.SystemSettings{
		Proxy: netproxy.DefaultConfig(),
		DataSource: engine.DataSourceSettings{
			GammaBaseURL: "https://gamma.example.com",
			CLOBBaseURL:  "https://clob.example.com",
		},
		Live: engine.LiveSettings{PrivateKey: key, SignatureType: intPointer(0)},
	}
	if err := instance.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if provider.BaseURL() != settings.DataSource.GammaBaseURL {
		t.Fatalf("gamma URL = %q", provider.BaseURL())
	}
	if live.EffectiveCLOBURL() != settings.DataSource.CLOBBaseURL {
		t.Fatalf("clob URL = %q", live.EffectiveCLOBURL())
	}
	if live.settings.PrivateKey != key {
		t.Fatalf("live settings were not applied: %+v", live.settings)
	}
	runtime := instance.SystemRuntime()
	if runtime.GammaBaseURL != settings.DataSource.GammaBaseURL || runtime.CLOBBaseURL != settings.DataSource.CLOBBaseURL {
		t.Fatalf("runtime endpoints = %+v", runtime)
	}
	if runtime.GammaDefault != provider.DefaultBaseURL() || runtime.CLOBDefault != live.defaultCLOB {
		t.Fatalf("runtime defaults = %+v", runtime)
	}
	stored, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Live.PrivateKey != key || stored.DataSource.GammaBaseURL != settings.DataSource.GammaBaseURL {
		t.Fatalf("stored settings = %+v", stored)
	}

	// Clearing the overrides falls back to the startup values.
	cleared := engine.SystemSettings{Proxy: netproxy.DefaultConfig()}
	if err := instance.UpdateSystemSettings(ctx, cleared); err != nil {
		t.Fatal(err)
	}
	if provider.BaseURL() != provider.DefaultBaseURL() || live.EffectiveCLOBURL() != live.defaultCLOB {
		t.Fatalf("cleared overrides did not restore defaults: gamma=%q clob=%q", provider.BaseURL(), live.EffectiveCLOBURL())
	}

	invalid := cleared
	invalid.Live.PrivateKey = "not-a-key"
	if err := instance.UpdateSystemSettings(ctx, invalid); err == nil {
		t.Fatal("an invalid private key must be rejected")
	}
}

func (l *runtimeLive) Probe(_ context.Context, settings engine.LiveSettings, clobURL string) (engine.LiveProbe, error) {
	l.settings = settings
	l.probeURL = clobURL
	if l.probeErr != nil {
		return engine.LiveProbe{}, l.probeErr
	}
	return engine.LiveProbe{
		Signer: "0x0000000000000000000000000000000000000001",
		Maker:  "0x0000000000000000000000000000000000000002",
		APIKey: "abcd1234…", Source: "derived",
		Cash: 42, Allowance: 42, LatencyMS: 12,
	}, nil
}

func (l *runtimeLive) ProbeRelayer(_ context.Context, settings engine.LiveSettings, clobURL, apiKey, address string) (engine.RelayerProbe, error) {
	l.settings = settings
	l.probeURL = clobURL
	l.relayerKey = apiKey
	return engine.RelayerProbe{
		Signer:  "0x0000000000000000000000000000000000000001",
		Address: address, APIKey: "01967c03…", KeyCount: 2, LatencyMS: 8,
	}, nil
}

func (l *runtimeLive) DiscoverFunder(_ context.Context, settings engine.LiveSettings, clobURL string) ([]string, error) {
	l.settings = settings
	l.probeURL = clobURL
	return []string{"0x0000000000000000000000000000000000000003"}, nil
}

func TestDiscoverFunderUsesStoredSigner(t *testing.T) {
	ctx := context.Background()
	live := &runtimeLive{fakeLive: &fakeLive{}}
	instance, _ := newTestEngine(t, &fakeProvider{markets: map[string]market.Market{}}, live)
	key := "0x" + strings.Repeat("1", 64)
	if err := instance.UpdateSystemSettings(ctx, engine.SystemSettings{
		Proxy: netproxy.DefaultConfig(),
		Live:  engine.LiveSettings{PrivateKey: key, SignatureType: intPointer(0)},
	}); err != nil {
		t.Fatal(err)
	}
	wallets, err := instance.DiscoverFunder(ctx, instance.SystemSettings().Live.Redacted(), "https://clob.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(wallets) != 1 || live.settings.PrivateKey != key {
		t.Fatalf("unexpected discovery: %v %+v", wallets, live.settings)
	}
}

func TestProbeLiveRestoresStoredSecrets(t *testing.T) {
	ctx := context.Background()
	live := &runtimeLive{fakeLive: &fakeLive{}}
	instance, _ := newTestEngine(t, &fakeProvider{markets: map[string]market.Market{}}, live)

	key := "0x" + strings.Repeat("1", 64)
	stored := engine.SystemSettings{
		Proxy: netproxy.DefaultConfig(),
		Live: engine.LiveSettings{
			PrivateKey: key, SignatureType: intPointer(0),
			APIKey: "stored-key", APISecret: "stored-secret", APIPassphrase: "stored-passphrase",
		},
	}
	if err := instance.UpdateSystemSettings(ctx, stored); err != nil {
		t.Fatal(err)
	}
	// The browser only ever sees masked secrets; echoing them back must not
	// wipe the stored values before the probe runs.
	masked := stored.Live.Redacted()
	probe, err := instance.ProbeLive(ctx, masked, "https://clob.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if probe.Cash != 42 || probe.Source != "derived" {
		t.Fatalf("unexpected probe: %+v", probe)
	}
	if live.settings.APISecret != "stored-secret" || live.settings.APIPassphrase != "stored-passphrase" {
		t.Fatalf("masked secrets were not restored: %+v", live.settings)
	}
	if live.probeURL != "https://clob.example.com" {
		t.Fatalf("probe CLOB URL = %q", live.probeURL)
	}
}

func TestProbeLiveRejectsPartialCredentials(t *testing.T) {
	live := &runtimeLive{fakeLive: &fakeLive{}}
	instance, _ := newTestEngine(t, &fakeProvider{markets: map[string]market.Market{}}, live)
	_, err := instance.ProbeLive(context.Background(), engine.LiveSettings{APIKey: "only-key"}, "")
	if err == nil || !errors.Is(err, engine.ErrInvalidSettings) {
		t.Fatalf("partial credentials must be rejected as invalid settings: %v", err)
	}
}

func TestProbeRelayerUsesStoredSignerSettings(t *testing.T) {
	ctx := context.Background()
	live := &runtimeLive{fakeLive: &fakeLive{}}
	instance, _ := newTestEngine(t, &fakeProvider{markets: map[string]market.Market{}}, live)
	key := "0x" + strings.Repeat("1", 64)
	if err := instance.UpdateSystemSettings(ctx, engine.SystemSettings{
		Proxy: netproxy.DefaultConfig(),
		Live:  engine.LiveSettings{PrivateKey: key, SignatureType: intPointer(0)},
	}); err != nil {
		t.Fatal(err)
	}
	probe, err := instance.ProbeRelayer(ctx, instance.SystemSettings().Live.Redacted(), "https://clob.example.com", "relayer-key", "0x0000000000000000000000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if probe.KeyCount != 2 || live.relayerKey != "relayer-key" {
		t.Fatalf("unexpected relayer probe: %+v key=%q", probe, live.relayerKey)
	}
	if live.settings.PrivateKey != key {
		t.Fatalf("stored private key was not restored: %+v", live.settings)
	}
}
