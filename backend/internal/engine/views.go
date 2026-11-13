package engine

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"pennyharvester/backend/internal/market"
)

func (e *Engine) Opportunities() OpportunityList {
	return e.OpportunitiesFor(0)
}

// OpportunitiesFor returns the radar for one account. A non-positive account ID
// returns the union view, which is what a client without account context (and
// the legacy API) sees.
func (e *Engine) OpportunitiesFor(accountID int64) OpportunityList {
	e.mu.RLock()
	defer e.mu.RUnlock()
	items := e.opportunities
	if accountID > 0 {
		items = e.opportunitiesByAccount[accountID]
	}
	items = append([]Opportunity{}, items...)
	venues := append([]market.Venue{}, e.scannedVenues...)
	return OpportunityList{Items: items, Total: len(items), ScannedAt: e.lastSyncAt, Source: e.sourceName(), Venues: venues}
}

// flattenOpportunities merges per-account radar views into the legacy union
// list. A row visible to several accounts appears once; the radar rows are
// identical because they are built from the same market data.
func flattenOpportunities(views map[int64][]Opportunity) []Opportunity {
	seen := make(map[string]struct{})
	items := make([]Opportunity, 0)
	for _, view := range views {
		for _, item := range view {
			key := opportunityKey(item)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			items = append(items, item)
		}
	}
	sortOpportunities(items)
	return items
}

// opportunityKey identifies one tradable outcome across venues and accounts.
func opportunityKey(opportunity Opportunity) string {
	return marketKey(opportunity.EffectiveVenue(), opportunity.MarketID) + "\x00" + opportunity.TokenID
}

// QuoteOpportunity returns a real-data estimate for one radar row: the ask
// book and taker fee are read from the venue that owns the token, so NetYield
// is net of that venue's fees and uses the price a real order would pay. It is
// read-only and available for paper accounts too.
func (e *Engine) QuoteOpportunity(ctx context.Context, accountID int64, tokenID string, amount float64, onlyVenue market.Venue) (OpportunityQuote, error) {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return OpportunityQuote{}, fmt.Errorf("%w: token_id 不能为空", ErrInvalidOrder)
	}
	if amount < 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return OpportunityQuote{}, fmt.Errorf("%w: 金额必须为有限正数", ErrInvalidOrder)
	}
	settings, err := e.settingsForAccount(ctx, accountID)
	if err != nil {
		return OpportunityQuote{}, err
	}

	e.mu.RLock()
	candidates := e.opportunities
	if accountID > 0 {
		if view, ok := e.opportunitiesByAccount[accountID]; ok {
			candidates = view
		}
	}
	var target Opportunity
	found := false
	lookup := func(items []Opportunity) {
		for _, opportunity := range items {
			if opportunity.TokenID != tokenID {
				continue
			}
			if onlyVenue != "" && opportunity.EffectiveVenue() != onlyVenue {
				continue
			}
			target = opportunity
			found = true
			return
		}
	}
	lookup(candidates)
	if !found && accountID > 0 {
		// The account view is rebuilt on each scan, so settings saved after the
		// last scan can briefly hide a row. Fall back to the union and let the
		// account's own price cap judge the quote.
		lookup(e.opportunities)
	}
	e.mu.RUnlock()
	if !found {
		return OpportunityQuote{}, fmt.Errorf("%w: 该 token 不在当前雷达列表，请先刷新行情", ErrInvalidOrder)
	}
	if amount == 0 {
		amount = settings.For(target.EffectiveVenue()).OrderSize
	}
	exec, err := e.executor(target.EffectiveVenue())
	if err != nil {
		return OpportunityQuote{}, err
	}
	quote, err := exec.Quote(ctx, target.TokenID, target.ConditionID, amount)
	if err != nil {
		return OpportunityQuote{}, fmt.Errorf("%w: 读取真实盘口或手续费失败: %v", ErrUpstream, err)
	}
	totalCost := quote.Notional + quote.Fee
	payout := quote.Shares
	netProfit := payout - totalCost
	netYield := 0.0
	if totalCost > 0 {
		netYield = netProfit / totalCost
	}
	grossYield := 0.0
	if quote.FillPrice > 0 && quote.FillPrice < 1 {
		grossYield = (1 - quote.FillPrice) / quote.FillPrice
	}
	v := target.EffectiveVenue()
	return OpportunityQuote{
		TokenID: target.TokenID, ConditionID: target.ConditionID,
		Venue: v, Currency: target.EffectiveCurrency(), Amount: roundForVenue(v, amount),
		BestAsk: quote.BestAsk, LimitPrice: quote.LimitPrice, FillPrice: quote.FillPrice,
		Shares: roundForVenue(v, quote.Shares), Notional: roundForVenue(v, quote.Notional),
		FeeRate: quote.FeeRate, Fee: roundForVenue(v, quote.Fee),
		TotalCost: roundForVenue(v, totalCost), Payout: roundForVenue(v, payout),
		NetProfit: roundForVenue(v, netProfit), NetYield: netYield,
		GrossYield: grossYield, AvailableNotional: roundForVenue(v, quote.AvailableNotional),
		FullyFilled:     quote.FullyFilled,
		ExceedsMaxPrice: quote.LimitPrice > settings.MaxPrice+1e-9, MaxPrice: settings.MaxPrice,
		QuotedAt: time.Now().UTC(),
	}, nil
}

