package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"pennyharvester/backend/internal/market"
)

type Engine struct {
	repo     Repository
	provider market.Provider
	live     LiveExecutor

	tradeMu       sync.Mutex
	mu            sync.RWMutex
	settings      Settings
	opportunities []Opportunity
	lastScanAt    *time.Time
	lastSyncAt    *time.Time
	lastError     string
	scanCount     int64
	running       bool

	startOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

func New(ctx context.Context, repo Repository, provider market.Provider, live LiveExecutor) (*Engine, error) {
	settings, err := repo.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}
	return &Engine{
		repo: repo, provider: provider, live: live, settings: settings,
		opportunities: []Opportunity{}, running: true, done: make(chan struct{}),
	}, nil
}

func (e *Engine) Start(parent context.Context) {
	e.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		e.cancel = cancel
		go e.run(ctx)
	})
}

func (e *Engine) Stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
	}
}

func (e *Engine) run(ctx context.Context) {
	defer close(e.done)
	for {
		if e.isRunning() {
			scanCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			_ = e.Scan(scanCtx)
			cancel()
		}
		settings := e.Settings()
		timer := time.NewTimer(time.Duration(settings.ScanIntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (e *Engine) Settings() Settings {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.settings
}

func (e *Engine) UpdateSettings(ctx context.Context, settings Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	if err := e.repo.SaveSettings(ctx, settings); err != nil {
		return err
	}
	e.mu.Lock()
	e.settings = settings
	e.mu.Unlock()
	return nil
}

func (e *Engine) Status() EngineStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return EngineStatus{Running: e.running, LastScanAt: e.lastScanAt, LastError: e.lastError, ScanCount: e.scanCount}
}

func (e *Engine) statusFor(mode AccountMode) EngineStatus {
	status := e.Status()
	status.Mode = string(mode)
	return status
}

func (e *Engine) SetRunning(running bool) EngineStatus {
	e.mu.Lock()
	e.running = running
	e.mu.Unlock()
	return e.Status()
}

func (e *Engine) isRunning() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.running
}

func (e *Engine) Opportunities() OpportunityList {
	e.mu.RLock()
	defer e.mu.RUnlock()
	items := append([]Opportunity{}, e.opportunities...)
	return OpportunityList{Items: items, Total: len(items), ScannedAt: e.lastSyncAt, Source: e.provider.Name()}
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
	if account.Mode == AccountModePaper && account.BaselineEquity <= 0 {
		account.BaselineEquity = e.Settings().InitialBankroll
	}
	account.LiveConfigured = false
	account.LiveReady = false
	account.LiveSigner = ""
	account.LiveError = ""
	if account.Mode != AccountModeLive {
		return account
	}
	if e.live == nil {
		account.LiveError = "未配置实盘执行器"
		return account
	}
	account.LiveConfigured = e.live.Configured()
	account.LiveReady = account.LiveConfigured && e.live.ReadyError() == ""
	account.LiveSigner = e.live.Signer()
	account.LiveError = e.live.ReadyError()
	return account
}

func (e *Engine) CreateAccount(ctx context.Context, name string, mode AccountMode) (Account, error) {
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
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	accounts, err := e.repo.ListAccounts(ctx)
	if err != nil {
		return Account{}, err
	}
	for _, existing := range accounts {
		if mode == AccountModeLive && existing.Mode == AccountModeLive {
			return Account{}, fmt.Errorf("%w: 每个实例只允许一个实盘账户", ErrAccountConflict)
		}
	}
	account := Account{
		Name: name, Mode: mode, AutoTrade: false, CreatedAt: time.Now().UTC(),
	}
	if mode == AccountModePaper {
		account.BaselineEquity = e.Settings().InitialBankroll
	}
	account, err = e.repo.CreateAccount(ctx, account)
	if err != nil {
		return Account{}, err
	}
	return e.decorateAccount(account), nil
}

func (e *Engine) UpdateAccount(ctx context.Context, id int64, name *string, autoTrade *bool, makeDefault *bool) (Account, error) {
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	account, err := e.repo.AccountByID(ctx, id)
	if err != nil {
		return Account{}, err
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
			if e.live == nil || !e.live.Configured() {
				return Account{}, fmt.Errorf("%w: 配置实盘凭据后才能开启自动交易", ErrLiveUnavailable)
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

func (e *Engine) Scan(ctx context.Context) error {
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()

	settings := e.Settings()
	markets, err := e.provider.ListOpen(ctx, settings.MaxDays)
	now := time.Now().UTC()
	e.recordScan(now, err)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}

	opportunities := make([]Opportunity, 0)
	byID := make(map[string]market.Market, len(markets))
	for _, item := range markets {
		byID[item.ID] = item
		for _, outcome := range item.Outcomes {
			opportunity, candidateErr := findOpportunity(item, outcome.Name, outcome.TokenID, settings, now)
			if candidateErr == nil {
				opportunities = append(opportunities, opportunity)
			}
		}
	}
	sort.Slice(opportunities, func(i, j int) bool {
		if opportunities[i].EndDate.Equal(opportunities[j].EndDate) {
			return opportunities[i].GrossYield > opportunities[j].GrossYield
		}
		return opportunities[i].EndDate.Before(opportunities[j].EndDate)
	})

	e.mu.Lock()
	e.opportunities = opportunities
	e.lastSyncAt = timePointer(now)
	e.mu.Unlock()

	accounts, err := e.repo.ListAccounts(ctx)
	if err != nil {
		e.setProcessingError(err.Error())
		return err
	}
	var processingErrors []string
	for _, account := range accounts {
		if err := e.refreshPositions(ctx, account.ID, byID); err != nil {
			processingErrors = append(processingErrors, fmt.Sprintf("%s: %v", account.Name, err))
		}
		if e.isRunning() && account.AutoTrade {
			if err := e.autoTrade(ctx, account, opportunities, settings); err != nil {
				processingErrors = append(processingErrors, fmt.Sprintf("%s: %v", account.Name, err))
			}
		}
		if err := e.snapshot(ctx, account); err != nil {
			processingErrors = append(processingErrors, fmt.Sprintf("%s: %v", account.Name, err))
		}
	}
	e.setProcessingError(strings.Join(processingErrors, "; "))
	return nil
}

func (e *Engine) recordScan(at time.Time, scanErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scanCount++
	e.lastScanAt = timePointer(at)
	if scanErr != nil {
		e.lastError = scanErr.Error()
	} else {
		e.lastError = ""
	}
}

func (e *Engine) setProcessingError(message string) {
	e.mu.Lock()
	e.lastError = message
	e.mu.Unlock()
}

func (e *Engine) refreshPositions(ctx context.Context, accountID int64, markets map[string]market.Market) error {
	positions, err := e.repo.ListPositions(ctx, accountID, "open")
	if err != nil {
		return err
	}
	var errs []string
	for _, position := range positions {
		item, ok := markets[position.MarketID]
		if !ok {
			item, err = e.provider.Get(ctx, position.MarketID)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", position.MarketID, err))
				continue
			}
		}
		outcome, ok := item.OutcomeByToken(position.TokenID)
		if !ok {
			errs = append(errs, position.MarketID+": 持仓 token 已不存在")
			continue
		}
		if settlementPrice, resolved := item.SettlementPrice(position.TokenID); resolved {
			closedAt := time.Now().UTC()
			position.ClosedAt = &closedAt
			position.UnrealizedPNL = 0
			position.CurrentPrice = settlementPrice
			payout := position.Shares * settlementPrice
			if settlementPrice == 1 {
				position.Status = "won"
			} else {
				position.Status = "lost"
			}
			position.RealizedPNL = payout - position.Cost
			order := Order{
				AccountID: accountID, PositionID: &position.ID, MarketID: position.MarketID, Question: position.Question,
				Outcome: position.Outcome, TokenID: position.TokenID, Kind: "auto", Type: "settle",
				Amount: payout, Price: position.CurrentPrice, Shares: position.Shares, Status: "settled",
				Reason: fmt.Sprintf("依据公开结算数据按 %.4f 结算", settlementPrice), CreatedAt: closedAt,
			}
			if err := e.repo.SettlePosition(ctx, position, order); err != nil {
				errs = append(errs, err.Error())
			}
			continue
		}
		unrealized := outcome.Price*position.Shares - position.Cost
		if err := e.repo.UpdateMark(ctx, position.ID, outcome.Price, unrealized); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (e *Engine) autoTrade(ctx context.Context, account Account, opportunities []Opportunity, settings Settings) error {
	if account.Mode == AccountModeLive {
		return e.autoLiveTrade(ctx, account, opportunities, settings)
	}
	return e.autoPaperTrade(ctx, account, opportunities, settings)
}

func (e *Engine) autoPaperTrade(ctx context.Context, account Account, opportunities []Opportunity, settings Settings) error {
	positions, err := e.repo.ListPositions(ctx, account.ID, "open")
	if err != nil {
		return err
	}
	heldTokens := make(map[string]bool, len(positions))
	marketExposure := make(map[string]float64)
	totalExposure := 0.0
	for _, position := range positions {
		heldTokens[position.TokenID] = true
		marketExposure[position.MarketID] += position.Cost
		totalExposure += position.Cost
	}
	for _, opportunity := range opportunities {
		if heldTokens[opportunity.TokenID] {
			continue
		}
		fee := settings.OrderSize * settings.FeeBPS / 10000
		cost := settings.OrderSize + fee
		fillPrice := opportunity.Price * (1 + settings.SlippageBPS/10000)
		if fillPrice > settings.MaxPrice || fillPrice >= 1 {
			continue
		}
		if totalExposure+cost > settings.MaxExposure+1e-9 {
			break
		}
		if marketExposure[opportunity.MarketID]+cost > settings.MaxPosition+1e-9 {
			continue
		}
		position, err := e.executePaperBuy(ctx, account.ID, opportunity, "auto", settings.OrderSize)
		if err != nil {
			// A risk rejection is already logged as an order; it must not stop
			// the engine from evaluating the remaining candidates this round.
			if errors.Is(err, ErrInvalidOrder) {
				continue
			}
			return err
		}
		heldTokens[position.TokenID] = true
		marketExposure[position.MarketID] += position.Cost
		totalExposure += position.Cost
	}
	return nil
}

func (e *Engine) autoLiveTrade(ctx context.Context, account Account, opportunities []Opportunity, settings Settings) error {
	if e.live == nil || !e.live.Configured() {
		return fmt.Errorf("%w: 实盘自动交易已开启，但未配置 POLYMARKET_PRIVATE_KEY", ErrLiveUnavailable)
	}
	positions, err := e.repo.ListPositions(ctx, account.ID, "all")
	if err != nil {
		return err
	}
	heldTokens := make(map[string]bool)
	marketExposure := make(map[string]float64)
	totalExposure, realized := 0.0, 0.0
	for _, position := range positions {
		heldTokens[position.TokenID] = true
		if position.Status == "open" {
			marketExposure[position.MarketID] += position.Cost
			totalExposure += position.Cost
		} else {
			realized += position.RealizedPNL
		}
	}
	if account.BaselineEquity <= 0 {
		account = e.ensureLiveBaseline(ctx, account, totalExposure, realized)
	}
	strategyCash := account.BaselineEquity + realized - totalExposure
	snapshot, err := e.live.Collateral(ctx)
	if err != nil {
		return fmt.Errorf("%w: 读取实盘 USDC 余额失败: %v", ErrLiveUnavailable, err)
	}
	cash := math.Min(strategyCash, snapshot.Amount)
	for _, opportunity := range opportunities {
		if heldTokens[opportunity.TokenID] {
			continue
		}
		budget := settings.OrderSize
		if totalExposure+budget > settings.MaxExposure+1e-9 {
			break
		}
		if marketExposure[opportunity.MarketID]+budget > settings.MaxPosition+1e-9 {
			continue
		}
		if budget > cash+1e-9 {
			return fmt.Errorf("%w: 实盘策略资金不足", ErrInvalidOrder)
		}
		position, err := e.executeLiveBuy(ctx, account, opportunity, "auto", budget)
		if err != nil {
			if errors.Is(err, ErrInvalidOrder) || errors.Is(err, ErrLiveUnavailable) {
				continue
			}
			return err
		}
		heldTokens[position.TokenID] = true
		marketExposure[position.MarketID] += position.Cost
		totalExposure += position.Cost
		cash -= position.Cost
	}
	return nil
}

type OrderRequest struct {
	MarketID string  `json:"market_id" binding:"required"`
	Outcome  string  `json:"outcome" binding:"required"`
	TokenID  string  `json:"token_id" binding:"required"`
	Amount   float64 `json:"amount" binding:"required"`
}

func (e *Engine) PlaceOrder(ctx context.Context, accountID int64, request OrderRequest) (Position, error) {
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	if request.Amount <= 0 || math.IsNaN(request.Amount) || math.IsInf(request.Amount, 0) {
		return Position{}, fmt.Errorf("%w: 金额必须为有限正数", ErrInvalidOrder)
	}
	account, err := e.repo.AccountByID(ctx, accountID)
	if err != nil {
		return Position{}, err
	}
	item, err := e.provider.Get(ctx, request.MarketID)
	if err != nil {
		return Position{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	opportunity, err := findOpportunity(item, request.Outcome, request.TokenID, e.Settings(), time.Now().UTC())
	if err != nil {
		return Position{}, err
	}
	if account.Mode == AccountModeLive {
		return e.executeLiveBuy(ctx, account, opportunity, "manual", request.Amount)
	}
	return e.executePaperBuy(ctx, account.ID, opportunity, "manual", request.Amount)
}

func (e *Engine) executePaperBuy(ctx context.Context, accountID int64, opportunity Opportunity, kind string, amount float64) (Position, error) {
	settings := e.Settings()
	fee := amount * settings.FeeBPS / 10000
	cost := amount + fee
	fillPrice := opportunity.Price * (1 + settings.SlippageBPS/10000)
	if fillPrice > settings.MaxPrice || fillPrice >= 1 {
		return Position{}, e.rejectOrder(ctx, accountID, opportunity, kind, amount, "计入滑点后超过最高买入价", "")
	}
	positions, err := e.repo.ListPositions(ctx, accountID, "all")
	if err != nil {
		return Position{}, err
	}
	marketExposure, totalExposure, realized := 0.0, 0.0, 0.0
	for _, position := range positions {
		if position.Status == "open" {
			totalExposure += position.Cost
			if position.MarketID == opportunity.MarketID {
				marketExposure += position.Cost
			}
		} else {
			realized += position.RealizedPNL
		}
	}
	cash := settings.InitialBankroll + realized - totalExposure
	reason := ""
	switch {
	case cost > cash+1e-9:
		reason = "模拟可用现金不足"
	case totalExposure+cost > settings.MaxExposure+1e-9:
		reason = "超过总敞口上限"
	case marketExposure+cost > settings.MaxPosition+1e-9:
		reason = "超过单市场敞口上限"
	}
	if reason != "" {
		return Position{}, e.rejectOrder(ctx, accountID, opportunity, kind, amount, reason, "")
	}

	now := time.Now().UTC()
	shares := amount / fillPrice
	position := Position{
		AccountID: accountID,
		MarketID:  opportunity.MarketID, ConditionID: opportunity.ConditionID,
		Question: opportunity.Question, Outcome: opportunity.Outcome, TokenID: opportunity.TokenID,
		Side: "BUY", EntryPrice: fillPrice, CurrentPrice: opportunity.Price,
		Shares: shares, Cost: cost, Fee: fee, Status: "open", OpenedAt: now,
		EndDate: opportunity.EndDate, UnrealizedPNL: opportunity.Price*shares - cost,
	}
	position, err = e.repo.CreatePosition(ctx, position)
	if err != nil {
		return Position{}, err
	}
	_, err = e.repo.CreateOrder(ctx, Order{
		AccountID: accountID, PositionID: &position.ID, MarketID: position.MarketID, Question: position.Question,
		Outcome: position.Outcome, TokenID: position.TokenID, Kind: kind, Type: "buy",
		Amount: amount, Price: fillPrice, Shares: shares, Status: "filled",
		Reason: "模拟成交；未发送任何真实订单", CreatedAt: now,
	})
	if err != nil {
		return Position{}, fmt.Errorf("持仓已保存，但订单日志写入失败: %w", err)
	}
	return position, nil
}

func (e *Engine) executeLiveBuy(ctx context.Context, account Account, opportunity Opportunity, kind string, amount float64) (Position, error) {
	if e.live == nil || !e.live.Configured() {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			"实盘未配置：请设置 POLYMARKET_PRIVATE_KEY 后重启服务", "")
	}
	settings := e.Settings()
	positions, err := e.repo.ListPositions(ctx, account.ID, "all")
	if err != nil {
		return Position{}, err
	}
	marketExposure, totalExposure, realized := 0.0, 0.0, 0.0
	for _, position := range positions {
		if position.Status == "open" {
			totalExposure += position.Cost
			if position.MarketID == opportunity.MarketID {
				marketExposure += position.Cost
			}
		} else {
			realized += position.RealizedPNL
		}
	}
	if account.BaselineEquity <= 0 {
		account = e.ensureLiveBaseline(ctx, account, totalExposure, realized)
	}
	strategyCash := account.BaselineEquity + realized - totalExposure
	snapshot, cashErr := e.live.Collateral(ctx)
	if cashErr != nil {
		_ = e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "无法读取实盘 USDC 余额："+cashErr.Error(), "")
		return Position{}, fmt.Errorf("%w: 无法读取实盘 USDC 余额: %v", ErrLiveUnavailable, cashErr)
	}
	cash := math.Min(strategyCash, snapshot.Amount)
	switch {
	case amount > cash+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘策略资金不足", "")
	case totalExposure+amount > settings.MaxExposure+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "超过总敞口上限", "")
	case marketExposure+amount > settings.MaxPosition+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "超过单市场敞口上限", "")
	}

	fill, err := e.live.Buy(ctx, opportunity.TokenID, amount, settings.MaxPrice)
	if err != nil {
		_ = e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘下单失败："+err.Error(), "")
		if errors.Is(err, ErrInvalidOrder) {
			return Position{}, err
		}
		return Position{}, fmt.Errorf("%w: 实盘下单失败: %v", ErrUpstream, err)
	}
	if fill.Shares <= 0 || fill.Cost <= 0 {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			"实盘订单未产生成交", fill.OrderID)
	}
	now := time.Now().UTC()
	position := Position{
		AccountID: account.ID,
		MarketID:  opportunity.MarketID, ConditionID: opportunity.ConditionID,
		Question: opportunity.Question, Outcome: opportunity.Outcome, TokenID: opportunity.TokenID,
		Side: "BUY", EntryPrice: fill.Price, CurrentPrice: opportunity.Price,
		Shares: fill.Shares, Cost: fill.Cost, Fee: fill.Fee, Status: "open", OpenedAt: now,
		EndDate: opportunity.EndDate, UnrealizedPNL: opportunity.Price*fill.Shares - fill.Cost,
	}
	position, err = e.repo.CreatePosition(ctx, position)
	if err != nil {
		return Position{}, err
	}
	_, err = e.repo.CreateOrder(ctx, Order{
		AccountID: account.ID, PositionID: &position.ID, MarketID: position.MarketID, Question: position.Question,
		Outcome: position.Outcome, TokenID: position.TokenID, Kind: kind, Type: "buy",
		Amount: fill.Cost, Price: fill.Price, Shares: fill.Shares, Status: "filled",
		Reason: "Polymarket CLOB 实盘成交", ExternalID: fill.OrderID, CreatedAt: now,
	})
	if err != nil {
		return Position{}, fmt.Errorf("实盘持仓已保存，但订单日志写入失败: %w", err)
	}
	return position, nil
}

func (e *Engine) rejectOrder(ctx context.Context, accountID int64, opportunity Opportunity, kind string, amount float64, reason, externalID string) error {
	_, _ = e.repo.CreateOrder(ctx, Order{
		AccountID: accountID, MarketID: opportunity.MarketID, Question: opportunity.Question,
		Outcome: opportunity.Outcome, TokenID: opportunity.TokenID, Kind: kind, Type: "buy",
		Amount: amount, Price: opportunity.Price, Status: "rejected", Reason: reason,
		ExternalID: externalID, CreatedAt: time.Now().UTC(),
	})
	if strings.HasPrefix(reason, "实盘未配置") {
		return fmt.Errorf("%w: %s", ErrLiveUnavailable, reason)
	}
	return fmt.Errorf("%w: %s", ErrInvalidOrder, reason)
}

func (e *Engine) Positions(ctx context.Context, accountID int64, status string) (PositionList, error) {
	items, err := e.repo.ListPositions(ctx, accountID, status)
	return PositionList{Items: items, Total: len(items)}, err
}

func (e *Engine) Orders(ctx context.Context, accountID int64, limit int) (OrderList, error) {
	items, err := e.repo.ListOrders(ctx, accountID, limit)
	return OrderList{Items: items, Total: len(items)}, err
}

func (e *Engine) Dashboard(ctx context.Context, accountID int64) (Dashboard, error) {
	account, err := e.ResolveAccount(ctx, accountID)
	if err != nil {
		return Dashboard{}, err
	}
	positions, err := e.repo.ListPositions(ctx, account.ID, "all")
	if err != nil {
		return Dashboard{}, err
	}
	settings := e.Settings()
	metrics := Metrics{Mode: string(account.Mode), InitialBankroll: settings.InitialBankroll}
	wins, marketValue := 0, 0.0
	for _, position := range positions {
		if position.Status == "open" {
			metrics.OpenPositions++
			metrics.OpenExposure += position.Cost
			metrics.UnrealizedPNL += position.UnrealizedPNL
			marketValue += position.CurrentPrice * position.Shares
		} else {
			metrics.SettledPositions++
			metrics.RealizedPNL += position.RealizedPNL
			if position.Status == "won" {
				wins++
			}
		}
	}
	metrics.TotalPNL = metrics.RealizedPNL + metrics.UnrealizedPNL
	if account.Mode == AccountModePaper {
		metrics.Cash = settings.InitialBankroll + metrics.RealizedPNL - metrics.OpenExposure
		metrics.Equity = metrics.Cash + marketValue
		metrics.InitialBankroll = settings.InitialBankroll
		if metrics.InitialBankroll > 0 {
			metrics.TotalReturn = metrics.TotalPNL / metrics.InitialBankroll
		}
	} else {
		account = e.ensureLiveBaseline(ctx, account, metrics.OpenExposure, metrics.RealizedPNL)
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
		if e.live != nil && e.live.Configured() {
			snapshot, cashErr := e.live.Collateral(ctx)
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
	if metrics.SettledPositions > 0 {
		metrics.WinRate = float64(wins) / float64(metrics.SettledPositions)
	}
	metrics.TotalOrders, err = e.repo.CountOrders(ctx, account.ID)
	if err != nil {
		return Dashboard{}, err
	}
	points, err := e.repo.ListSnapshots(ctx, account.ID, 500)
	if err != nil {
		return Dashboard{}, err
	}
	if len(points) == 0 {
		points = []EquityPoint{{Time: time.Now().UTC(), Value: metrics.Equity}}
	}
	e.mu.RLock()
	source := SourceStatus{Name: e.provider.Name(), LastSyncAt: e.lastSyncAt}
	e.mu.RUnlock()
	return Dashboard{
		Account: e.decorateAccount(account),
		Metrics: metrics,
		Engine:  e.statusFor(account.Mode),
		Source:  source,
		Equity:  points,
	}, nil
}

func (e *Engine) ensureLiveBaseline(ctx context.Context, account Account, openExposure, realized float64) Account {
	if account.Mode != AccountModeLive || account.BaselineEquity > 0 || e.live == nil || !e.live.Configured() {
		return account
	}
	snapshot, err := e.live.Collateral(ctx)
	if err != nil {
		return account
	}
	// The wallet balance may already reflect open positions from this app, so
	// add the tracked exposure back to recover the starting capital.
	baseline := snapshot.Amount + openExposure - realized
	if baseline <= 0 {
		return account
	}
	if err := e.repo.UpdateAccountBaseline(ctx, account.ID, baseline); err != nil {
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
	return e.repo.AddSnapshot(ctx, account.ID, EquityPoint{Time: time.Now().UTC(), Value: dashboard.Metrics.Equity})
}

func timePointer(value time.Time) *time.Time { return &value }
