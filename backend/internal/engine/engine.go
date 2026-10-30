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
	"pennyharvester/backend/internal/netproxy"
)

type Engine struct {
	repo     Repository
	provider market.Provider
	live     LiveExecutor
	proxy    *netproxy.Manager

	tradeMu       sync.Mutex
	mu            sync.RWMutex
	settings      Settings
	system        SystemSettings
	systemError   string
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

func New(ctx context.Context, repo Repository, provider market.Provider, live LiveExecutor, proxy *netproxy.Manager) (*Engine, error) {
	settings, err := repo.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}
	system, err := repo.LoadSystemSettings(ctx)
	if err != nil {
		return nil, err
	}
	if proxy == nil {
		proxy = netproxy.NewMust(system.Proxy)
	} else if err := proxy.Apply(system.Proxy); err != nil {
		// A row written by an older build must not stop the process from
		// booting; the system settings page will surface the stored value.
		proxy = netproxy.NewMust(netproxy.DefaultConfig())
	}
	// Paper accounts created before the baseline was persisted (or legacy
	// accounts) get the current default once. After that the stored baseline
	// is authoritative and changing the global default does not rewrite
	// historical performance.
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, account := range accounts {
		if account.Mode == AccountModePaper && account.BaselineEquity <= 0 {
			if err := repo.UpdateAccountBaseline(ctx, account.ID, settings.InitialBankroll); err != nil {
				return nil, err
			}
		}
	}
	instance := &Engine{
		repo: repo, provider: provider, live: live, proxy: proxy, settings: settings, system: system,
		opportunities: []Opportunity{}, running: true, done: make(chan struct{}),
	}
	// Apply web-managed runtime settings on top of the process environment. A
	// row written by an older build has empty fields and therefore inherits the
	// startup configuration. An unusable row must not prevent boot; it is
	// surfaced through SystemRuntime instead.
	if err := instance.applyRuntimeSettings(system); err != nil {
		instance.systemError = err.Error()
	}
	return instance, nil
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
	// The version is owned by the server so a client payload can never roll the
	// one-time fee migration back or trigger it again.
	settings.FeeModelVersion = FeeModelVersion
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

func (e *Engine) SystemSettings() SystemSettings {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.system
}

// UpdateSystemSettings persists the runtime egress, endpoint and live-trading
// configuration and applies it without a restart. Values are applied before
// the row is saved so an unusable URL or credential set never lands in the
// database; a failed save rolls the live clients back to the previous state.
func (e *Engine) UpdateSystemSettings(ctx context.Context, settings SystemSettings) error {
	settings = settings.Normalize()
	if err := settings.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	// Serialize with scans and order placement so a live order cannot be
	// signed with one credential set while the client is being rebuilt for
	// another.
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	previous := e.SystemSettings()
	if err := e.applyRuntimeSettings(settings); err != nil {
		// applyRuntimeSettings rolls back internally on a partial failure.
		return fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	if err := e.repo.SaveSystemSettings(ctx, settings); err != nil {
		_ = e.applyRuntimeSettings(previous)
		return err
	}
	e.mu.Lock()
	e.system = settings
	e.systemError = ""
	e.mu.Unlock()
	if reloader, ok := e.live.(interface{ ReloadProxy() }); ok && reloader != nil {
		reloader.ReloadProxy()
	}
	return nil
}

// ProbeLive checks candidate live credentials against the CLOB without saving
// them or placing an order. The browser echoes masked secrets back, so they are
// restored from the stored settings first.
func (e *Engine) ProbeLive(ctx context.Context, settings LiveSettings, clobBaseURL string) (LiveProbe, error) {
	settings = settings.MergeSecret(e.SystemSettings().Live).Normalize()
	if err := settings.Validate(); err != nil {
		return LiveProbe{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	probe, ok := e.live.(liveProbeRuntime)
	if !ok || probe == nil {
		return LiveProbe{}, fmt.Errorf("当前实盘执行器不支持凭据检测")
	}
	return probe.Probe(ctx, settings, clobBaseURL)
}

// ProbeRelayer checks a Relayer API key without submitting anything. Relayer
// keys only authenticate gasless wallet batches; they cannot place orders.
func (e *Engine) ProbeRelayer(ctx context.Context, settings LiveSettings, clobBaseURL, apiKey, address string) (RelayerProbe, error) {
	settings = settings.MergeSecret(e.SystemSettings().Live).Normalize()
	if err := settings.Validate(); err != nil {
		return RelayerProbe{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	probe, ok := e.live.(relayerProbeRuntime)
	if !ok || probe == nil {
		return RelayerProbe{}, fmt.Errorf("当前实盘执行器不支持 Relayer 凭据检测")
	}
	return probe.ProbeRelayer(ctx, settings, clobBaseURL, apiKey, address)
}

// DiscoverFunder returns the proxy / deposit wallets the CLOB associates with
// the configured signer. It works before a funder is set, so the settings page
// can fill the field instead of asking the operator to copy an address.
func (e *Engine) DiscoverFunder(ctx context.Context, settings LiveSettings, clobBaseURL string) ([]string, error) {
	settings = settings.MergeSecret(e.SystemSettings().Live).Normalize()
	discovery, ok := e.live.(funderDiscovery)
	if !ok || discovery == nil {
		return nil, fmt.Errorf("当前实盘执行器不支持自动获取 Funder")
	}
	return discovery.DiscoverFunder(ctx, settings, clobBaseURL)
}

// applyRuntimeSettings applies proxy, data-source and live credentials in
// order. If a later step fails, earlier steps are rolled back to previous.
func (e *Engine) applyRuntimeSettings(settings SystemSettings) error {
	previous := e.SystemSettings()
	if e.proxy != nil {
		if err := e.proxy.Apply(settings.Proxy); err != nil {
			return err
		}
	}
	provider, hasProvider := e.provider.(dataSourceRuntime)
	if hasProvider {
		if err := provider.SetBaseURL(settings.DataSource.GammaBaseURL); err != nil {
			if e.proxy != nil {
				_ = e.proxy.Apply(previous.Proxy)
			}
			return err
		}
	}
	updater, hasLive := e.live.(liveSettingsRuntime)
	if hasLive {
		if err := updater.UpdateSettings(settings.Live, settings.DataSource.CLOBBaseURL); err != nil {
			if hasProvider {
				_ = provider.SetBaseURL(previous.DataSource.GammaBaseURL)
			}
			if e.proxy != nil {
				_ = e.proxy.Apply(previous.Proxy)
			}
			return err
		}
	}
	return nil
}

// SystemRuntime reports the effective endpoints and the live executor state.
func (e *Engine) SystemRuntime() SystemRuntime {
	runtime := SystemRuntime{}
	if provider, ok := e.provider.(dataSourceRuntime); ok && provider != nil {
		runtime.GammaBaseURL = provider.BaseURL()
		runtime.GammaDefault = provider.DefaultBaseURL()
	}
	if live, ok := e.live.(liveSettingsRuntime); ok && live != nil {
		runtime.CLOBBaseURL = live.EffectiveCLOBURL()
		runtime.LiveEnvironment = live.Environment()
		runtime.CLOBDefault = runtime.LiveEnvironment.CLOBBaseURL
	}
	runtime.LiveConfigured, runtime.LiveReady, runtime.LiveError = e.LiveStatus()
	e.mu.RLock()
	runtime.Error = e.systemError
	e.mu.RUnlock()
	return runtime
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
	if e.live == nil {
		account.LiveError = "未配置实盘执行器"
		return account
	}
	account.LiveConfigured = e.live.Configured()
	account.LiveReady = account.LiveConfigured && e.live.ReadyError() == ""
	account.LiveSigner = e.live.Signer()
	account.LiveMaker = e.live.Maker()
	account.LiveError = e.live.ReadyError()
	return account
}

// LiveStatus reports the executor state independently of the currently
// selected default account.
func (e *Engine) LiveStatus() (configured, ready bool, message string) {
	if e.live == nil {
		return false, false, "未配置实盘执行器"
	}
	configured = e.live.Configured()
	message = e.live.ReadyError()
	return configured, configured && message == "", message
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
			if message := e.live.ReadyError(); message != "" {
				return Account{}, fmt.Errorf("%w: 实盘凭据未就绪: %s", ErrLiveUnavailable, message)
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

// Scan refreshes public data and processes every account, including automatic
// orders. It is used by the background engine and tests.
func (e *Engine) Scan(ctx context.Context) error { return e.scan(ctx, true) }

// ScanOnly refreshes public data, marks positions and writes equity snapshots,
// but never places an order. Manual scans use this so a user who only wants to
// refresh the radar cannot accidentally trigger live automatic trading.
func (e *Engine) ScanOnly(ctx context.Context) error { return e.scan(ctx, false) }

func (e *Engine) scan(ctx context.Context, execute bool) error {
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

	// Polymarket exposes Chinese question and outcome labels through the Gamma
	// locale parameter. Fetch them only for the markets that passed the filters
	// so the radar matches the Chinese Polymarket UI without doubling the
	// paginated market scan. A translation failure must not break trading; the
	// original English text remains available as a fallback.
	if localizedProvider, ok := e.provider.(market.LocalizedProvider); ok && len(opportunities) > 0 {
		ids := make([]string, 0, len(opportunities))
		seen := make(map[string]bool, len(opportunities))
		for _, opportunity := range opportunities {
			if !seen[opportunity.MarketID] {
				seen[opportunity.MarketID] = true
				ids = append(ids, opportunity.MarketID)
			}
		}
		if localized, localizedErr := localizedProvider.MarketsLocale(ctx, ids, "zh"); localizedErr == nil {
			byID := make(map[string]market.Market, len(localized))
			for _, item := range localized {
				byID[item.ID] = item
			}
			for i := range opportunities {
				localizedItem, ok := byID[opportunities[i].MarketID]
				if !ok {
					continue
				}
				if localizedItem.Question != "" {
					opportunities[i].QuestionZH = localizedItem.Question
				}
				if localizedItem.EventTitle != "" {
					opportunities[i].EventTitleZH = localizedItem.EventTitle
				}
				if opportunities[i].GroupItemTitle == "" {
					opportunities[i].GroupItemTitle = localizedItem.GroupItemTitle
				}
				if outcome, ok := localizedItem.OutcomeByToken(opportunities[i].TokenID); ok && outcome.Name != "" {
					opportunities[i].OutcomeZH = outcome.Name
				}
			}
		}
	}

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
		unresolved := 0
		if account.Mode == AccountModeLive {
			remaining, err := e.reconcileLiveOrders(ctx, account, byID)
			unresolved = remaining
			if err != nil {
				processingErrors = append(processingErrors, fmt.Sprintf("%s 对账: %v", account.Name, err))
			}
		}
		if err := e.refreshPositions(ctx, account.ID, byID); err != nil {
			processingErrors = append(processingErrors, fmt.Sprintf("%s: %v", account.Name, err))
			// Do not place new orders against positions whose marks or
			// settlement state could not be refreshed.
			unresolved++
		}
		if execute && e.isRunning() && account.AutoTrade {
			switch {
			case unresolved > 0:
				processingErrors = append(processingErrors, fmt.Sprintf("%s: 存在未对账或未刷新的持仓，跳过本轮自动交易", account.Name))
			default:
				if err := e.autoTrade(ctx, account, opportunities, settings); err != nil {
					processingErrors = append(processingErrors, fmt.Sprintf("%s: %v", account.Name, err))
				}
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
			switch {
			case settlementPrice == 1:
				position.Status = "won"
			case settlementPrice == 0.5:
				position.Status = "split"
			default:
				position.Status = "lost"
			}
			position.RealizedPNL = payout - position.Cost
			kind := position.Kind
			if kind == "" {
				kind = "manual"
			}
			order := Order{
				AccountID: accountID, PositionID: &position.ID, MarketID: position.MarketID, Question: position.Question,
				Outcome: position.Outcome, TokenID: position.TokenID, Kind: kind, Type: "settle",
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
	positions, err := e.repo.ListPositions(ctx, account.ID, "all")
	if err != nil {
		return err
	}
	heldTokens := make(map[string]bool, len(positions))
	marketExposure := make(map[string]float64)
	totalExposure, realized := 0.0, 0.0
	for _, position := range positions {
		if position.Status != "open" {
			realized += position.RealizedPNL
			continue
		}
		heldTokens[position.TokenID] = true
		marketExposure[position.MarketID] += position.Cost
		totalExposure += position.Cost
	}
	baseline := account.BaselineEquity
	if baseline <= 0 {
		baseline = settings.InitialBankroll
	}
	cash := baseline + realized - totalExposure
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
		if cost > cash+1e-9 {
			break
		}
		if marketExposure[opportunity.MarketID]+cost > settings.MaxPosition+1e-9 {
			continue
		}
		position, err := e.executePaperBuy(ctx, account, opportunity, "auto", settings.OrderSize)
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
		cash -= position.Cost
	}
	return nil
}

func (e *Engine) autoLiveTrade(ctx context.Context, account Account, opportunities []Opportunity, settings Settings) error {
	if e.live == nil || !e.live.Configured() {
		return fmt.Errorf("%w: 实盘自动交易已开启，但未配置 POLYMARKET_PRIVATE_KEY", ErrLiveUnavailable)
	}
	if message := e.live.ReadyError(); message != "" {
		return fmt.Errorf("%w: 实盘自动交易已开启，但实盘凭据未就绪: %s", ErrLiveUnavailable, message)
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
		if opportunity.ConditionID == "" {
			continue
		}
		budget := settings.OrderSize
		estimatedFee, feeErr := e.live.EstimateBuyFee(ctx, opportunity.ConditionID, budget, opportunity.Price)
		if feeErr != nil {
			return fmt.Errorf("%w: 估算实盘手续费失败: %v", ErrLiveUnavailable, feeErr)
		}
		if estimatedFee < 0 || math.IsNaN(estimatedFee) || math.IsInf(estimatedFee, 0) {
			return fmt.Errorf("%w: 实盘手续费估算无效", ErrLiveUnavailable)
		}
		cost := budget + estimatedFee
		if totalExposure+cost > settings.MaxExposure+1e-9 {
			break
		}
		if marketExposure[opportunity.MarketID]+cost > settings.MaxPosition+1e-9 {
			continue
		}
		if cost > cash+1e-9 {
			return fmt.Errorf("%w: 实盘策略资金不足（含预估手续费）", ErrInvalidOrder)
		}
		if snapshot.Allowance > 0 && cost > snapshot.Allowance+1e-9 {
			break
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
	return e.executePaperBuy(ctx, account, opportunity, "manual", request.Amount)
}

func (e *Engine) executePaperBuy(ctx context.Context, account Account, opportunity Opportunity, kind string, amount float64) (Position, error) {
	settings := e.Settings()
	fee := amount * settings.FeeBPS / 10000
	cost := amount + fee
	fillPrice := opportunity.Price * (1 + settings.SlippageBPS/10000)
	if fillPrice > settings.MaxPrice || fillPrice >= 1 {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "计入滑点后超过最高买入价", "")
	}
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
	baseline := account.BaselineEquity
	if baseline <= 0 {
		baseline = settings.InitialBankroll
	}
	cash := baseline + realized - totalExposure
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
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, reason, "")
	}

	now := time.Now().UTC()
	shares := amount / fillPrice
	position := Position{
		AccountID: account.ID,
		MarketID:  opportunity.MarketID, ConditionID: opportunity.ConditionID,
		Question: opportunity.Question, Outcome: opportunity.Outcome, TokenID: opportunity.TokenID,
		Side: "BUY", Kind: kind, EntryPrice: fillPrice, CurrentPrice: opportunity.Price,
		Shares: shares, Cost: cost, Fee: fee, Status: "open", OpenedAt: now,
		EndDate: opportunity.EndDate, UnrealizedPNL: opportunity.Price*shares - cost,
	}
	position, _, err = e.repo.CreatePositionWithOrder(ctx, position, Order{
		AccountID: account.ID, MarketID: position.MarketID, Question: position.Question,
		Outcome: position.Outcome, TokenID: position.TokenID, Kind: kind, Type: "buy",
		Amount: cost, Price: fillPrice, Shares: shares, Fee: fee, Status: "filled",
		Reason: "模拟成交；未发送任何真实订单", CreatedAt: now,
	})
	if err != nil {
		return Position{}, err
	}
	return position, nil
}

func (e *Engine) executeLiveBuy(ctx context.Context, account Account, opportunity Opportunity, kind string, amount float64) (Position, error) {
	if e.live == nil || !e.live.Configured() {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			"实盘未配置：请设置 POLYMARKET_PRIVATE_KEY 后重启服务", "")
	}
	if message := e.live.ReadyError(); message != "" {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			"实盘未就绪："+message, "")
	}
	if opportunity.ConditionID == "" {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			"缺少 condition_id，无法加载实盘手续费", "")
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
	estimatedFee, feeErr := e.live.EstimateBuyFee(ctx, opportunity.ConditionID, amount, opportunity.Price)
	if feeErr != nil {
		_ = e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "无法估算实盘手续费："+feeErr.Error(), "")
		return Position{}, fmt.Errorf("%w: 无法估算实盘手续费: %v", ErrLiveUnavailable, feeErr)
	}
	if estimatedFee < 0 || math.IsNaN(estimatedFee) || math.IsInf(estimatedFee, 0) {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘手续费估算无效", "")
	}
	totalCost := amount + estimatedFee
	cash := math.Min(strategyCash, snapshot.Amount)
	switch {
	case totalCost > cash+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘策略资金不足（含预估手续费）", "")
	case snapshot.Allowance > 0 && totalCost > snapshot.Allowance+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘 USDC 授权额度不足，请先在 Polymarket 完成授权", "")
	case totalExposure+totalCost > settings.MaxExposure+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "超过总敞口上限（含预估手续费）", "")
	case marketExposure+totalCost > settings.MaxPosition+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "超过单市场敞口上限（含预估手续费）", "")
	}

	var pending Order
	fill, err := e.live.Buy(ctx, opportunity.TokenID, opportunity.ConditionID, amount, settings.MaxPrice, func(orderID string) error {
		created, createErr := e.repo.CreateOrder(ctx, Order{
			AccountID: account.ID, MarketID: opportunity.MarketID, Question: opportunity.Question,
			Outcome: opportunity.Outcome, TokenID: opportunity.TokenID, Kind: kind, Type: "buy",
			Amount: amount, Price: opportunity.Price, Status: "pending",
			Reason: "实盘订单已签名，等待成交回报", ExternalID: orderID, CreatedAt: time.Now().UTC(),
		})
		if createErr == nil {
			pending = created
		}
		return createErr
	})
	if err != nil {
		if pending.ID != 0 {
			status, reason := "rejected", "实盘下单失败："+err.Error()
			explicitRejection := strings.EqualFold(fill.Status, "rejected") || strings.EqualFold(fill.Status, "canceled") ||
				strings.EqualFold(fill.Status, "cancelled") || strings.EqualFold(fill.Status, "expired")
			if strings.EqualFold(fill.Status, "unknown") || (fill.OrderID != "" && !explicitRejection) {
				status, reason = "unknown", "实盘订单结果未知，等待对账："+err.Error()
			}
			_ = e.repo.MarkOrderStatus(ctx, pending.ID, status, reason)
		} else {
			_ = e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘下单失败："+err.Error(), "")
		}
		if errors.Is(err, ErrInvalidOrder) {
			return Position{}, err
		}
		return Position{}, fmt.Errorf("%w: 实盘下单失败: %v", ErrUpstream, err)
	}
	if fill.Shares <= 0 || fill.Cost <= 0 {
		if pending.ID != 0 {
			status := "rejected"
			if strings.EqualFold(fill.Status, "unknown") {
				status = "unknown"
			}
			_ = e.repo.MarkOrderStatus(ctx, pending.ID, status, "实盘订单未产生成交（状态 "+fill.Status+"）")
			return Position{}, fmt.Errorf("%w: 实盘订单未产生成交（状态 %s）", ErrUpstream, fill.Status)
		}
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			"实盘订单未产生成交", fill.OrderID)
	}
	now := time.Now().UTC()
	position := Position{
		AccountID: account.ID,
		MarketID:  opportunity.MarketID, ConditionID: opportunity.ConditionID,
		Question: opportunity.Question, Outcome: opportunity.Outcome, TokenID: opportunity.TokenID,
		Side: "BUY", Kind: kind, EntryPrice: fill.Price, CurrentPrice: opportunity.Price,
		Shares: fill.Shares, Cost: fill.Cost, Fee: fill.Fee, Status: "open", OpenedAt: now,
		EndDate: opportunity.EndDate, UnrealizedPNL: opportunity.Price*fill.Shares - fill.Cost,
	}
	if pending.ID != 0 {
		position, err = e.repo.FinalizeLiveBuy(ctx, pending.ID, position, fill)
		if err != nil {
			return Position{}, fmt.Errorf("实盘成交已确认，但本地账务写入失败: %w", err)
		}
		return position, nil
	}
	// Defensive fallback for an executor that does not call onSigned. The
	// real implementation always persists the pending order first.
	position, _, err = e.repo.CreatePositionWithOrder(ctx, position, Order{
		AccountID: account.ID, MarketID: position.MarketID, Question: position.Question,
		Outcome: position.Outcome, TokenID: position.TokenID, Kind: kind, Type: "buy",
		Amount: fill.Cost, Price: fill.Price, Shares: fill.Shares, Fee: fill.Fee, Status: "filled",
		Reason: "Polymarket CLOB 实盘成交", ExternalID: fill.OrderID, CreatedAt: now,
	})
	if err != nil {
		return Position{}, fmt.Errorf("实盘成交已确认，但本地账务写入失败: %w", err)
	}
	return position, nil
}

// reconcileLiveOrders resolves signed orders whose submit response was lost or
// whose CLOB status was still unknown. It returns the number of orders that
// remain unresolved; callers must not auto-trade that account while it is > 0.
func (e *Engine) reconcileLiveOrders(ctx context.Context, account Account, markets map[string]market.Market) (int, error) {
	orders, err := e.repo.ListUnresolvedOrders(ctx, account.ID)
	if err != nil {
		return 0, err
	}
	if len(orders) == 0 {
		return 0, nil
	}
	if e.live == nil || !e.live.Configured() {
		return len(orders), fmt.Errorf("实盘未配置，无法对账 %d 笔待确认订单", len(orders))
	}
	if message := e.live.ReadyError(); message != "" {
		return len(orders), fmt.Errorf("实盘未就绪，无法对账: %s", message)
	}
	var errs []string
	remaining := 0
	for _, order := range orders {
		if order.ExternalID == "" {
			remaining++
			continue
		}
		item, ok := markets[order.MarketID]
		if !ok {
			item, err = e.provider.Get(ctx, order.MarketID)
			if err != nil {
				errs = append(errs, fmt.Sprintf("订单 %s 读取市场失败: %v", order.ExternalID, err))
				remaining++
				continue
			}
		}
		fill, found, lookupErr := e.live.LookupOrder(ctx, order.ExternalID, item.ConditionID)
		if lookupErr != nil {
			errs = append(errs, fmt.Sprintf("订单 %s 对账失败: %v", order.ExternalID, lookupErr))
			remaining++
			continue
		}
		if !found {
			if time.Since(order.CreatedAt) >= 2*time.Minute {
				if markErr := e.repo.MarkOrderStatus(ctx, order.ID, "rejected", "CLOB 未找到该订单，按未成交处理"); markErr != nil {
					errs = append(errs, markErr.Error())
					remaining++
				}
			} else {
				remaining++
			}
			continue
		}
		if fill.Shares > 0 && fill.Cost > 0 {
			outcome, outcomeOK := item.OutcomeByToken(order.TokenID)
			if !outcomeOK {
				errs = append(errs, fmt.Sprintf("订单 %s 的 token 已不在市场中", order.ExternalID))
				remaining++
				continue
			}
			openedAt := order.CreatedAt
			if openedAt.IsZero() {
				openedAt = time.Now().UTC()
			}
			kind := order.Kind
			if kind == "" {
				kind = "manual"
			}
			position := Position{
				AccountID: account.ID, MarketID: order.MarketID, ConditionID: item.ConditionID,
				Question: order.Question, Outcome: order.Outcome, TokenID: order.TokenID,
				Side: "BUY", Kind: kind, EntryPrice: fill.Price, CurrentPrice: outcome.Price,
				Shares: fill.Shares, Cost: fill.Cost, Fee: fill.Fee, Status: "open", OpenedAt: openedAt,
				EndDate: item.EndDate, UnrealizedPNL: outcome.Price*fill.Shares - fill.Cost,
			}
			if _, finalizeErr := e.repo.FinalizeLiveBuy(ctx, order.ID, position, fill); finalizeErr != nil {
				errs = append(errs, fmt.Sprintf("订单 %s 对账写入失败: %v", order.ExternalID, finalizeErr))
				remaining++
			}
			continue
		}
		switch strings.ToLower(strings.TrimSpace(fill.Status)) {
		case "canceled", "cancelled", "rejected", "expired":
			if markErr := e.repo.MarkOrderStatus(ctx, order.ID, "rejected", "实盘订单未成交："+fill.Status); markErr != nil {
				errs = append(errs, markErr.Error())
				remaining++
			}
		default:
			remaining++
		}
	}
	if len(errs) > 0 {
		return remaining, errors.New(strings.Join(errs, "; "))
	}
	return remaining, nil
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

func (e *Engine) Orders(ctx context.Context, accountID int64, status string, limit, offset int) (OrderList, error) {
	items, err := e.repo.ListOrders(ctx, accountID, status, limit, offset)
	if err != nil {
		return OrderList{}, err
	}
	total, err := e.repo.CountOrders(ctx, accountID, status)
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
	positions, err := e.repo.ListPositions(ctx, account.ID, "all")
	if err != nil {
		return Dashboard{}, err
	}
	settings := e.Settings()
	metrics := Metrics{Mode: string(account.Mode), InitialBankroll: settings.InitialBankroll}
	wins, marketValue := 0, 0.0
	for _, position := range positions {
		metrics.FeesPaid += position.Fee
		if position.Status == "open" {
			metrics.OpenPositions++
			metrics.OpenExposure += position.Cost
			metrics.UnrealizedPNL += position.UnrealizedPNL
			marketValue += position.CurrentPrice * position.Shares
		} else {
			metrics.SettledPositions++
			metrics.RealizedPNL += position.RealizedPNL
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
	decided := metrics.SettledPositions - metrics.SplitPositions
	if decided > 0 {
		metrics.WinRate = float64(wins) / float64(decided)
	}
	metrics.TotalOrders, err = e.repo.CountOrders(ctx, account.ID, "all")
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