func (e *Engine) Accounts(ctx context.Context) (AccountList, error) {
	accounts, err := e.repo.ListAccounts(ctx)
	if err != nil {
		return AccountList{}, err
	}
	for i := range accounts {
		accounts[i] = e.decorateAccount(accounts[i])
	}
	return AccountList{Items: accounts, Total: len(accounts)}, nil
}

func (e *Engine) ResolveAccount(ctx context.Context, id int64) (Account, error) {
	var (
		account Account
		err     error
	)
	if id == 0 {
		account, err = e.repo.DefaultAccount(ctx)
	} else {
		account, err = e.repo.AccountByID(ctx, id)
	}
	if err != nil {
		return Account{}, err
	}
	return e.decorateAccount(account), nil
}

func (e *Engine) decorateAccount(account Account) Account {
	account.Venue = account.EffectiveVenue()
	account.Currency = account.EffectiveCurrency()
	account.BaselineEquity = roundForVenue(account.Venue, account.BaselineEquity)
	if account.Mode == AccountModePaper && account.BaselineEquity <= 0 {
		// New() backfills persisted accounts; this is a last-resort display
		// fallback for a caller that constructed an engine without New.
		account.BaselineEquity = e.Settings().InitialBankroll
	}
	account.LiveConfigured = false
	account.LiveReady = false
	account.LiveSigner = ""
	account.LiveMaker = ""
	account.LiveError = ""
	if account.Mode != AccountModeLive {
		return account
	}
	exec, err := e.executor(account.EffectiveVenue())
	if err != nil {
		account.LiveError = ErrVenueReadOnly.Error()
		return account
	}
	account.LiveConfigured = exec.Configured()
	account.LiveError = exec.ReadyError()
	account.LiveReady = account.LiveConfigured && account.LiveError == ""
	account.LiveSigner = exec.Signer()
	account.LiveMaker = exec.Maker()
	if reporter, ok := exec.(interface{ Testnet() bool }); ok {
		account.LiveTestnet = reporter.Testnet()
	}
	return account
}

func (e *Engine) CreateAccount(ctx context.Context, name string, mode AccountMode, v market.Venue) (Account, error) {
	return e.CreateAccountWithBankroll(ctx, name, mode, v, 0)
}

