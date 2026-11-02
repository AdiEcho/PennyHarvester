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
	"pennyharvester/backend/internal/venue"
)

type fakeProvider struct {
	// venue lets a test model a second exchange. Empty means Polymarket.
	venue     market.Venue
	markets   map[string]market.Market
	localized map[string]market.Market
	eventTags map[string][]market.Tag
	err       error
}

func (f *fakeProvider) Name() string { return "test" }

// Venue is required by venue.MarketData; the fakes model Polymarket unless a
// test builds a venue-specific provider.
func (f *fakeProvider) Venue() market.Venue {
	if f.venue == "" {
		return market.VenuePolymarket
	}
	return f.venue
}
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

func (f *fakeProvider) EventTags(_ context.Context, ids []string) (map[string][]market.Tag, error) {
	items := make(map[string][]market.Tag, len(ids))
	for _, id := range ids {
		if tags, ok := f.eventTags[id]; ok {
			items[id] = tags
		}
	}
	return items, nil
}

type fakeLive struct {
	balance   float64
	allowance float64
	fee       float64
	fill      engine.LiveFill
	sellFill  engine.LiveFill
	sellErr   error
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
func (f *fakeLive) Sell(_ context.Context, _, _ string, shares float64, onSigned func(string) error) (engine.LiveFill, error) {
	orderID := f.sellFill.OrderID
	if orderID == "" {
		orderID = "fake-sell"
	}
	if onSigned != nil {
		if err := onSigned(orderID); err != nil {
			return engine.LiveFill{}, err
		}
	}
	fill := f.sellFill
	if fill.Shares == 0 && f.sellErr == nil {
		fill.Shares = shares
	}
	return fill, f.sellErr
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

// nearClose rewrites a test market's end date relative to now so automatic
// entry-window tests do not have to use multi-day fixtures.
func nearClose(item market.Market, remaining time.Duration) market.Market {
	item.EndDate = time.Now().UTC().Add(remaining)
	return item
}

func newTestEngine(t *testing.T, provider market.Provider, live engine.LiveExecutor) (*engine.Engine, *store.SQLite) {
	t.Helper()
	data, ok := provider.(venue.MarketData)
	if !ok {
		t.Fatalf("test provider %T does not implement venue.MarketData", provider)
	}
	registry := venue.NewRegistry()
	if err := registry.Register(testCapabilities(data.Venue()), data, live); err != nil {
		t.Fatal(err)
	}
	return newTestEngineWithRegistry(t, registry)
}

// newTestEngineWithRegistry builds an engine over an arbitrary venue set. Tests
// that model two venues use it directly.
func newTestEngineWithRegistry(t *testing.T, registry *venue.Registry) (*engine.Engine, *store.SQLite) {
	t.Helper()
	repository, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	instance, err := engine.New(context.Background(), repository, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	return instance, repository
}

// testCapabilities is the Polymarket-shaped fact sheet the engine needs for
// settlement and currency decisions.
func testCapabilities(v market.Venue) venue.Capabilities {
	currency := market.CurrencyUSDC
	if v == market.VenuePredictFun {
		currency = market.CurrencyUSDT
	}
	return venue.Capabilities{
		Venue: v, Label: v.Label(), Currency: currency, QuoteDecimals: 6,
		OrderTypes: []string{"FAK"}, SupportsSettlement: v == market.VenuePolymarket,
	}
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

	quote, err := instance.QuoteOpportunity(context.Background(), "eligible-yes", 0, "")
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
	quote, err = instance.QuoteOpportunity(context.Background(), "eligible-yes", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if !quote.ExceedsMaxPrice || quote.Amount != 50 {
		t.Fatalf("a fill above max_price must be flagged: %+v", quote)
	}

	if _, err := instance.QuoteOpportunity(context.Background(), "missing-token", 0, ""); !errors.Is(err, engine.ErrInvalidOrder) {
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
	positions, err := instance.Positions(context.Background(), paper.ID, "won", "")
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

func TestScanAppliesSportsPolicyAndVolatilityGuard(t *testing.T) {
	start := time.Now().UTC().Add(time.Hour)
	sportsGame := binary("sports-game", 0.98, 20000, 2)
	sportsGame.EventID = "e-sports"
	sportsGame.GameStartTime = &start
	sportsFuture := binary("sports-future", 0.98, 20000, 2)
	sportsFuture.EventID = "e-sports-future"
	politics := binary("politics", 0.98, 20000, 2)
	politics.EventID = "e-politics"
	volatile := binary("volatile", 0.98, 20000, 2)
	volatile.EventID = "e-politics"
	change := 0.20
	volatile.PriceChange24h = &change

	provider := &fakeProvider{
		markets: map[string]market.Market{
			"sports-game": sportsGame, "sports-future": sportsFuture,
			"politics": politics, "volatile": volatile,
		},
		eventTags: map[string][]market.Tag{
			"e-sports":        {{Slug: "sports"}},
			"e-sports-future": {{Slug: "sports"}},
			"e-politics":      {{Slug: "politics"}},
		},
	}
	instance, _ := newTestEngine(t, provider, nil)
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The default policy removes game markets (overtime risk) but keeps
	// season-long sports futures. The 24h volatility guard removes the
	// volatile market before categories are resolved.
	opportunities := instance.Opportunities()
	if opportunities.Total != 2 {
		t.Fatalf("expected sports future and politics, got %+v", opportunities.Items)
	}
	byID := map[string]engine.Opportunity{}
	for _, item := range opportunities.Items {
		byID[item.MarketID] = item
	}
	if _, ok := byID["sports-game"]; ok {
		t.Fatalf("sports game should be excluded by default: %+v", byID)
	}
	if _, ok := byID["volatile"]; ok {
		t.Fatalf("volatile market should be excluded: %+v", byID)
	}
	if item := byID["sports-future"]; item.Category != "sports" || item.SportsGame || item.OvertimeRisk {
		t.Fatalf("sports future flags are wrong: %+v", item)
	}
	if item := byID["politics"]; item.Category != "politics" || item.SportsGame {
		t.Fatalf("politics flags are wrong: %+v", item)
	}

	settings := instance.Settings()
	settings.SportsPolicy = engine.SportsPolicyExcludeAll
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	opportunities = instance.Opportunities()
	if opportunities.Total != 1 || opportunities.Items[0].MarketID != "politics" {
		t.Fatalf("exclude_all should leave only politics, got %+v", opportunities.Items)
	}

	settings.SportsPolicy = engine.SportsPolicyAllow
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	opportunities = instance.Opportunities()
	if opportunities.Total != 3 {
		t.Fatalf("allow should keep all non-volatile markets, got %+v", opportunities.Items)
	}
	for _, item := range opportunities.Items {
		if item.MarketID == "sports-game" && (!item.SportsGame || !item.OvertimeRisk) {
			t.Fatalf("sports game must be flagged when allowed: %+v", item)
		}
	}
}

func TestPlaceOrderRejectsSportsGameByDefault(t *testing.T) {
	start := time.Now().UTC().Add(time.Hour)
	item := binary("sports-game", 0.98, 20000, 2)
	item.EventID = "e-sports"
	item.GameStartTime = &start
	provider := &fakeProvider{
		markets:   map[string]market.Market{"sports-game": item},
		eventTags: map[string][]market.Tag{"e-sports": {{Slug: "sports"}}},
	}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{
		MarketID: "sports-game", Outcome: "Yes", TokenID: "sports-game-yes", Amount: 100,
	}); !errors.Is(err, engine.ErrInvalidOrder) {
		t.Fatalf("manual sports game order must be rejected by default, got %v", err)
	}

	settings := instance.Settings()
	settings.SportsPolicy = engine.SportsPolicyAllow
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{
		MarketID: "sports-game", Outcome: "Yes", TokenID: "sports-game-yes", Amount: 100,
	}); err != nil {
		t.Fatalf("manual order should be allowed when policy is allow, got %v", err)
	}
}

func TestScanUsesMarketTagsDirectly(t *testing.T) {
	item := binary("tagged", 0.98, 20000, 2)
	item.EventID = "e-tagged"
	item.Tags = []market.Tag{{ID: "21", Label: "Crypto", Slug: "crypto"}}
	provider := &fakeProvider{markets: map[string]market.Market{"tagged": item}}
	instance, _ := newTestEngine(t, provider, nil)
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	opportunities := instance.Opportunities()
	if opportunities.Total != 1 || opportunities.Items[0].Category != "crypto" {
		t.Fatalf("market tags should classify without an event request: %+v", opportunities.Items)
	}
}

func TestSettingsValidateCategoryAndVolatility(t *testing.T) {
	settings := engine.DefaultSettings()
	if settings.DefaultCategory != engine.RadarCategoryNonSports || settings.SportsPolicy != engine.SportsPolicyExcludeGames {
		t.Fatalf("unexpected radar defaults: %+v", settings)
	}
	if settings.StopLossEnabled || settings.StopLossRatio != 0.70 {
		t.Fatalf("stop-loss must default to disabled with a 70%% trigger: %+v", settings)
	}
	if err := settings.Validate(); err != nil {
		t.Fatalf("default settings must be valid: %v", err)
	}
	settings.DefaultCategory = "not-a-category"
	if err := settings.Validate(); err == nil {
		t.Fatal("invalid default category must be rejected")
	}
	settings = engine.DefaultSettings()
	settings.SportsPolicy = "not-a-policy"
	if err := settings.Validate(); err == nil {
		t.Fatal("invalid sports policy must be rejected")
	}
	settings = engine.DefaultSettings()
	settings.MaxPriceChange24h = 1.5
	if err := settings.Validate(); err == nil {
		t.Fatal("out-of-range volatility limit must be rejected")
	}
	settings = engine.DefaultSettings()
	settings.StopLossRatio = 1.2
	if err := settings.Validate(); err == nil {
		t.Fatal("an out-of-range stop-loss ratio must be rejected")
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

func TestPaperStopLossClosesPositionAtRatio(t *testing.T) {
	ctx := context.Background()
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	// Stop-loss must work without enabling automatic entries: this position is
	// opened manually and the account keeps auto-trade disabled.
	position, err := instance.PlaceOrder(ctx, paper.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A price fall below 80% of entry with the stop disabled must leave the
	// position untouched.
	drop := provider.markets["one"]
	drop.Outcomes[0].Price = 0.75
	drop.Outcomes[1].Price = 0.25
	provider.markets["one"] = drop
	if err := instance.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	open, err := instance.Positions(ctx, paper.ID, "open", "")
	if err != nil || open.Total != 1 {
		t.Fatalf("stop disabled must keep the position open: %+v err=%v", open, err)
	}

	settings := instance.Settings()
	settings.StopLossEnabled = true
	settings.StopLossRatio = 0.80
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if err := instance.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	sold, err := instance.Positions(ctx, paper.ID, "sold", "")
	if err != nil {
		t.Fatal(err)
	}
	if sold.Total != 1 {
		t.Fatalf("stop-loss did not close the position: %+v", sold.Items)
	}
	closed := sold.Items[0]
	if closed.ID != position.ID || closed.Status != "lost" || closed.ExitKind != "stop" || closed.RealizedPNL >= 0 {
		t.Fatalf("unexpected stopped position: %+v", closed)
	}
	trigger := position.EntryPrice * settings.StopLossRatio
	if closed.CurrentPrice > trigger || closed.CurrentPrice <= 0 {
		t.Fatalf("exit price %.4f is not below trigger %.4f", closed.CurrentPrice, trigger)
	}
	wantGross := closed.CurrentPrice * closed.Shares
	wantFee := wantGross * settings.FeeBPS / 10000
	if math.Abs(closed.RealizedPNL-(wantGross-wantFee-position.Cost)) > 1e-6 {
		t.Fatalf("realized pnl does not net the sell fee: %+v", closed)
	}
	if math.Abs(closed.Fee-(position.Fee+wantFee)) > 1e-9 {
		t.Fatalf("position fee must include the sell fee: got %v want %v", closed.Fee, position.Fee+wantFee)
	}

	orders, err := instance.Orders(ctx, paper.ID, "all", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var sellOrder *engine.Order
	for i := range orders.Items {
		if orders.Items[i].Action == "stop" {
			sellOrder = &orders.Items[i]
		}
	}
	if sellOrder == nil || sellOrder.Type != "settle" || sellOrder.Status != "filled" || sellOrder.Shares != closed.Shares {
		t.Fatalf("stop-loss order was not recorded: %+v", orders.Items)
	}
	dashboard, err := instance.Dashboard(ctx, paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard.Metrics.OpenPositions != 0 || dashboard.Metrics.StoppedPositions != 1 ||
		dashboard.Metrics.WinRate != 0 || dashboard.Metrics.RealizedPNL >= 0 {
		t.Fatalf("unexpected metrics after stop-loss: %+v", dashboard.Metrics)
	}
}

func TestLiveStopLossSellsThroughExecutor(t *testing.T) {
	ctx := context.Background()
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	live := &fakeLive{
		balance: 10000, allowance: 10000,
		fill:     engine.LiveFill{Shares: 100, Cost: 98.2, Price: 0.982, Fee: 0.1, Status: "matched"},
		sellFill: engine.LiveFill{Shares: 100, Cost: 74.9, Price: 0.75, Fee: 0.1, Status: "matched"},
	}
	instance, _ := newTestEngine(t, provider, live)
	account := accountByMode(t, instance, engine.AccountModeLive)
	position, err := instance.PlaceOrder(ctx, account.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	drop := provider.markets["one"]
	drop.Outcomes[0].Price = 0.75
	drop.Outcomes[1].Price = 0.25
	provider.markets["one"] = drop

	settings := instance.Settings()
	settings.StopLossEnabled = true
	settings.StopLossRatio = 0.80
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if err := instance.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	sold, err := instance.Positions(ctx, account.ID, "sold", "")
	if err != nil || sold.Total != 1 {
		t.Fatalf("live stop-loss did not close the position: %+v err=%v", sold, err)
	}
	if sold.Items[0].ID != position.ID || sold.Items[0].RealizedPNL >= 0 {
		t.Fatalf("unexpected live stopped position: %+v", sold.Items[0])
	}
	orders, err := instance.Orders(ctx, account.ID, "all", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var liveSell *engine.Order
	for i := range orders.Items {
		if orders.Items[i].Action == "stop" {
			liveSell = &orders.Items[i]
		}
	}
	if liveSell == nil || liveSell.Status != "filled" || liveSell.ExternalID != "fake-sell" {
		t.Fatalf("live stop-loss order was not finalized: %+v", orders.Items)
	}
	// The executor reports the gross notional (98.2 buy, 74.9 sell) and the
	// taker fee separately; the engine must fold it in exactly once, otherwise
	// the two booking paths (direct fill vs reconciliation) disagree by a fee.
	if math.Abs(position.Cost-98.3) > 1e-9 {
		t.Fatalf("live entry basis must include the fee once: %+v", position)
	}
	if math.Abs(liveSell.Amount-74.8) > 1e-9 {
		t.Fatalf("live exit proceeds must net the fee once: %+v", liveSell)
	}
}

func TestUnknownLiveStopLossIsReconciled(t *testing.T) {
	ctx := context.Background()
	provider := &fakeProvider{markets: map[string]market.Market{"one": binary("one", 0.98, 20000, 2)}}
	live := &fakeLive{
		balance: 10000, allowance: 10000,
		fill:     engine.LiveFill{Shares: 100, Cost: 98.2, Price: 0.982, Fee: 0.1, Status: "matched"},
		sellFill: engine.LiveFill{OrderID: "pending-sell", Status: "unknown"},
		sellErr:  errors.New("timeout"),
	}
	instance, _ := newTestEngine(t, provider, live)
	account := accountByMode(t, instance, engine.AccountModeLive)
	if _, err := instance.UpdateAccount(ctx, account.ID, nil, boolPointer(true), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PlaceOrder(ctx, account.ID, engine.OrderRequest{
		MarketID: "one", Outcome: "Yes", TokenID: "one-yes", Amount: 100,
	}); err != nil {
		t.Fatal(err)
	}
	drop := provider.markets["one"]
	drop.Outcomes[0].Price = 0.75
	drop.Outcomes[1].Price = 0.25
	provider.markets["one"] = drop
	settings := instance.Settings()
	settings.StopLossEnabled = true
	settings.StopLossRatio = 0.80
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if err := instance.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	unresolved, err := instance.Orders(ctx, account.ID, "unresolved", 10, 0, "")
	if err != nil || unresolved.Total != 1 || unresolved.Items[0].Action != "stop" {
		t.Fatalf("unknown sell was not persisted: %+v err=%v", unresolved, err)
	}

	live.sellErr = nil
	live.lookup = map[string]engine.LiveFill{
		"pending-sell": {OrderID: "pending-sell", Shares: 100, Cost: 74.9, Price: 0.75, Fee: 0.1, Status: "matched"},
	}
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	sold, err := instance.Positions(ctx, account.ID, "sold", "")
	if err != nil || sold.Total != 1 {
		t.Fatalf("reconciliation did not close the position: %+v err=%v", sold, err)
	}
	orders, err := instance.Orders(ctx, account.ID, "all", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var sell *engine.Order
	for i := range orders.Items {
		if orders.Items[i].Action == "stop" {
			sell = &orders.Items[i]
		}
	}
	if sell == nil || sell.Status != "filled" || math.Abs(sell.Amount-(74.9-0.1)) > 1e-9 {
		t.Fatalf("sell reconciliation did not net the fee: %+v", orders.Items)
	}
}

// Predict.fun books are routinely thinner than Polymarket's, so the liquidity
// floor is per venue: one global number would either hide every Predict.fun row
// or let Polymarket noise through.
func TestVenueMinLiquidityOverridesGlobalFloor(t *testing.T) {
	ctx := context.Background()
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{
		"pm": binary("pm", 0.97, 5000, 3),
	}}, nil)
	addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
		venue: market.VenuePredictFun, markets: map[string]market.Market{"pf": binary("pf", 0.96, 5000, 2)},
	}, nil)
	instance, _ := newTestEngineWithRegistry(t, registry)
	settings := instance.Settings()
	settings.MinPrice = 0.10
	settings.MaxPrice = 0.99
	// The global floor hides both fixtures; the Predict.fun override admits its own.
	settings.MinLiquidity = 10000
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{MinLiquidity: 100})

	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	list := instance.Opportunities()
	if list.Total != 1 || list.Items[0].Venue != market.VenuePredictFun {
		t.Fatalf("only the venue with a lower floor may pass: %+v", list.Items)
	}

	// The override must be per venue, not a global rewrite.
	full := instance.Settings()
	if full.MinLiquidity != 10000 {
		t.Fatalf("global floor must stay 10000, got %v", full.MinLiquidity)
	}
	if got := full.For(market.VenuePolymarket).MinLiquidity; got != 10000 {
		t.Fatalf("polymarket must inherit the global floor, got %v", got)
	}
	if got := full.For(market.VenuePredictFun).MinLiquidity; got != 100 {
		t.Fatalf("predictfun must use its own floor, got %v", got)
	}
}

func TestScopedScanKeepsOtherVenueRows(t *testing.T) {
	ctx := context.Background()
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{
		"pm": binary("pm", 0.97, 20000, 3),
	}}, nil)
	addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
		venue: market.VenuePredictFun, markets: map[string]market.Market{"pf": binary("pf", 0.96, 20000, 2)},
	}, nil)
	instance, _ := newTestEngineWithRegistry(t, registry)
	// Widen the default 0.95–0.99 window so both fixtures are candidates.
	settings := instance.Settings()
	settings.MinPrice = 0.10
	settings.MaxPrice = 0.99
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{})

	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	full := instance.Opportunities()
	if full.Total != 2 {
		t.Fatalf("a full scan must hold both venues: %+v", full.Items)
	}

	// A venue-scoped scan (the radar chip, a per-venue scan button) must not
	// empty the other venue's rows: that is exactly the table the operator is
	// looking at when they click the chip.
	if _, err := instance.ManualScan(ctx, 0, false, market.VenuePredictFun); err != nil {
		t.Fatal(err)
	}
	scoped := instance.Opportunities()
	if scoped.Total != 2 {
		t.Fatalf("a scoped scan dropped rows from other venues: %+v", scoped.Items)
	}
	seen := map[market.Venue]bool{}
	for _, item := range scoped.Items {
		seen[item.EffectiveVenue()] = true
	}
	if !seen[market.VenuePolymarket] || !seen[market.VenuePredictFun] {
		t.Fatalf("scoped radar must still report both venues: %+v", scoped.Items)
	}
	if len(scoped.Venues) != 2 {
		t.Fatalf("radar must report every venue it holds, got %v", scoped.Venues)
	}

	// A full scan replaces the radar outright.
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	if got := instance.Opportunities(); got.Total != 2 {
		t.Fatalf("a full scan must replace the radar: %+v", got.Items)
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
	orders, err := instance.Orders(context.Background(), paper.ID, "all", 10, 0, "")
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
	orders, err := instance.Orders(context.Background(), paper.ID, "all", 10, 0, "")
	if err != nil || orders.Total != 1 || orders.Items[0].Status != "rejected" {
		t.Fatalf("rejection was not logged: %+v, %v", orders, err)
	}
}

func TestAutoPaperTradeHonorsExposureAndDoesNotDuplicate(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"one": nearClose(binary("one", 0.98, 20000, 2), 10*time.Minute),
		"two": nearClose(binary("two", 0.97, 20000, 3), 11*time.Minute),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil, nil); err != nil {
		t.Fatal(err)
	}
	settings := instance.Settings()
	// The caps are ordered (single market must not exceed the total), so both
	// have to move for the total cap to be the binding one.
	settings.MaxPosition = 140
	settings.MaxExposure = 150
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := instance.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	positions, err := instance.Positions(context.Background(), paper.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("expected exactly one capped position, got %+v", positions.Items)
	}
}

func TestAutoTradeWithInsufficientCashSkipsWithoutSpam(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"one": nearClose(binary("one", 0.98, 20000, 2), 10*time.Minute),
		"two": nearClose(binary("two", 0.97, 20000, 3), 11*time.Minute),
	}}
	instance, repository := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil, nil); err != nil {
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
	positions, _ := instance.Positions(context.Background(), paper.ID, "open", "")
	if positions.Total != 0 {
		t.Fatalf("expected no positions without cash, got %+v", positions.Items)
	}
	orders, _ := instance.Orders(context.Background(), paper.ID, "all", 10, 0, "")
	if orders.Total != 0 {
		t.Fatalf("automatic cash rejection should be skipped before logging: %+v", orders.Items)
	}
}

func TestAutoTradeWaitsForEntryWindow(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"soon": nearClose(binary("soon", 0.98, 20000, 2), 30*time.Minute),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err := instance.Positions(context.Background(), paper.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 0 {
		t.Fatalf("auto trade must wait until the 20-minute window: %+v", positions.Items)
	}

	item := provider.markets["soon"]
	item.EndDate = time.Now().UTC().Add(10 * time.Minute)
	provider.markets["soon"] = item
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err = instance.Positions(context.Background(), paper.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("auto trade should enter inside the window: %+v", positions.Items)
	}
}

func TestAutoTradeUsesShorterSportsEntryWindow(t *testing.T) {
	item := nearClose(binary("sports-future", 0.98, 20000, 2), 10*time.Minute)
	item.Tags = []market.Tag{{ID: "sports", Label: "Sports", Slug: "sports"}}
	provider := &fakeProvider{markets: map[string]market.Market{"sports-future": item}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err := instance.Positions(context.Background(), paper.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 0 {
		t.Fatalf("sports markets must wait until the 5-minute window: %+v", positions.Items)
	}

	item.EndDate = time.Now().UTC().Add(4 * time.Minute)
	provider.markets["sports-future"] = item
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err = instance.Positions(context.Background(), paper.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("sports markets should enter inside their 5-minute window: %+v", positions.Items)
	}
}

func TestAutoTradeMarksMarketAsOrdered(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		// Both outcomes are inside the configured price range, so only the
		// per-market mark prevents a second automatic entry.
		"two-sided": nearClose(binary("two-sided", 0.60, 20000, 2), 10*time.Minute),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil, nil); err != nil {
		t.Fatal(err)
	}
	settings := instance.Settings()
	settings.MinPrice = 0.40
	settings.MaxPrice = 0.99
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := instance.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	positions, err := instance.Positions(context.Background(), paper.ID, "all", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("a market must only be auto-ordered once: %+v", positions.Items)
	}
	orders, err := instance.Orders(context.Background(), paper.ID, "filled", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if orders.Total != 1 {
		t.Fatalf("expected exactly one filled automatic order, got %+v", orders.Items)
	}
}

func TestAutoTradeDoesNotReenterSettledMarket(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"one": nearClose(binary("one", 0.98, 20000, 2), 10*time.Minute),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	item := provider.markets["one"]
	item.Closed = true
	item.Active = false
	item.ResolutionStatus = "resolved"
	item.Outcomes[0].Price = 1
	item.Outcomes[1].Price = 0
	provider.markets["one"] = item
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Simulate the upstream listing the same market as an open opportunity
	// again. The settled position must still block a second automatic entry.
	item.Closed = false
	item.Active = true
	item.ResolutionStatus = ""
	item.Outcomes[0].Price = 0.98
	item.Outcomes[1].Price = 0.02
	item.EndDate = time.Now().UTC().Add(10 * time.Minute)
	provider.markets["one"] = item
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	open, err := instance.Positions(context.Background(), paper.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if open.Total != 0 {
		t.Fatalf("a settled market must not be re-entered: %+v", open.Items)
	}
	won, err := instance.Positions(context.Background(), paper.ID, "won", "")
	if err != nil {
		t.Fatal(err)
	}
	if won.Total != 1 {
		t.Fatalf("expected the original settled position to remain: %+v", won.Items)
	}
	filled, err := instance.Orders(context.Background(), paper.ID, "filled", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if filled.Total != 1 {
		t.Fatalf("expected exactly one filled order, got %+v", filled.Items)
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
	positions, _ := instance.Positions(context.Background(), paper.ID, "open", "")
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
	if !errors.Is(err, engine.ErrVenueReadOnly) && !errors.Is(err, engine.ErrVenueNotConfigured) {
		t.Fatalf("expected the venue to refuse live orders, got %v", err)
	}
	orders, err := instance.Orders(context.Background(), liveAccount.ID, "all", 10, 0, "")
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
	orders, err := instance.Orders(context.Background(), liveAccount.ID, "all", 10, 0, "")
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

// TestLiveOrderRefusesProtocolV2Market guards the Protocol V2 boundary: those
// markets settle through ExchangeV3 with signing domain version "3" and pUSD
// approvals, so signing one with the CTFExchangeV2 domain would be rejected by
// the exchange after the order had already been submitted.
func TestLiveOrderRefusesProtocolV2Market(t *testing.T) {
	item := binary("v2", 0.98, 20000, 2)
	item.Version = "v2"
	provider := &fakeProvider{markets: map[string]market.Market{"v2": item}}
	executor := &fakeLive{
		balance: 1000,
		fill:    engine.LiveFill{OrderID: "must-not-submit", Shares: 1, Cost: 1, Price: 0.98},
	}
	instance, _ := newTestEngine(t, provider, executor)
	liveAccount := accountByMode(t, instance, engine.AccountModeLive)
	_, err := instance.PlaceOrder(context.Background(), liveAccount.ID, engine.OrderRequest{
		MarketID: "v2", Outcome: "Yes", TokenID: "v2-yes", Amount: 100,
	})
	if err == nil || !strings.Contains(err.Error(), "Protocol V2") {
		t.Fatalf("a Protocol V2 market must be refused before signing: %v", err)
	}
	orders, orderErr := instance.Orders(context.Background(), liveAccount.ID, "all", 10, 0, "")
	if orderErr != nil || orders.Total != 1 || orders.Items[0].Status != "rejected" {
		t.Fatalf("the refusal must be recorded as a rejected order: %+v, %v", orders, orderErr)
	}
}

// TestOrderRefusesPausedOrderBook covers acceptingOrders: a market can stay
// active and unclosed while its book is paused, and only the acceptingOrders
// flag says so.
func TestOrderRefusesPausedOrderBook(t *testing.T) {
	item := binary("paused", 0.98, 20000, 2)
	paused := false
	item.AcceptingOrders = &paused
	provider := &fakeProvider{markets: map[string]market.Market{"paused": item}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	_, err := instance.PlaceOrder(context.Background(), paper.ID, engine.OrderRequest{
		MarketID: "paused", Outcome: "Yes", TokenID: "paused-yes", Amount: 100,
	})
	if err == nil || !strings.Contains(err.Error(), "暂停") {
		t.Fatalf("a paused order book must be refused: %v", err)
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
	positions, err := instance.Positions(context.Background(), paper.ID, "all", "")
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
	orders, err := instance.Orders(context.Background(), paper.ID, "settled", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if orders.Total != 1 || orders.Items[0].Kind != "manual" {
		t.Fatalf("settlement must retain the original order kind: %+v", orders.Items)
	}
}

func TestScanOnlyDoesNotAutoTrade(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{"one": nearClose(binary("one", 0.98, 20000, 2), 10*time.Minute)}}
	instance, _ := newTestEngine(t, provider, nil)
	paper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.UpdateAccount(context.Background(), paper.ID, nil, boolPointer(true), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err := instance.Positions(context.Background(), paper.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 0 {
		t.Fatalf("scan-only must not place orders: %+v", positions.Items)
	}
	if err := instance.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err = instance.Positions(context.Background(), paper.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 {
		t.Fatalf("full scan should auto-trade: %+v", positions.Items)
	}
}

func TestManualScanAutoTradesSelectedAccountAndSkipsHeld(t *testing.T) {
	provider := &fakeProvider{markets: map[string]market.Market{
		"held":  nearClose(binary("held", 0.98, 20000, 2), 10*time.Minute),
		"fresh": nearClose(binary("fresh", 0.98, 20000, 2), 10*time.Minute),
	}}
	instance, _ := newTestEngine(t, provider, nil)
	focused := accountByMode(t, instance, engine.AccountModePaper)
	other, err := instance.CreateAccount(context.Background(), "other paper", engine.AccountModePaper, market.VenuePolymarket)
	if err != nil {
		t.Fatal(err)
	}
	// The focused account already holds one of the two markets. Its AutoTrade
	// switch stays off: an explicit manual scan is the user's consent for this
	// one pass and must not leak into other accounts.
	if _, err := instance.PlaceOrder(context.Background(), focused.ID, engine.OrderRequest{
		MarketID: "held", Outcome: "Yes", TokenID: "held-yes", Amount: 50,
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := instance.ManualScan(context.Background(), focused.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if summary.AccountID != focused.ID || !summary.Execute || summary.OrdersPlaced != 1 || summary.SkippedHeld != 1 {
		t.Fatalf("unexpected manual scan summary: %+v", summary)
	}
	positions, err := instance.Positions(context.Background(), focused.ID, "all", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 2 {
		t.Fatalf("manual scan should add exactly one position: %+v", positions.Items)
	}
	otherPositions, err := instance.Positions(context.Background(), other.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if otherPositions.Total != 0 {
		t.Fatalf("manual scan leaked into another account: %+v", otherPositions.Items)
	}
	// Re-running must skip both markets instead of stacking another entry.
	summary, err = instance.ManualScan(context.Background(), focused.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if summary.OrdersPlaced != 0 || summary.SkippedHeld != 2 {
		t.Fatalf("repeat scan should skip held markets: %+v", summary)
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
	orders, err := instance.Orders(context.Background(), paper.ID, "all", 2, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if orders.Total != 3 || len(orders.Items) != 2 {
		t.Fatalf("expected total 3 and two rows, got total=%d rows=%d", orders.Total, len(orders.Items))
	}
	filled, err := instance.Orders(context.Background(), paper.ID, "filled", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if filled.Total != 3 {
		t.Fatalf("expected 3 filled orders, got %d", filled.Total)
	}
	rejected, err := instance.Orders(context.Background(), paper.ID, "rejected", 10, 0, "")
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
	orders, err := instance.Orders(context.Background(), liveAccount.ID, "all", 10, 0, "")
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
	orders, err := instance.Orders(context.Background(), liveAccount.ID, "unresolved", 10, 0, "")
	if err != nil || orders.Total != 1 || orders.Items[0].Status != "unknown" {
		t.Fatalf("unknown order was not persisted: %+v, %v", orders, err)
	}
	executor.err = nil
	executor.fill = engine.LiveFill{OrderID: "pending-1", Shares: 100, Cost: 98, Price: 0.98, Status: "matched"}
	executor.lookup = map[string]engine.LiveFill{"pending-1": executor.fill}
	if err := instance.ScanOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	positions, err := instance.Positions(context.Background(), liveAccount.ID, "open", "")
	if err != nil {
		t.Fatal(err)
	}
	if positions.Total != 1 || math.Abs(positions.Items[0].Shares-100) > 1e-9 {
		t.Fatalf("reconciliation did not create the position: %+v", positions.Items)
	}
	orders, err = instance.Orders(context.Background(), liveAccount.ID, "all", 10, 0, "")
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
func (p *runtimeProvider) Venue() market.Venue    { return market.VenuePolymarket }

type runtimeLive struct {
	*fakeLive
	clobURL     string
	defaultCLOB string
	settings    engine.LiveSettings
	endpoints   engine.WalletEndpoints
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

func (l *runtimeLive) DiscoverWallet(_ context.Context, settings engine.LiveSettings, endpoints engine.WalletEndpoints) (engine.WalletDiscovery, error) {
	l.settings = settings
	l.probeURL = endpoints.CLOB
	l.endpoints = endpoints
	return engine.WalletDiscovery{
		Signer:     "0x0000000000000000000000000000000000000001",
		Funder:     "0x0000000000000000000000000000000000000003",
		WalletType: 3, WalletName: "Deposit Wallet（POLY_1271）", Deployed: true,
		Source:  "polymarket-profile",
		Wallets: []string{"0x0000000000000000000000000000000000000003"},
	}, nil
}

func TestDiscoverWalletUsesStoredSigner(t *testing.T) {
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
	discovery, err := instance.DiscoverWallet(ctx, market.VenuePolymarket, instance.SystemSettings().Live.Redacted(), engine.WalletEndpoints{
		CLOB: "https://clob.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if discovery.WalletType != 3 || discovery.Funder == "" || live.settings.PrivateKey != key {
		t.Fatalf("unexpected discovery: %+v %+v", discovery, live.settings)
	}
	// An empty Gamma endpoint falls back to the effective data-source URL, so
	// the profile lookup always has somewhere to go.
	if live.endpoints.Gamma != instance.SystemRuntime().GammaBaseURL {
		t.Fatalf("gamma endpoint = %q, want %q", live.endpoints.Gamma, instance.SystemRuntime().GammaBaseURL)
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
	probe, err := instance.ProbeLive(ctx, market.VenuePolymarket, masked, "https://clob.example.com")
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
	_, err := instance.ProbeLive(context.Background(), market.VenuePolymarket, engine.LiveSettings{APIKey: "only-key"}, "")
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
	probe, err := instance.ProbeRelayer(ctx, market.VenuePolymarket, instance.SystemSettings().Live.Redacted(), "https://clob.example.com", "relayer-key", "0x0000000000000000000000000000000000000001")
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

// addVenue registers one fake venue with the engine's test capabilities.
func addVenue(t *testing.T, registry *venue.Registry, v market.Venue, data venue.MarketData, live engine.LiveExecutor) {
	t.Helper()
	addVenueCaps(t, registry, testCapabilities(v), data, live)
}

// addVenueCaps registers a venue with an explicit fact sheet, for the few tests
// that care about a capability rather than about the venue itself.
func addVenueCaps(t *testing.T, registry *venue.Registry, caps venue.Capabilities, data venue.MarketData, live engine.LiveExecutor) {
	t.Helper()
	if err := registry.Register(caps, data, live); err != nil {
		t.Fatal(err)
	}
}

// enableVenue turns a venue on and applies the given overrides. The map is
// rebuilt so a test never mutates the engine's stored settings in place.
func enableVenue(t *testing.T, instance *engine.Engine, v market.Venue, override engine.VenueSettings) {
	t.Helper()
	settings := instance.Settings()
	venues := make(map[market.Venue]engine.VenueSettings, len(settings.Venues)+1)
	for key, entry := range settings.Venues {
		venues[key] = entry
	}
	override.Enabled = true
	venues[v] = override
	settings.Venues = venues
	if err := instance.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
}

func TestPaperFeeIsVenueAware(t *testing.T) {
	ctx := context.Background()
	t.Run("polymarket keeps the historical notional rate", func(t *testing.T) {
		provider := &fakeProvider{markets: map[string]market.Market{"pm": binary("pm", 0.98, 20000, 2)}}
		instance, _ := newTestEngine(t, provider, nil)
		paper := accountByMode(t, instance, engine.AccountModePaper)
		settings := instance.Settings()
		position, err := instance.PlaceOrder(ctx, paper.ID, engine.OrderRequest{
			MarketID: "pm", Outcome: "Yes", TokenID: "pm-yes", Amount: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		wantFee := 100 * settings.FeeBPS / 10000
		if math.Abs(position.Fee-wantFee) > 1e-9 {
			t.Fatalf("polymarket paper fee changed: got %v want %v", position.Fee, wantFee)
		}
		if math.Abs(position.Cost-(100+wantFee)) > 1e-9 {
			t.Fatalf("polymarket paper cost changed: %+v", position)
		}
	})

	t.Run("predictfun charges rate times min(p, 1-p)", func(t *testing.T) {
		pfMarket := binary("pf", 0.40, 20000, 2)
		pfMarket.FeeRateBPS = 200
		registry := venue.NewRegistry()
		addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{}}, nil)
		addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
			venue: market.VenuePredictFun, markets: map[string]market.Market{"pf": pfMarket},
		}, nil)
		instance, _ := newTestEngineWithRegistry(t, registry)
		settings := instance.Settings()
		settings.MinPrice = 0.10
		settings.MaxPrice = 0.99
		if err := instance.UpdateSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
		enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{})
		pfPaper, err := instance.CreateAccount(ctx, "PF paper", engine.AccountModePaper, market.VenuePredictFun)
		if err != nil {
			t.Fatal(err)
		}
		position, err := instance.PlaceOrder(ctx, pfPaper.ID, engine.OrderRequest{
			MarketID: "pf", Outcome: "Yes", TokenID: "pf-yes", Amount: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		wantFee := 0.02 * math.Min(position.EntryPrice, 1-position.EntryPrice) * position.Shares
		if math.Abs(position.Fee-wantFee) > 1e-9 {
			t.Fatalf("predictfun paper fee = %v, want rate*min(p,1-p)*shares = %v", position.Fee, wantFee)
		}
		if position.Venue != market.VenuePredictFun || position.Currency != market.CurrencyUSDT {
			t.Fatalf("predictfun position must carry its own venue/currency: %+v", position)
		}
		orders, err := instance.Orders(ctx, pfPaper.ID, "all", 10, 0, "")
		if err != nil || orders.Total != 1 {
			t.Fatalf("unexpected orders: %+v err=%v", orders, err)
		}
		if orders.Items[0].Venue != market.VenuePredictFun || orders.Items[0].Currency != market.CurrencyUSDT {
			t.Fatalf("predictfun order must carry its own venue/currency: %+v", orders.Items[0])
		}
	})
}

func TestExposureCapsAreVenueScoped(t *testing.T) {
	ctx := context.Background()
	pfMarket := binary("pf-a", 0.40, 20000, 2)
	pfMarket.FeeRateBPS = 200
	pfMarket2 := binary("pf-b", 0.40, 20000, 2)
	pfMarket2.FeeRateBPS = 200
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{
		"pm": binary("pm", 0.98, 20000, 2),
	}}, nil)
	addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
		venue:   market.VenuePredictFun,
		markets: map[string]market.Market{"pf-a": pfMarket, "pf-b": pfMarket2},
	}, nil)
	instance, _ := newTestEngineWithRegistry(t, registry)
	settings := instance.Settings()
	settings.MinPrice = 0.10
	settings.MaxPrice = 0.99
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	// Predict.fun is capped at 120 total while Polymarket keeps the generous
	// global 1000. A Predict.fun order must not consume the Polymarket bucket.
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{
		OrderSize: 100, MaxPosition: 110, MaxExposure: 120,
	})
	pfPaper, err := instance.CreateAccount(ctx, "PF paper", engine.AccountModePaper, market.VenuePredictFun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PlaceOrder(ctx, pfPaper.ID, engine.OrderRequest{
		MarketID: "pf-a", Outcome: "Yes", TokenID: "pf-a-yes", Amount: 100,
	}); err != nil {
		t.Fatalf("first predictfun order must fit the per-venue cap: %v", err)
	}
	if _, err := instance.PlaceOrder(ctx, pfPaper.ID, engine.OrderRequest{
		MarketID: "pf-b", Outcome: "Yes", TokenID: "pf-b-yes", Amount: 100,
	}); !errors.Is(err, engine.ErrInvalidOrder) {
		t.Fatalf("second predictfun order should breach the venue cap, got %v", err)
	}
	pmPaper := accountByMode(t, instance, engine.AccountModePaper)
	if _, err := instance.PlaceOrder(ctx, pmPaper.ID, engine.OrderRequest{
		MarketID: "pm", Outcome: "Yes", TokenID: "pm-yes", Amount: 100,
	}); err != nil {
		t.Fatalf("polymarket must keep its own exposure bucket: %v", err)
	}
}

func TestScanErrorIsolationKeepsOtherVenueOpportunities(t *testing.T) {
	ctx := context.Background()
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{
		"pm": binary("pm", 0.98, 20000, 2),
	}}, nil)
	addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
		venue: market.VenuePredictFun, err: errors.New("predictfun is down"),
	}, nil)
	instance, _ := newTestEngineWithRegistry(t, registry)
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{})
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatalf("one degraded venue must not fail the whole scan: %v", err)
	}
	opportunities := instance.Opportunities()
	if opportunities.Total != 1 || opportunities.Items[0].Venue != market.VenuePolymarket {
		t.Fatalf("polymarket opportunities must survive: %+v", opportunities.Items)
	}
	if len(opportunities.Venues) != 1 || opportunities.Venues[0] != market.VenuePolymarket {
		t.Fatalf("only the successful venue should be reported as contributing: %v", opportunities.Venues)
	}
	status := instance.Status()
	if !strings.Contains(status.LastError, "Predict.fun") {
		t.Fatalf("last error must name the failing venue, got %q", status.LastError)
	}
}

func TestVenueAndTokenIdentityAreNamespaced(t *testing.T) {
	ctx := context.Background()
	// Both venues use the same decimal ids; the engine must keep them apart.
	pmMarket := binary("shared", 0.98, 20000, 2)
	pfMarket := binary("shared", 0.40, 20000, 2)
	pfMarket.FeeRateBPS = 200
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{"shared": pmMarket}}, nil)
	addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
		venue: market.VenuePredictFun, markets: map[string]market.Market{"shared": pfMarket},
	}, nil)
	instance, _ := newTestEngineWithRegistry(t, registry)
	settings := instance.Settings()
	settings.MinPrice = 0.10
	settings.MaxPrice = 0.99
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{})
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	opportunities := instance.Opportunities()
	byVenue := map[market.Venue]engine.Opportunity{}
	for _, item := range opportunities.Items {
		if item.TokenID != "shared-yes" {
			continue
		}
		byVenue[item.Venue] = item
	}
	pm, pmOK := byVenue[market.VenuePolymarket]
	pf, pfOK := byVenue[market.VenuePredictFun]
	if !pmOK || !pfOK || pm.Price == pf.Price {
		t.Fatalf("same token id from two venues was not kept apart: %+v", opportunities.Items)
	}
	if pm.TokenID != pf.TokenID || pm.Venue == pf.Venue {
		t.Fatalf("unexpected venue identity: pm=%+v pf=%+v", pm, pf)
	}
	if len(opportunities.Venues) != 2 {
		t.Fatalf("both venues contributed: %v", opportunities.Venues)
	}
	if caps := instance.VenueCapabilities(); len(caps) != 2 || caps[0].Venue != market.VenuePolymarket ||
		caps[1].Venue != market.VenuePredictFun || caps[1].Currency != market.CurrencyUSDT {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
	if venues := instance.Venues(); len(venues) != 2 || venues[0] != market.VenuePolymarket || venues[1] != market.VenuePredictFun {
		t.Fatalf("unexpected venue order: %v", venues)
	}
}

func TestLiveAccountIsUniquePerVenue(t *testing.T) {
	ctx := context.Background()
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{}}, nil)
	addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
		venue: market.VenuePredictFun, markets: map[string]market.Market{},
	}, nil)
	instance, _ := newTestEngineWithRegistry(t, registry)
	if _, err := instance.CreateAccount(ctx, "another PM live", engine.AccountModeLive, market.VenuePolymarket); !errors.Is(err, engine.ErrAccountConflict) {
		t.Fatalf("a second Polymarket live account must conflict, got %v", err)
	}
	pfLive, err := instance.CreateAccount(ctx, "PF live", engine.AccountModeLive, market.VenuePredictFun)
	if err != nil {
		t.Fatalf("a Predict.fun live account may coexist: %v", err)
	}
	if pfLive.Venue != market.VenuePredictFun || pfLive.Currency != market.CurrencyUSDT {
		t.Fatalf("unexpected predictfun account: %+v", pfLive)
	}
	if _, err := instance.CreateAccount(ctx, "second PF live", engine.AccountModeLive, market.VenuePredictFun); !errors.Is(err, engine.ErrAccountConflict) {
		t.Fatalf("a second Predict.fun live account must conflict, got %v", err)
	}
	pmVenue := market.VenuePolymarket
	if _, err := instance.UpdateAccount(ctx, pfLive.ID, nil, nil, nil, &pmVenue); !errors.Is(err, engine.ErrInvalidOrder) {
		t.Fatalf("changing an account's venue must be rejected, got %v", err)
	}
}

func TestSettlementIsSkippedForVenuesWithoutSupport(t *testing.T) {
	ctx := context.Background()
	pfMarket := binary("pf", 0.40, 20000, 2)
	pfMarket.FeeRateBPS = 200
	pf := &fakeProvider{venue: market.VenuePredictFun, markets: map[string]market.Market{"pf": pfMarket}}
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{}}, nil)
	// Predict.fun does settle (its provider maps the published winner), so the
	// mechanism under test is "no settler capability" and the fact sheet has to
	// say so explicitly.
	noSettler := testCapabilities(market.VenuePredictFun)
	noSettler.SupportsSettlement = false
	addVenueCaps(t, registry, noSettler, pf, nil)
	instance, _ := newTestEngineWithRegistry(t, registry)
	settings := instance.Settings()
	settings.MinPrice = 0.10
	settings.MaxPrice = 0.99
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{})
	paper, err := instance.CreateAccount(ctx, "PF paper", engine.AccountModePaper, market.VenuePredictFun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PlaceOrder(ctx, paper.ID, engine.OrderRequest{
		MarketID: "pf", Outcome: "Yes", TokenID: "pf-yes", Amount: 100,
	}); err != nil {
		t.Fatal(err)
	}
	// The market resolves according to reference data, but Predict.fun has no
	// settler in this build: the position must stay open and no fabricated
	// settlement order may be written.
	pfMarket.Closed = true
	pfMarket.Active = false
	pfMarket.ResolutionStatus = "resolved"
	pfMarket.Outcomes[0].Price = 1
	pfMarket.Outcomes[1].Price = 0
	pf.markets["pf"] = pfMarket
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	open, err := instance.Positions(ctx, paper.ID, "open", "")
	if err != nil || open.Total != 1 || open.Items[0].Status != "open" {
		t.Fatalf("predictfun position must stay open: %+v err=%v", open, err)
	}
	settled, err := instance.Orders(ctx, paper.ID, "settled", 10, 0, "")
	if err != nil || settled.Total != 0 {
		t.Fatalf("no settlement order may be fabricated: %+v err=%v", settled, err)
	}
	runtime := instance.SystemRuntime()
	if !strings.Contains(runtime.Error, "暂不支持自动结算") || !strings.Contains(runtime.Error, "Predict.fun") {
		t.Fatalf("runtime must surface the missing settler: %q", runtime.Error)
	}
}

// Predict.fun publishes resolution.winningIndexSet, the provider maps it onto a
// 1/0 payout, and the engine must then close the position exactly the way it
// closes a Polymarket one.
func TestPredictFunPositionSettlesOnPublishedWinner(t *testing.T) {
	ctx := context.Background()
	pfMarket := binary("pf", 0.40, 20000, 2)
	pfMarket.FeeRateBPS = 200
	pfMarket.Venue = market.VenuePredictFun
	pfMarket.Currency = market.CurrencyUSDT
	pf := &fakeProvider{venue: market.VenuePredictFun, markets: map[string]market.Market{"pf": pfMarket}}
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{}}, nil)
	settling := testCapabilities(market.VenuePredictFun)
	settling.SupportsSettlement = true
	addVenueCaps(t, registry, settling, pf, nil)
	instance, _ := newTestEngineWithRegistry(t, registry)
	settings := instance.Settings()
	settings.MinPrice = 0.10
	settings.MaxPrice = 0.99
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{})
	paper, err := instance.CreateAccount(ctx, "PF paper", engine.AccountModePaper, market.VenuePredictFun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PlaceOrder(ctx, paper.ID, engine.OrderRequest{
		MarketID: "pf", Outcome: "Yes", TokenID: "pf-yes", Amount: 100,
	}); err != nil {
		t.Fatal(err)
	}

	pfMarket.Closed = true
	pfMarket.Active = false
	pfMarket.ResolutionStatus = "resolved"
	pfMarket.Outcomes[0].Price = 1
	pfMarket.Outcomes[1].Price = 0
	pf.markets["pf"] = pfMarket
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}

	won, err := instance.Positions(ctx, paper.ID, "won", "")
	if err != nil {
		t.Fatal(err)
	}
	if won.Total != 1 {
		t.Fatalf("predictfun position must settle as won: %+v", won.Items)
	}
	position := won.Items[0]
	if position.RealizedPNL <= 0 {
		t.Fatalf("a winning position must book a payout: %+v", position)
	}
	settled, err := instance.Orders(ctx, paper.ID, "settled", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if settled.Total != 1 || settled.Items[0].Type != "settle" {
		t.Fatalf("expected one settle order: %+v", settled.Items)
	}
	if strings.Contains(instance.SystemRuntime().Error, "暂不支持自动结算") {
		t.Fatalf("a settling venue must not be reported as unsupported: %q", instance.SystemRuntime().Error)
	}
}

func TestSettingsVenueDefaultsAndValidation(t *testing.T) {
	settings := engine.DefaultSettings()
	settings.Venues = nil
	if !settings.For(market.VenuePolymarket).Enabled {
		t.Fatal("polymarket must be enabled when the venue map is absent")
	}
	if settings.For(market.VenuePredictFun).Enabled {
		t.Fatal("predictfun must be disabled when the venue map is absent")
	}
	settings.Venues = map[market.Venue]engine.VenueSettings{
		market.VenuePredictFun: {OrderSize: 3, MaxPosition: 15, MaxExposure: 45, FeeBPS: 200},
	}
	pf := settings.For(market.VenuePredictFun)
	if pf.Enabled {
		t.Fatal("an entry without enabled=true must not start trading")
	}
	if pf.OrderSize != 3 || pf.MaxPosition != 15 || pf.MaxExposure != 45 || pf.FeeBPS != 200 {
		t.Fatalf("venue overrides were not applied: %+v", pf)
	}
	if settings.For(market.VenuePolymarket).OrderSize != settings.OrderSize {
		t.Fatal("an absent venue must inherit the global order size")
	}
	if err := settings.Validate(); err != nil {
		t.Fatalf("valid venue overrides were rejected: %v", err)
	}

	invalid := settings
	invalid.Venues = map[market.Venue]engine.VenueSettings{
		market.VenuePredictFun: {Enabled: true, OrderSize: 10, MaxPosition: 5, MaxExposure: 20},
	}
	if err := invalid.Validate(); !errors.Is(err, engine.ErrInvalidSettings) || !strings.Contains(err.Error(), "Predict.fun") {
		t.Fatalf("invalid venue caps must name the venue: %v", err)
	}
	unknown := settings
	unknown.Venues = map[market.Venue]engine.VenueSettings{"binance": {Enabled: true}}
	if err := unknown.Validate(); !errors.Is(err, engine.ErrInvalidSettings) {
		t.Fatalf("an unknown venue key must be rejected: %v", err)
	}
}

func TestPredictFunAmountsAreRoundedForAPI(t *testing.T) {
	ctx := context.Background()
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{}}, nil)
	addVenue(t, registry, market.VenuePredictFun, &fakeProvider{
		venue: market.VenuePredictFun, markets: map[string]market.Market{},
	}, nil)
	instance, repository := newTestEngineWithRegistry(t, registry)
	pfPaper, err := instance.CreateAccount(ctx, "PF paper", engine.AccountModePaper, market.VenuePredictFun)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	noisy := 1.0000000000000002
	if _, err := repository.CreatePosition(ctx, engine.Position{
		AccountID: pfPaper.ID, Venue: market.VenuePredictFun, Currency: market.CurrencyUSDT,
		MarketID: "pf", ConditionID: "c-pf", Question: "q", Outcome: "Yes", TokenID: "t-pf",
		Side: "BUY", Kind: "manual", EntryPrice: 0.4, CurrentPrice: 0.4,
		Shares: 10 * noisy, Cost: noisy, Fee: noisy / 10, Status: "open",
		OpenedAt: now, EndDate: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	list, err := instance.Positions(ctx, pfPaper.ID, "all", "")
	if err != nil || list.Total != 1 {
		t.Fatalf("unexpected positions: %+v err=%v", list, err)
	}
	position := list.Items[0]
	if position.Cost != 1 || position.Fee != 0.1 || position.Shares != 10 {
		t.Fatalf("predictfun amounts must be rounded to 6 decimals for the API: %+v", position)
	}
	dashboard, err := instance.Dashboard(ctx, pfPaper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard.Metrics.Venue != market.VenuePredictFun || dashboard.Metrics.Currency != market.CurrencyUSDT {
		t.Fatalf("metrics must be scoped to the account venue: %+v", dashboard.Metrics)
	}
	pmPaper := accountByMode(t, instance, engine.AccountModePaper)
	pmDashboard, err := instance.Dashboard(ctx, pmPaper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pmDashboard.Metrics.Venue != market.VenuePolymarket || pmDashboard.Metrics.Currency != market.CurrencyUSDC {
		t.Fatalf("polymarket metrics must stay on their own venue: %+v", pmDashboard.Metrics)
	}
}

func TestSystemSettingsRejectSharedVenuePrivateKey(t *testing.T) {
	key := "0x" + strings.Repeat("5", 64)
	settings := engine.DefaultSystemSettings()
	settings.Live = engine.LiveSettings{PrivateKey: key}
	settings.Venues[market.VenuePredictFun] = engine.VenueSystemSettings{
		Live: engine.VenueLiveSettings{LiveSettings: engine.LiveSettings{PrivateKey: key, APIKey: "pf-key"}},
	}
	settings = settings.SyncPolymarketVenue()
	err := settings.Validate()
	if err == nil || !strings.Contains(err.Error(), "不能与") {
		t.Fatalf("reusing the Polymarket private key for Predict.fun must be rejected: %v", err)
	}
}

// redeemLive is the venue.Redeemer half of a fake execution venue: it records
// each redemption request so a test can assert that exactly one transaction was
// attempted for a settled position.
type redeemLive struct {
	*fakeLive
	redemptions []venue.RedeemRequest
	err         error
}

func (r *redeemLive) Redeem(_ context.Context, request venue.RedeemRequest) (venue.RedeemResult, error) {
	r.redemptions = append(r.redemptions, request)
	if r.err != nil {
		return venue.RedeemResult{}, r.err
	}
	return venue.RedeemResult{Venue: market.VenuePredictFun, TxHash: "0xredeemed", Method: "ctf"}, nil
}

// TestRedeemPositionBurnsSettledSharesOnce covers the money path: a live position
// settles, one redemption is submitted with the position's own condition id and
// share count, the attempt is recorded as an order, and a second click is refused
// instead of spending gas on a transaction that would revert.
func TestRedeemPositionBurnsSettledSharesOnce(t *testing.T) {
	ctx := context.Background()
	pfMarket := binary("pf", 0.40, 20000, 2)
	pfMarket.FeeRateBPS = 200
	pfMarket.Venue = market.VenuePredictFun
	pfMarket.Currency = market.CurrencyUSDT
	pf := &fakeProvider{venue: market.VenuePredictFun, markets: map[string]market.Market{"pf": pfMarket}}
	pfLive := &redeemLive{fakeLive: &fakeLive{
		balance: 1000, allowance: 1000,
		fill: engine.LiveFill{Shares: 100, Cost: 40, Price: 0.40, Status: "matched"},
	}}
	caps := testCapabilities(market.VenuePredictFun)
	caps.SupportsSettlement = true
	caps.SupportsRedeem = true
	registry := venue.NewRegistry()
	addVenueCaps(t, registry, caps, pf, pfLive)
	instance, _ := newTestEngineWithRegistry(t, registry)
	settings := instance.Settings()
	settings.MinPrice = 0.10
	settings.MaxPrice = 0.99
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{})
	account, err := instance.CreateAccount(ctx, "PF live", engine.AccountModeLive, market.VenuePredictFun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PlaceOrder(ctx, account.ID, engine.OrderRequest{
		MarketID: "pf", Outcome: "Yes", TokenID: "pf-yes", Amount: 40,
	}); err != nil {
		t.Fatal(err)
	}

	pfMarket.Closed = true
	pfMarket.Active = false
	pfMarket.ResolutionStatus = "resolved"
	pfMarket.Outcomes[0].Price = 1
	pfMarket.Outcomes[1].Price = 0
	pf.markets["pf"] = pfMarket
	if err := instance.ScanOnly(ctx); err != nil {
		t.Fatal(err)
	}
	won, err := instance.Positions(ctx, account.ID, "won", "")
	if err != nil || won.Total != 1 {
		t.Fatalf("expected one settled position: %+v err=%v", won.Items, err)
	}
	position := won.Items[0]

	result, err := instance.RedeemPosition(ctx, account.ID, position.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.TxHash != "0xredeemed" || result.Method != "ctf" {
		t.Fatalf("unexpected redemption result: %+v", result)
	}
	if len(pfLive.redemptions) != 1 {
		t.Fatalf("expected exactly one redemption, got %d", len(pfLive.redemptions))
	}
	sent := pfLive.redemptions[0]
	if sent.ConditionID != position.ConditionID || sent.TokenID != position.TokenID {
		t.Fatalf("redemption used the wrong market: %+v position=%+v", sent, position)
	}
	if math.Abs(sent.Shares-position.Shares) > 1e-9 {
		t.Fatalf("redemption shares = %v, want %v", sent.Shares, position.Shares)
	}
	orders, err := instance.Orders(ctx, account.ID, "all", 20, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var redemption *engine.Order
	for i := range orders.Items {
		if orders.Items[i].Action == "redeem" {
			redemption = &orders.Items[i]
		}
	}
	if redemption == nil || redemption.Type != "settle" || redemption.Status != "filled" ||
		!strings.Contains(redemption.Reason, "0xredeemed") {
		t.Fatalf("redemption was not recorded: %+v", orders.Items)
	}

	if _, err := instance.RedeemPosition(ctx, account.ID, position.ID); err == nil {
		t.Fatal("a second redemption of the same position must be refused")
	}
	if len(pfLive.redemptions) != 1 {
		t.Fatalf("a refused redemption must not reach the chain: %d attempts", len(pfLive.redemptions))
	}
}

// TestRedeemPositionRejectsOpenAndSimulated covers the two ways a redemption must
// be refused before any transaction is signed.
func TestRedeemPositionRejectsOpenAndSimulated(t *testing.T) {
	ctx := context.Background()
	pfMarket := binary("pf", 0.40, 20000, 2)
	pfMarket.FeeRateBPS = 200
	pfMarket.Venue = market.VenuePredictFun
	pfMarket.Currency = market.CurrencyUSDT
	pf := &fakeProvider{venue: market.VenuePredictFun, markets: map[string]market.Market{"pf": pfMarket}}
	pfLive := &redeemLive{fakeLive: &fakeLive{
		balance: 1000, allowance: 1000,
		fill: engine.LiveFill{Shares: 100, Cost: 40, Price: 0.40, Status: "matched"},
	}}
	caps := testCapabilities(market.VenuePredictFun)
	caps.SupportsSettlement = true
	caps.SupportsRedeem = true
	registry := venue.NewRegistry()
	addVenueCaps(t, registry, caps, pf, pfLive)
	instance, _ := newTestEngineWithRegistry(t, registry)
	settings := instance.Settings()
	settings.MinPrice = 0.10
	settings.MaxPrice = 0.99
	if err := instance.UpdateSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	enableVenue(t, instance, market.VenuePredictFun, engine.VenueSettings{})
	live, err := instance.CreateAccount(ctx, "PF live", engine.AccountModeLive, market.VenuePredictFun)
	if err != nil {
		t.Fatal(err)
	}
	open, err := instance.PlaceOrder(ctx, live.ID, engine.OrderRequest{
		MarketID: "pf", Outcome: "Yes", TokenID: "pf-yes", Amount: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.RedeemPosition(ctx, live.ID, open.ID); err == nil {
		t.Fatal("an unsettled position must not be redeemable")
	}
	paper, err := instance.CreateAccount(ctx, "PF paper", engine.AccountModePaper, market.VenuePredictFun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.RedeemPosition(ctx, paper.ID, open.ID); err == nil {
		t.Fatal("a simulated position must not be redeemable")
	}
	if len(pfLive.redemptions) != 0 {
		t.Fatalf("no refused redemption may reach the chain: %d attempts", len(pfLive.redemptions))
	}
}