// CreateAccountWithBankroll creates an account with an explicit paper baseline.
// A non-positive initialBankroll keeps the historical behaviour and falls back
// to the strategy block's InitialBankroll, so existing callers are unaffected.
// The value is only meaningful for paper accounts: a live baseline is synced
// from the wallet balance, never from a form field.
func (e *Engine) CreateAccountWithBankroll(ctx context.Context, name string, mode AccountMode, v market.Venue, initialBankroll float64) (Account, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Account{}, fmt.Errorf("%w: 账户名称不能为空", ErrInvalidOrder)
	}
	if len([]rune(name)) > 64 {
		return Account{}, fmt.Errorf("%w: 账户名称不能超过 64 个字符", ErrInvalidOrder)
	}
	if !mode.Valid() {
		return Account{}, fmt.Errorf("%w: 账户模式仅支持 live 或 paper", ErrInvalidOrder)
	}
	v = effectiveVenue(v)
	if !v.Valid() {
		return Account{}, fmt.Errorf("%w: 未知场所", ErrInvalidOrder)
	}
	if math.IsNaN(initialBankroll) || math.IsInf(initialBankroll, 0) || initialBankroll < 0 {
		return Account{}, fmt.Errorf("%w: 模拟初始资金必须是大于 0 的有限数字", ErrInvalidOrder)
	}
	if mode != AccountModePaper {
		initialBankroll = 0
	}
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	accounts, err := e.repo.ListAccounts(ctx)
	if err != nil {
		return Account{}, err
	}
	for _, existing := range accounts {
		// Live accounts are unique per venue: Polymarket and Predict.fun keep
		// separate wallets, so one live account each may coexist.
		if mode == AccountModeLive && existing.Mode == AccountModeLive && existing.EffectiveVenue() == v {
			return Account{}, fmt.Errorf("%w: 每个场所只允许一个实盘账户", ErrAccountConflict)
		}
	}
	account := Account{
		Name: name, Mode: mode, Venue: v, Currency: currencyForVenue(v), AutoTrade: false, CreatedAt: time.Now().UTC(),
	}
	if mode == AccountModePaper {
		// The form value wins; an empty field keeps the account's strategy
		// default so the two entry points never disagree in silence.
		bankroll := initialBankroll
		if bankroll <= 0 {
			bankroll = e.Settings().InitialBankroll
		}
		if bankroll <= 0 {
			return Account{}, fmt.Errorf("%w: 模拟初始资金必须大于 0", ErrInvalidOrder)
		}
		account.BaselineEquity = roundForVenue(v, bankroll)
	}
	account, err = e.repo.CreateAccount(ctx, account)
	if err != nil {
		return Account{}, err
	}
	return e.decorateAccount(account), nil
}

func (e *Engine) UpdateAccount(ctx context.Context, id int64, name *string, autoTrade *bool, makeDefault *bool, v *market.Venue) (Account, error) {
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	account, err := e.repo.AccountByID(ctx, id)
	if err != nil {
		return Account{}, err
	}
	if v != nil {
		// An account's venue decides which wallet and currency its ledger is
		// denominated in. Changing it after the fact would silently reinterpret
		// every historical row, so it is rejected instead of migrated.
		requested := effectiveVenue(*v)
		if !requested.Valid() {
			return Account{}, fmt.Errorf("%w: 未知场所", ErrInvalidOrder)
		}
		if requested != account.EffectiveVenue() {
			return Account{}, fmt.Errorf("%w: 账户所属场所不能修改", ErrInvalidOrder)
		}
	}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" {
			return Account{}, fmt.Errorf("%w: 账户名称不能为空", ErrInvalidOrder)
		}
		if len([]rune(trimmed)) > 64 {
			return Account{}, fmt.Errorf("%w: 账户名称不能超过 64 个字符", ErrInvalidOrder)
		}
		account.Name = trimmed
	}
	if autoTrade != nil {
		if *autoTrade && account.Mode == AccountModeLive {
			exec, execErr := e.executor(account.EffectiveVenue())
			if execErr != nil {
				return Account{}, fmt.Errorf("%w: 配置实盘凭据后才能开启自动交易", ErrLiveUnavailable)
			}
			if !exec.Configured() {
				return Account{}, fmt.Errorf("%w: 配置实盘凭据后才能开启自动交易", ErrVenueNotConfigured)
			}
			if message := exec.ReadyError(); message != "" {
				return Account{}, fmt.Errorf("%w: 实盘凭据未就绪: %s", ErrVenueNotConfigured, message)
			}
		}
		account.AutoTrade = *autoTrade
	}
	if err := e.repo.UpdateAccount(ctx, account); err != nil {
		return Account{}, err
	}
	if makeDefault != nil && *makeDefault {
		if err := e.repo.SetDefaultAccount(ctx, account.ID); err != nil {
			return Account{}, err
		}
		account.IsDefault = true
	}
	account, err = e.repo.AccountByID(ctx, account.ID)
	if err != nil {
		return Account{}, err
	}
	return e.decorateAccount(account), nil
}

func (e *Engine) DeleteAccount(ctx context.Context, id int64) error {
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	account, err := e.repo.AccountByID(ctx, id)
	if err != nil {
		return err
	}
	if account.IsDefault {
		return fmt.Errorf("%w: 默认账户不能删除，请先切换默认账户", ErrAccountConflict)
	}
	if account.Mode == AccountModeLive {
		return fmt.Errorf("%w: 实盘账户不能删除", ErrAccountConflict)
	}
	return e.repo.DeleteAccount(ctx, id)
}

func (e *Engine) Positions(ctx context.Context, accountID int64, status string, v market.Venue) (PositionList, error) {
	items, err := e.repo.ListPositions(ctx, accountID, status, v)
	if err != nil {
		return PositionList{}, err
	}
	for i := range items {
		items[i] = sanitizePosition(items[i])
	}
	return PositionList{Items: items, Total: len(items)}, nil
}

func (e *Engine) Orders(ctx context.Context, accountID int64, status string, limit, offset int, v market.Venue) (OrderList, error) {
	items, err := e.repo.ListOrders(ctx, accountID, status, limit, offset, v)
	if err != nil {
		return OrderList{}, err
	}
	for i := range items {
		items[i] = sanitizeOrder(items[i])
	}
	total, err := e.repo.CountOrders(ctx, accountID, status, v)
	if err != nil {
		return OrderList{}, err
	}
	return OrderList{Items: items, Total: int(total)}, nil
}

func (e *Engine) Dashboard(ctx context.Context, accountID int64) (Dashboard, error) {
	account, err := e.ResolveAccount(ctx, accountID)
	if err != nil {
		return Dashboard{}, err
	}
	// Positions are scoped by account, and every account belongs to one venue,
	// so the sums below can never mix USDC with USDT.
	positions, err := e.repo.ListPositions(ctx, account.ID, "all", "")
	if err != nil {
		return Dashboard{}, err
	}
	settings, err := e.settingsForAccount(ctx, account.ID)
	if err != nil {
		return Dashboard{}, err
	}
	metrics := Metrics{
		Mode: string(account.Mode), Venue: account.EffectiveVenue(), Currency: account.EffectiveCurrency(),
		InitialBankroll: settings.InitialBankroll,
	}
	wins, marketValue := 0, 0.0
	for _, position := range positions {
		metrics.FeesPaid += position.Fee
		metrics.RealizedPNL += position.RealizedPNL
		if position.Status == "open" {
			metrics.OpenPositions++
			metrics.OpenExposure += position.Cost
			metrics.UnrealizedPNL += position.UnrealizedPNL
			marketValue += position.CurrentPrice * position.Shares
		} else {
			metrics.SettledPositions++
			if position.ExitKind == "stop" {
				metrics.StoppedPositions++
			}
			switch position.Status {
			case "won":
				wins++
			case "split":
				metrics.SplitPositions++
			}
		}
	}
	metrics.TotalPNL = metrics.RealizedPNL + metrics.UnrealizedPNL
	if account.Mode == AccountModePaper {
		baseline := account.BaselineEquity
		if baseline <= 0 {
			baseline = settings.InitialBankroll
		}
		metrics.Cash = baseline + metrics.RealizedPNL - metrics.OpenExposure
		metrics.Equity = metrics.Cash + marketValue
		metrics.InitialBankroll = baseline
		if metrics.InitialBankroll > 0 {
			metrics.TotalReturn = metrics.TotalPNL / metrics.InitialBankroll
		}
	} else {
		// One bounded context covers the baseline and the wallet read: both are
		// venue calls, and neither may outlive the dashboard's own budget.
		liveCtx, cancelLive := context.WithTimeout(ctx, liveBalanceTimeout)
		defer cancelLive()
		account = e.ensureLiveBaseline(liveCtx, account, metrics.OpenExposure, metrics.RealizedPNL)
		if account.BaselineEquity <= 0 {
			metrics.InitialBankroll = 0
		} else {
			metrics.InitialBankroll = account.BaselineEquity
		}
		metrics.Cash = account.BaselineEquity + metrics.RealizedPNL - metrics.OpenExposure
		metrics.Equity = metrics.Cash + marketValue
		if metrics.InitialBankroll > 0 {
			metrics.TotalReturn = metrics.TotalPNL / metrics.InitialBankroll
		}
		exec, execErr := e.executor(account.EffectiveVenue())
		if execErr == nil && exec.Configured() {
			snapshot, cashErr := exec.Collateral(liveCtx)
			if cashErr != nil {
				metrics.WalletCashStale = true
				if account.LiveError == "" {
					account.LiveError = cashErr.Error()
				}
			} else {
				metrics.WalletCash = snapshot.Amount
				metrics.WalletCashStale = snapshot.Stale
			}
		} else {
			metrics.WalletCashStale = true
		}
	}
	// A stop-loss exit is a decided outcome: it settles for cash and lost money,
	// so it must sit in the win-rate denominator as a loss. Excluding it would
	// report a 100% win rate for a book that contains losing trades.
	decided := metrics.SettledPositions - metrics.SplitPositions
	if decided > 0 {
		metrics.WinRate = float64(wins) / float64(decided)
	}
	startedAt, err := e.repo.EarliestAutoBuyAt(ctx, account.ID)
	if err != nil {
		return Dashboard{}, err
	}
	if startedAt != nil {
		metrics.AutoTradeStartedAt = startedAt
		metrics.AnnualizedDays = math.Round(time.Since(*startedAt).Hours()/24*100) / 100
		metrics.AnnualizedReturn = annualizedReturn(metrics.TotalReturn, metrics.AnnualizedDays)
	}
	metrics.TotalOrders, err = e.repo.CountOrders(ctx, account.ID, "all", "")
	if err != nil {
		return Dashboard{}, err
	}
	points, err := e.repo.ListSnapshots(ctx, account.ID, 500)
	if err != nil {
		return Dashboard{}, err
	}
	if len(points) == 0 {
		points = []EquityPoint{{
			Venue: account.EffectiveVenue(), Currency: account.EffectiveCurrency(),
			Time: time.Now().UTC(), Value: metrics.Equity,
		}}
	}
	for i := range points {
		points[i] = sanitizeEquityPoint(points[i])
	}
	metrics = sanitizeMetrics(metrics)
	e.mu.RLock()
	source := SourceStatus{Name: e.sourceName(), LastSyncAt: e.lastSyncAt}
	e.mu.RUnlock()
	source.Health = e.dataHealth()
	return Dashboard{
		Account: e.decorateAccount(account),
		Metrics: metrics,
		Engine:  e.statusFor(account.Mode),
		Source:  source,
		Equity:  points,
	}, nil
}

func (e *Engine) ensureLiveBaseline(ctx context.Context, account Account, openExposure, realized float64) Account {
	exec, err := e.executor(account.EffectiveVenue())
	if account.Mode != AccountModeLive || account.BaselineEquity > 0 || err != nil || !exec.Configured() {
		return account
	}
	snapshot, snapshotErr := exec.Collateral(ctx)
	if snapshotErr != nil {
		return account
	}
	// The wallet balance may already reflect open positions from this app, so
	// add the tracked exposure back to recover the starting capital.
	baseline := snapshot.Amount + openExposure - realized
	if baseline <= 0 {
		return account
	}
	updated, err := e.repo.InitializeAccountBaseline(ctx, account.ID, baseline)
	if err != nil {
		return account
	}
	if !updated {
		// Another path won the bootstrap. Its value is authoritative; do not
		// overwrite it with this possibly stale exposure view.
		if fresh, err := e.repo.AccountByID(ctx, account.ID); err == nil {
			return fresh
		}
		return account
	}
	account.BaselineEquity = baseline
	return account
}

func (e *Engine) snapshot(ctx context.Context, account Account) error {
	if account.Mode == AccountModeLive && account.BaselineEquity <= 0 {
		return nil
	}
	dashboard, err := e.Dashboard(ctx, account.ID)
	if err != nil {
		return err
	}
	if account.Mode == AccountModeLive && dashboard.Metrics.WalletCashStale && dashboard.Metrics.WalletCash == 0 {
		return nil
	}
	return e.repo.AddSnapshot(ctx, account.ID, EquityPoint{
		Venue: account.EffectiveVenue(), Currency: account.EffectiveCurrency(),
		Time: time.Now().UTC(), Value: dashboard.Metrics.Equity,
	})
}

// moneyPrecision is the number of decimals the API exposes for a venue's
// amounts. Predict.fun signs 18-decimal integers, but showing that scale back
// as a float64 leaks representation noise; six decimals is what a human reads
// and what the Polymarket side already carries.
const moneyPrecision = 6

// roundForVenue rounds a Predict.fun amount to six visible decimals. A
// Polymarket value is returned untouched so existing rows and responses stay
// byte-identical.
func roundForVenue(v market.Venue, value float64) float64 {
	if effectiveVenue(v) == market.VenuePolymarket {
		return value
	}
	scale := math.Pow10(moneyPrecision)
	return math.Round(value*scale) / scale
}

func sanitizePosition(p Position) Position {
	if p.EffectiveVenue() == market.VenuePolymarket {
		return p
	}
	v := p.EffectiveVenue()
	p.EntryPrice = roundForVenue(v, p.EntryPrice)
	p.CurrentPrice = roundForVenue(v, p.CurrentPrice)
	p.Shares = roundForVenue(v, p.Shares)
	p.Cost = roundForVenue(v, p.Cost)
	p.Fee = roundForVenue(v, p.Fee)
	p.RealizedPNL = roundForVenue(v, p.RealizedPNL)
	p.UnrealizedPNL = roundForVenue(v, p.UnrealizedPNL)
	p.Venue = v
	p.Currency = p.EffectiveCurrency()
	return p
}

func sanitizeOrder(o Order) Order {
	if o.EffectiveVenue() == market.VenuePolymarket {
		return o
	}
	v := o.EffectiveVenue()
	o.Amount = roundForVenue(v, o.Amount)
	o.Price = roundForVenue(v, o.Price)
	o.Shares = roundForVenue(v, o.Shares)
	o.Fee = roundForVenue(v, o.Fee)
	o.Venue = v
	o.Currency = o.EffectiveCurrency()
	return o
}

func sanitizeEquityPoint(p EquityPoint) EquityPoint {
	p.Venue = effectiveVenue(p.Venue)
	if p.Currency == "" {
		p.Currency = currencyForVenue(p.Venue)
	}
	p.Value = roundForVenue(p.Venue, p.Value)
	return p
}

// annualizedReturn compounds a period return over the elapsed days. Below one
// day it returns 0 instead of amplifying hours of noise into a three-digit
// number, and a wiped-out book reports -100% rather than a NaN root.
func annualizedReturn(totalReturn, days float64) float64 {
	if days < 1 {
		return 0
	}
	growth := 1 + totalReturn
	if growth <= 0 {
		return -1
	}
	return math.Pow(growth, 365/days) - 1
}

func sanitizeMetrics(m Metrics) Metrics {
	m.Venue = effectiveVenue(m.Venue)
	if m.Currency == "" {
		m.Currency = currencyForVenue(m.Venue)
	}
	v := m.Venue
	m.InitialBankroll = roundForVenue(v, m.InitialBankroll)
	m.Cash = roundForVenue(v, m.Cash)
	m.WalletCash = roundForVenue(v, m.WalletCash)
	m.Equity = roundForVenue(v, m.Equity)
	m.RealizedPNL = roundForVenue(v, m.RealizedPNL)
	m.UnrealizedPNL = roundForVenue(v, m.UnrealizedPNL)
	m.TotalPNL = roundForVenue(v, m.TotalPNL)
	m.FeesPaid = roundForVenue(v, m.FeesPaid)
	m.OpenExposure = roundForVenue(v, m.OpenExposure)
	m.TotalReturn = roundForVenue(v, m.TotalReturn)
	return m
}

func timePointer(value time.Time) *time.Time { return &value }
