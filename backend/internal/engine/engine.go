package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
	"pennyharvester/backend/internal/venue"
)

const (
	localizationTimeout    = 2 * time.Second
	positionRefreshTimeout = 3 * time.Second
	// liveBalanceTimeout bounds the wallet read a dashboard request makes against
	// the venue. The dashboard is polled by the web UI, so an unreachable venue
	// must report stale cash instead of holding the request open until the HTTP
	// client gives up.
	liveBalanceTimeout = 5 * time.Second
)

type Engine struct {
	repo   Repository
	venues *venue.Registry
	proxy  *netproxy.Manager

	tradeMu       sync.Mutex
	mu            sync.RWMutex
	settings      Settings
	system        SystemSettings
	systemError   string
	opportunities []Opportunity
	// opportunitiesByAccount holds each account's own radar view, filtered by
	// that account's strategy. The union above keeps legacy callers and token
	// lookups working across accounts.
	opportunitiesByAccount map[int64][]Opportunity
	// scannedVenues records which venues contributed market data to the last
	// scan, including venues that returned markets but no qualifying rows.
	scannedVenues []market.Venue
	lastScanAt    *time.Time
	lastSyncAt    *time.Time
	lastError     string
	scanCount     int64
	running       bool

	startOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
	// scanReset wakes the scheduler after a settings change so a shorter
	// interval starts now instead of after the old, longer sleep.
	scanReset chan struct{}
}

func New(ctx context.Context, repo Repository, venues *venue.Registry, proxy *netproxy.Manager) (*Engine, error) {
	settings, err := repo.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}
	system, err := repo.LoadSystemSettings(ctx)
	if err != nil {
		return nil, err
	}
	// A fresh database returns the built-in defaults without normalizing them;
	// normalize once here so venues.polymarket always has a readable view.
	system = system.Normalize()
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
	if venues == nil {
		venues = venue.NewRegistry()
	}
	instance := &Engine{
		repo: repo, venues: venues, proxy: proxy, settings: settings, system: system,
		opportunities: []Opportunity{}, opportunitiesByAccount: map[int64][]Opportunity{},
		running: true, done: make(chan struct{}), scanReset: make(chan struct{}, 1),
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

// Venues lists the registered venues in presentation order.
func (e *Engine) Venues() []market.Venue {
	if e.venues == nil {
		return nil
	}
	return e.venues.Venues()
}

// VenueCapabilities returns every registered venue's fact sheet for the API.
func (e *Engine) VenueCapabilities() []venue.Capabilities {
	if e.venues == nil {
		return []venue.Capabilities{}
	}
	return e.venues.All()
}

// data returns a venue's market data source. A nil result means the venue is
// not registered, which callers must treat as unavailable rather than falling
// back to another venue.
func (e *Engine) data(v market.Venue) venue.MarketData {
	if e.venues == nil {
		return nil
	}
	return e.venues.Data(effectiveVenue(v))
}

// executor resolves a venue's trading surface. The error distinguishes a
// read-only venue from one that is merely unconfigured.
func (e *Engine) executor(v market.Venue) (venue.Executor, error) {
	if e.venues == nil {
		return nil, fmt.Errorf("%w: %s", ErrVenueReadOnly, effectiveVenue(v).Label())
	}
	exec, err := e.venues.ExecutorFor(effectiveVenue(v))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVenueReadOnly, err)
	}
	return exec, nil
}

func (e *Engine) capabilities(v market.Venue) (venue.Capabilities, bool) {
	if e.venues == nil {
		return venue.Capabilities{}, false
	}
	return e.venues.Capabilities(effectiveVenue(v))
}

// supportsSettlement reports whether the engine may settle this venue's
// positions from reference data. Polymarket resolves through its own API;
// Predict.fun publishes the winning outcome, which the provider maps onto a 1/0
// payout. A venue without such a feed keeps its positions open instead of being
// guessed closed from another exchange's prices.
func (e *Engine) supportsSettlement(v market.Venue) bool {
	caps, ok := e.capabilities(v)
	if !ok {
		return effectiveVenue(v) == market.VenuePolymarket
	}
	return caps.SupportsSettlement
}

// sourceName is the aggregate label the radar and dashboard render. With one
// registered venue it is exactly that provider's name, which keeps the
// Polymarket label byte-identical.
func (e *Engine) sourceName() string {
	names := make([]string, 0, len(e.Venues()))
	for _, v := range e.Venues() {
		if data := e.data(v); data != nil {
			names = append(names, data.Name())
		}
	}
	return strings.Join(names, " + ")
}

// marketKey namespaces an identifier by venue. Token and market ids are only
// unique inside one venue: both exchanges use decimal ERC-1155 ids, so a bare
// string key would let a Predict.fun row mask a Polymarket row.
func marketKey(v market.Venue, id string) string {
	return string(effectiveVenue(v)) + "\x00" + id
}

// eventKey groups the markets that settle together. The venue prefix is
// required because an event id is only meaningful inside its own venue. An
// empty key means the market carries no event, and such rows are capped by
// market alone.
func eventKey(v market.Venue, eventID string) string {
	if eventID == "" {
		return ""
	}
	return string(effectiveVenue(v)) + "\x00" + eventID
}

// venueList returns the venues a scan should visit for the given options.
func (e *Engine) venueList(settings Settings, only market.Venue) []market.Venue {
	if only != "" && !only.Valid() {
		return nil
	}
	venues := make([]market.Venue, 0, len(e.Venues()))
	for _, v := range e.Venues() {
		if only != "" && v != only {
			continue
		}
		if !settings.For(v).Enabled {
			continue
		}
		venues = append(venues, v)
	}
	return venues
}

// venueScanContext gives one venue its own slice of the scan deadline so a
// slow upstream cannot consume the budget of another venue.
func venueScanContext(ctx context.Context, venues int) (context.Context, context.CancelFunc) {
	const budget = 90 * time.Second
	window := budget
	if venues > 1 {
		window = budget / time.Duration(venues)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < window {
			window = remaining
		}
	}
	if window <= 0 {
		window = time.Second
	}
	return context.WithTimeout(ctx, window)
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
		started := time.Now()
		settings := e.Settings()
		interval := time.Duration(settings.ScanIntervalSeconds) * time.Second
		if e.isRunning() {
			scanCtx, cancel := context.WithTimeout(ctx, scanBudget(interval))
			_ = e.Scan(scanCtx)
			cancel()
		}
		elapsed := time.Since(started)
		wait := waitForNextScan(interval, elapsed)
		if elapsed > interval {
			log.Printf("扫描耗时 %s，超过 %s 的扫描间隔；下一轮在 %s 后开始", elapsed.Round(time.Millisecond), interval, wait.Round(time.Millisecond))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-e.scanReset:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

// scanBudget leaves one second of the interval for scheduling and the next
// timer, so a stalled upstream cannot push the following scan past the
// configured cadence. Long intervals keep the previous 90s ceiling; manual
// scans do not use this path and keep their longer request context.
func scanBudget(interval time.Duration) time.Duration {
	const maxScanBudget = 90 * time.Second
	if interval <= 0 {
		return maxScanBudget
	}
	budget := interval - time.Second
	if budget < time.Second {
		budget = time.Second
	}
	if budget > maxScanBudget {
		budget = maxScanBudget
	}
	return budget
}

// waitForNextScan returns the pause after a scan so the next start lands on the
// configured cadence. If a scan overruns the interval, waiting a full interval
// would halve the effective rate; waiting nothing would turn a slow upstream
// into a busy retry. One second is the floor.
func waitForNextScan(interval, elapsed time.Duration) time.Duration {
	if interval <= 0 {
		return time.Second
	}
	wait := interval - elapsed
	if wait < time.Second {
		return time.Second
	}
	return wait
}

func (e *Engine) Settings() Settings {
	e.mu.RLock()
	defer e.mu.RUnlock()
	settings := e.settings
	// The venue map is shared state: a caller that mutates what it loaded would
	// change the engine's configuration without saving it.
	settings.Venues = cloneVenueSettings(settings.Venues)
	return settings
}

// cloneVenueSettings copies the per-venue strategy blocks so a returned snapshot
// is safe to edit and save back.
func cloneVenueSettings(in map[market.Venue]VenueSettings) map[market.Venue]VenueSettings {
	if in == nil {
		return nil
	}
	out := make(map[market.Venue]VenueSettings, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// normalizeSettingsInput fills the server-owned version and the fields older
// API clients omit, so a partial payload is not rejected outright.
func normalizeSettingsInput(settings Settings) Settings {
	// The version is owned by the server so a client payload can never roll the
	// one-time fee migration back or trigger it again.
	settings.FeeModelVersion = FeeModelVersion
	defaults := DefaultSettings()
	if settings.DefaultCategory == "" {
		settings.DefaultCategory = defaults.DefaultCategory
	}
	if settings.SportsPolicy == "" {
		settings.SportsPolicy = defaults.SportsPolicy
	}
	if settings.StopLossRatio == 0 {
		settings.StopLossRatio = defaults.StopLossRatio
	}
	return settings
}

// mergeVenueSettings keeps per-venue overrides a payload does not mention. An
// old client that only sends the flat fields must not erase the venue map.
func mergeVenueSettings(previous, settings Settings) Settings {
	if settings.Venues == nil {
		settings.Venues = previous.Venues
		return settings
	}
	if len(previous.Venues) == 0 {
		return settings
	}
	merged := make(map[market.Venue]VenueSettings, len(previous.Venues)+len(settings.Venues))
	for v, entry := range previous.Venues {
		merged[v] = entry
	}
	for v, entry := range settings.Venues {
		merged[v] = entry
	}
	settings.Venues = merged
	return settings
}

func (e *Engine) UpdateSettings(ctx context.Context, settings Settings) error {
	settings = normalizeSettingsInput(settings)
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	settings = mergeVenueSettings(e.Settings(), settings)
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := e.validateStopLossExit(settings); err != nil {
		return err
	}
	if err := e.repo.SaveSettings(ctx, settings); err != nil {
		return err
	}
	e.mu.Lock()
	e.settings = settings
	e.mu.Unlock()
	// A shorter cadence should apply before the old sleep expires.
	select {
	case e.scanReset <- struct{}{}:
	default:
	}
	return nil
}

// SettingsFor returns the strategy block that applies to one account. A
// non-positive account ID resolves to the global defaults, which is also the
// template a new account inherits until it saves its own strategy.
func (e *Engine) SettingsFor(ctx context.Context, accountID int64) (Settings, error) {
	return e.settingsForAccount(ctx, accountID)
}

func (e *Engine) settingsForAccount(ctx context.Context, accountID int64) (Settings, error) {
	if accountID <= 0 {
		return e.Settings(), nil
	}
	settings, err := e.repo.LoadAccountSettings(ctx, accountID)
	if err != nil {
		return Settings{}, err
	}
	// Scanning is a process-wide scheduler, not account state. Accounts may
	// still carry a scan interval from older builds, but the value the engine
	// actually uses is the global one. Returning the effective value keeps the
	// settings page from showing a number that has no effect.
	settings.ScanIntervalSeconds = e.Settings().ScanIntervalSeconds
	return settings, nil
}

// UpdateAccountSettings validates and persists one account's strategy block.
// The global settings stay untouched: saving account A must never change what
// account B trades.
func (e *Engine) UpdateAccountSettings(ctx context.Context, accountID int64, settings Settings) error {
	if accountID <= 0 {
		return fmt.Errorf("%w: account_id 不能为空", ErrInvalidOrder)
	}
	if _, err := e.repo.AccountByID(ctx, accountID); err != nil {
		return err
	}
	settings = normalizeSettingsInput(settings)
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	previous, err := e.settingsForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	settings = mergeVenueSettings(previous, settings)
	// The scan interval is global. Keep the account row consistent with the
	// scheduler so a later read cannot surface a stale per-account value.
	settings.ScanIntervalSeconds = previous.ScanIntervalSeconds
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := e.validateStopLossExit(settings); err != nil {
		return err
	}
	return e.repo.SaveAccountSettings(ctx, accountID, settings)
}

// validateStopLossExit refuses to enable the stop-loss when this build could not
// exit what the settings would buy. Every venue floors a sell by shares while a
// buy is sized in money, and one share always costs less than one currency unit
// (prices are strictly below 1), so a single order below the venue's minimum
// share count always lands under that floor. Such a position is not lost: it
// simply cannot be sold before settlement, which is the opposite of what a
// stop-loss promises. The rule lives here rather than in Settings.Validate
// because persisted settings are validated on load, and an installation that
// already runs the stop-loss on small orders must keep loading.
func (e *Engine) validateStopLossExit(settings Settings) error {
	if !settings.StopLossEnabled {
		return nil
	}
	for _, v := range market.VenueOrder {
		effective := settings.For(v)
		if !effective.Enabled {
			continue
		}
		caps, ok := e.capabilities(v)
		if !ok || caps.MinSellShares <= 0 {
			continue
		}
		if effective.OrderSize+1e-9 >= caps.MinSellShares {
			continue
		}
		return fmt.Errorf("%w: 开启自动止损需要 %s 的单笔金额不低于 %.0f %s（该场所卖单最小 %.0f 份，单笔金额低于这个数就买不到可卖出的份额，仓位只能持有到结算）",
			ErrInvalidSettings, caps.Label, caps.MinSellShares, caps.Currency, caps.MinSellShares)
	}
	return nil
}

func (e *Engine) SystemSettings() SystemSettings {
	e.mu.RLock()
	defer e.mu.RUnlock()
	system := e.system
	system.Venues = cloneVenueSystemSettings(system.Venues)
	return system
}

// cloneVenueSystemSettings copies the per-venue credential blocks so a returned
// snapshot cannot be edited in place behind the engine's back.
func cloneVenueSystemSettings(in map[market.Venue]VenueSystemSettings) map[market.Venue]VenueSystemSettings {
	if in == nil {
		return nil
	}
	out := make(map[market.Venue]VenueSystemSettings, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
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
	for _, v := range e.Venues() {
		if reloader, ok := e.venues.Executor(v).(interface{ ReloadProxy() }); ok && reloader != nil {
			reloader.ReloadProxy()
		}
	}
	return nil
}

// ProbeLive checks candidate live credentials against the venue's trading API
// without saving them or placing an order. The browser echoes masked secrets
// back, so they are restored from the stored settings first. Only the
// Polymarket adapter implements the probe interface today.
func (e *Engine) ProbeLive(ctx context.Context, v market.Venue, settings LiveSettings, clobBaseURL string) (LiveProbe, error) {
	return e.ProbeLiveVenue(ctx, v, VenueLiveSettings{LiveSettings: settings}, clobBaseURL)
}

// VenueApproval reports the on-chain allowances a venue's live trading needs. A
// venue that trades through a proxy, or one that is registered read-only,
// reports that no approval is required instead of failing.
func (e *Engine) VenueApproval(ctx context.Context, v market.Venue) (venue.Approval, error) {
	approver, label, err := e.approver(v)
	if err != nil {
		return venue.Approval{}, err
	}
	approval, err := approver.ApprovalState(ctx)
	if err != nil {
		return venue.Approval{}, fmt.Errorf("%w: %s 授权状态读取失败: %v", ErrUpstream, label, err)
	}
	return approval, nil
}

// ApproveVenue grants the missing on-chain allowances and returns the new state.
// It holds the trade mutex: an approval transaction must not interleave with an
// order that is being signed against the old allowance.
func (e *Engine) ApproveVenue(ctx context.Context, v market.Venue) (venue.Approval, error) {
	approver, label, err := e.approver(v)
	if err != nil {
		return venue.Approval{}, err
	}
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	approval, err := approver.GrantApprovals(ctx)
	if err != nil {
		return venue.Approval{}, fmt.Errorf("%w: %s 链上授权失败: %v", ErrUpstream, label, err)
	}
	return approval, nil
}

// RedeemPosition burns a settled position's shares back into collateral. The
// engine records the attempt before the transaction is sent, so a crash between
// signing and the receipt still leaves an auditable order row, and it refuses a
// second redemption of the same position: the transaction costs gas and a
// duplicate would revert on chain after being paid for.
func (e *Engine) RedeemPosition(ctx context.Context, accountID, positionID int64) (venue.RedeemResult, error) {
	account, err := e.repo.AccountByID(ctx, accountID)
	if err != nil {
		return venue.RedeemResult{}, err
	}
	// A simulated position exists only in this database; there are no on-chain
	// shares to burn, so a redemption would cost gas to burn nothing.
	if account.Mode != AccountModeLive {
		return venue.RedeemResult{}, fmt.Errorf("%w: 模拟账户的仓位没有链上份额，无需赎回", ErrInvalidOrder)
	}
	positions, err := e.repo.ListPositions(ctx, accountID, "", "")
	if err != nil {
		return venue.RedeemResult{}, err
	}
	var position Position
	found := false
	for _, candidate := range positions {
		if candidate.ID == positionID {
			position, found = candidate, true
			break
		}
	}
	if !found {
		return venue.RedeemResult{}, fmt.Errorf("%w: 仓位 %d 不存在", ErrNotFound, positionID)
	}
	switch position.Status {
	case "won", "lost", "split":
	default:
		return venue.RedeemResult{}, fmt.Errorf("%w: 只有已结算的仓位可以赎回（当前状态 %s）", ErrInvalidOrder, position.Status)
	}
	v := position.EffectiveVenue()
	label := v.Label()
	caps, ok := e.capabilities(v)
	if !ok || !caps.SupportsRedeem {
		return venue.RedeemResult{}, fmt.Errorf("%w: %s 不支持链上赎回", ErrVenueReadOnly, label)
	}
	exec, err := e.executor(v)
	if err != nil {
		return venue.RedeemResult{}, fmt.Errorf("%w: %s 只读运行，无法赎回", ErrVenueReadOnly, label)
	}
	redeemer, ok := exec.(venue.Redeemer)
	if !ok || redeemer == nil {
		return venue.RedeemResult{}, fmt.Errorf("%w: %s 不支持链上赎回", ErrVenueReadOnly, label)
	}
	if !exec.Configured() {
		return venue.RedeemResult{}, fmt.Errorf("%w: %s 未配置实盘凭据，无法赎回", ErrVenueNotConfigured, label)
	}
	// A redemption already on its way must not be re-sent: the second
	// transaction would burn nothing and still cost gas. This scans the account's
	// recent order book for the venue; the chain stays the real authority, since
	// redeeming burned shares is a no-op rather than a double payment.
	orders, err := e.repo.ListOrders(ctx, accountID, "", 500, 0, v)
	if err != nil {
		return venue.RedeemResult{}, err
	}
	for _, order := range orders {
		// The orders table checks type in ('buy','settle'); a redemption is
		// bookkeeping on a settled position, so it is stored as a settle action
		// named "redeem" rather than as a new row type that would force a table
		// rebuild on every existing database.
		if order.Type != "settle" || order.Action != "redeem" || order.PositionID == nil || *order.PositionID != position.ID {
			continue
		}
		if order.Status == "pending" || order.Status == "filled" {
			return venue.RedeemResult{}, fmt.Errorf("%w: 该仓位已经提交过赎回（工单 %d）", ErrInvalidOrder, order.ID)
		}
	}
	intent, err := e.repo.CreateOrder(ctx, Order{
		AccountID: accountID, Venue: v, Currency: position.EffectiveCurrency(),
		PositionID: &position.ID, MarketID: position.MarketID,
		Question: position.Question, Outcome: position.Outcome, TokenID: position.TokenID,
		Kind: position.Kind, Type: "settle", Action: "redeem",
		Price: position.CurrentPrice, Shares: position.Shares, Amount: position.Shares,
		Status: "pending", Reason: "链上赎回已提交，等待交易回执",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return venue.RedeemResult{}, err
	}
	// Approving and ordering both mutate chain state for the same wallet, so a
	// redemption takes the same lock as an order.
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	result, err := redeemer.Redeem(ctx, venue.RedeemRequest{
		ConditionID: position.ConditionID,
		TokenID:     position.TokenID,
		Shares:      position.Shares,
	})
	if err != nil {
		_ = e.repo.MarkOrderStatus(ctx, intent.ID, "rejected", clampReason(err.Error()))
		return venue.RedeemResult{}, fmt.Errorf("%w: %s 赎回失败: %v", ErrUpstream, label, err)
	}
	_ = e.repo.MarkOrderStatus(ctx, intent.ID, "filled", "链上赎回交易已提交 "+result.TxHash)
	return result, nil
}

// clampReason bounds an error string before it becomes an order reason. The
// order table renders reasons inline, and an RPC dump would make the row
// unreadable; the full error is returned to the caller either way.
func clampReason(reason string) string {
	const limit = 300
	if len(reason) <= limit {
		return reason
	}
	return reason[:limit] + "…"
}

// approver resolves the venue's approval capability, explaining the two cases in
// which there is nothing to approve.
func (e *Engine) approver(v market.Venue) (venue.Approver, string, error) {
	label := effectiveVenue(v).Label()
	exec, err := e.executor(v)
	if err != nil {
		return nil, label, fmt.Errorf("%w: %s 只读运行，无法执行链上授权", ErrVenueReadOnly, label)
	}
	approver, ok := exec.(venue.Approver)
	if !ok || approver == nil {
		return nil, label, fmt.Errorf("%w: %s 通过托管账户交易", ErrNoApproval, label)
	}
	// Reading allowances needs a wallet to read them for, so an unconfigured
	// venue answers with the configuration error instead of a chain-timeout.
	if !exec.Configured() {
		return nil, label, fmt.Errorf("%w: %s 未配置实盘凭据，无法读取链上授权", ErrInvalidSettings, label)
	}
	return approver, label, nil
}

// ProbeLiveVenue is the venue-aware credential check used by the settings page.
// Polymarket keeps its dedicated /auth probe; Predict.fun has no such endpoint,
// so its credentials are validated locally and a collateral read through the
// BNB chain proves the key and the RPC endpoint agree.
func (e *Engine) ProbeLiveVenue(ctx context.Context, v market.Venue, settings VenueLiveSettings, clobBaseURL string) (LiveProbe, error) {
	v = effectiveVenue(v)
	if v == market.VenuePredictFun {
		return e.probePredictFunLive(ctx, v, settings)
	}
	flat := settings.LiveSettings.MergeSecret(e.SystemSettings().Live).Normalize()
	if err := flat.Validate(); err != nil {
		return LiveProbe{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	exec, err := e.executor(v)
	if err != nil {
		return LiveProbe{}, fmt.Errorf("%w", ErrVenueReadOnly)
	}
	probe, ok := exec.(liveProbeRuntime)
	if !ok || probe == nil {
		return LiveProbe{}, fmt.Errorf("%w: %s 暂不支持凭据检测", ErrInvalidSettings, v.Label())
	}
	return probe.Probe(ctx, flat, clobBaseURL)
}

// ProbeRelayer checks a Relayer API key without submitting anything. Relayer
// keys only authenticate gasless wallet batches; they cannot place orders.
func (e *Engine) ProbeRelayer(ctx context.Context, v market.Venue, settings LiveSettings, clobBaseURL, apiKey, address string) (RelayerProbe, error) {
	v = effectiveVenue(v)
	settings = settings.MergeSecret(e.SystemSettings().Live).Normalize()
	if err := settings.Validate(); err != nil {
		return RelayerProbe{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	exec, err := e.executor(v)
	if err != nil {
		return RelayerProbe{}, fmt.Errorf("%w", ErrVenueReadOnly)
	}
	probe, ok := exec.(relayerProbeRuntime)
	if !ok || probe == nil {
		return RelayerProbe{}, fmt.Errorf("%w: %s 暂不支持 Relayer 凭据检测", ErrInvalidSettings, v.Label())
	}
	return probe.ProbeRelayer(ctx, settings, clobBaseURL, apiKey, address)
}

// DiscoverWallet resolves the Polymarket account wallet (the funder) of the
// configured signer and detects its wallet type (EOA / Proxy / Safe / Deposit).
// It works before a funder is set, so the settings page can fill both fields
// instead of asking the operator to copy an address and guess a type.
func (e *Engine) DiscoverWallet(ctx context.Context, v market.Venue, settings LiveSettings, endpoints WalletEndpoints) (WalletDiscovery, error) {
	v = effectiveVenue(v)
	if v != market.VenuePolymarket {
		return WalletDiscovery{}, fmt.Errorf("%w: %s 不支持钱包自动检测", ErrInvalidSettings, v.Label())
	}
	settings = settings.MergeSecret(e.SystemSettings().Live).Normalize()
	runtime := e.SystemRuntime()
	if strings.TrimSpace(endpoints.Gamma) == "" {
		endpoints.Gamma = runtime.GammaBaseURL
	}
	exec, err := e.executor(v)
	if err != nil {
		return WalletDiscovery{}, fmt.Errorf("%w", ErrVenueReadOnly)
	}
	discovery, ok := exec.(walletDiscovery)
	if !ok || discovery == nil {
		return WalletDiscovery{}, fmt.Errorf("当前实盘执行器不支持钱包自动检测")
	}
	return discovery.DiscoverWallet(ctx, settings, endpoints)
}

// applyRuntimeSettings applies proxy, per-venue data-source endpoints and the
// Polymarket live credentials in order. If a later step fails, earlier steps
// are rolled back so an unusable value never becomes effective.
func (e *Engine) applyRuntimeSettings(settings SystemSettings) error {
	settings = settings.Normalize()
	previous := e.SystemSettings()
	if e.proxy != nil {
		if err := e.proxy.Apply(settings.Proxy); err != nil {
			return err
		}
	}
	type restore struct {
		provider dataSourceRuntime
		baseURL  string
	}
	var restored []restore
	rollback := func() {
		for i := len(restored) - 1; i >= 0; i-- {
			_ = restored[i].provider.SetBaseURL(restored[i].baseURL)
		}
		if e.proxy != nil {
			_ = e.proxy.Apply(previous.Proxy)
		}
	}
	for _, v := range e.Venues() {
		provider, ok := e.data(v).(dataSourceRuntime)
		if !ok || provider == nil {
			continue
		}
		restored = append(restored, restore{provider: provider, baseURL: provider.BaseURL()})
		if err := provider.SetBaseURL(venueBaseURL(settings, v)); err != nil {
			rollback()
			return err
		}
	}
	if updater, ok := e.venues.Executor(market.VenuePolymarket).(liveSettingsRuntime); ok && updater != nil {
		pm := settings.Venues[market.VenuePolymarket]
		if err := updater.UpdateSettings(pm.Live.LiveSettings, pm.DataSource.CLOBBaseURL); err != nil {
			rollback()
			return err
		}
	}
	// Predict.fun shares one configuration between its data client and its
	// executor, so the executor applies the stored credentials directly. They
	// are validated before the swap, which keeps the previous ones effective
	// when a stored row is unusable. An absent venue block means "inherit the
	// process environment", so the constructor's credentials are left alone.
	if pf, ok := settings.Venues[market.VenuePredictFun]; ok {
		if updater, ok := e.venues.Executor(market.VenuePredictFun).(venueCredentialRuntime); ok && updater != nil {
			err := updater.ApplyLiveSettings(pf.Live.APIKey, pf.Live.PrivateKey, pf.Live.SmartWallet, pf.Live.Testnet, pf.DataSource.BaseURL)
			if err != nil {
				rollback()
				return err
			}
		}
	}
	return nil
}

// venueCredentialRuntime lets the engine hand a venue its stored credentials
// without a restart. Predict.fun implements it on the data client and on the
// executor, which share one atomic configuration.
type venueCredentialRuntime interface {
	ApplyLiveSettings(apiKey, privateKey, smartWallet string, testnet *bool, baseURL string) error
}

// venueCredentialResetter restores the startup configuration. Credential
// application treats empty strings as "keep", so it cannot express "go back to
// the process wallet"; a reset can, and a test run must never leave the
// executor holding candidate credentials.
type venueCredentialResetter interface {
	ResetLiveSettings() error
}

// probePredictFunLive checks candidate credentials without saving them. The
// candidate is applied, the collateral balance is read through the chain
// endpoint, and the stored credentials are restored afterwards so a test never
// leaves the executor half-configured.
func (e *Engine) probePredictFunLive(ctx context.Context, v market.Venue, candidate VenueLiveSettings) (LiveProbe, error) {
	// The probe swaps credentials on the executor that live orders also use, so
	// it runs under the same mutex: a scan must not sign an order with the
	// candidate key, and a half-applied probe must not race a settings save.
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	exec, err := e.executor(v)
	if err != nil {
		return LiveProbe{}, fmt.Errorf("%w", ErrVenueReadOnly)
	}
	runtime, ok := exec.(venueCredentialRuntime)
	if !ok || runtime == nil {
		return LiveProbe{}, fmt.Errorf("%w: %s 暂不支持凭据检测", ErrInvalidSettings, v.Label())
	}
	stored := e.SystemSettings().Venues[v].Live
	value := func(candidate, stored string) string {
		if candidate == "" || candidate == netproxy.RedactedSecret {
			return stored
		}
		return candidate
	}
	apiKey := value(strings.TrimSpace(candidate.APIKey), stored.APIKey)
	privateKey := value(candidate.PrivateKey, stored.PrivateKey)
	smartWallet := value(strings.TrimSpace(candidate.SmartWallet), stored.SmartWallet)
	testnet := candidate.Testnet
	if err := runtime.ApplyLiveSettings(apiKey, privateKey, smartWallet, testnet, ""); err != nil {
		return LiveProbe{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	defer e.restoreLiveCredentials(v, exec, runtime, stored)
	if message := exec.ReadyError(); message != "" {
		return LiveProbe{}, fmt.Errorf("%w: %s", ErrInvalidSettings, message)
	}
	start := time.Now()
	snapshot, err := exec.Collateral(ctx)
	if err != nil {
		return LiveProbe{}, err
	}
	return LiveProbe{
		Signer:    exec.Signer(),
		Maker:     exec.Maker(),
		APIKey:    maskCredential(apiKey),
		Source:    "chain",
		Mode:      "live",
		Cash:      snapshot.Amount,
		Allowance: snapshot.Allowance,
		LatencyMS: float64(time.Since(start).Milliseconds()),
	}, nil
}

// restoreLiveCredentials puts the executor back on its startup configuration and
// then re-applies what the operator saved. A reset alone would drop persisted
// credentials; re-applying alone would keep the candidate's wallet, because an
// empty string means "keep" rather than "inherit".
func (e *Engine) restoreLiveCredentials(v market.Venue, exec venue.Executor, runtime venueCredentialRuntime, stored VenueLiveSettings) {
	var restoreErr error
	if resetter, ok := exec.(venueCredentialResetter); ok {
		if err := resetter.ResetLiveSettings(); err == nil {
			if block, exists := e.SystemSettings().Venues[v]; exists {
				restoreErr = runtime.ApplyLiveSettings(block.Live.APIKey, block.Live.PrivateKey, block.Live.SmartWallet, block.Live.Testnet, block.DataSource.BaseURL)
			}
		} else {
			restoreErr = err
		}
	} else {
		restoreErr = runtime.ApplyLiveSettings(stored.APIKey, stored.PrivateKey, stored.SmartWallet, stored.Testnet, "")
	}
	if restoreErr == nil {
		return
	}
	// A probe that cannot put the saved credentials back leaves the executor on
	// the tested candidate, so the failure has to be visible: swallowing it would
	// let a "credential check" silently switch what the next order signs with.
	e.mu.Lock()
	e.systemError = fmt.Sprintf("%s 凭据检测后恢复已存配置失败：%v", v.Label(), restoreErr)
	e.mu.Unlock()
}

// maskCredential keeps the identifying prefix of an API key, matching how the
// Polymarket probe reports it.
func maskCredential(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "…"
}

// venueBaseURL is the data-source override for one venue. Polymarket keeps the
// legacy gamma_base_url field; Predict.fun uses its own base_url.
func venueBaseURL(settings SystemSettings, v market.Venue) string {
	entry, ok := settings.Venues[v]
	if !ok {
		return ""
	}
	if v == market.VenuePredictFun {
		return entry.DataSource.BaseURL
	}
	return entry.DataSource.GammaBaseURL
}

// SystemRuntime reports the effective endpoints and every venue's executor
// state. The flat Gamma/CLOB fields remain the Polymarket view.
func (e *Engine) SystemRuntime() SystemRuntime {
	runtime := SystemRuntime{}
	if provider, ok := e.data(market.VenuePolymarket).(dataSourceRuntime); ok && provider != nil {
		runtime.GammaBaseURL = provider.BaseURL()
		runtime.GammaDefault = provider.DefaultBaseURL()
	}
	if live, ok := e.venues.Executor(market.VenuePolymarket).(liveSettingsRuntime); ok && live != nil {
		runtime.CLOBBaseURL = live.EffectiveCLOBURL()
		runtime.LiveEnvironment = live.Environment()
		runtime.CLOBDefault = runtime.LiveEnvironment.CLOBBaseURL
	}
	runtime.LiveConfigured, runtime.LiveReady, runtime.LiveError = e.LiveStatus()
	runtime.Venues = e.venueRuntimes()
	e.mu.RLock()
	runtime.Error = e.systemError
	e.mu.RUnlock()
	// A venue without settlement support must say so where an operator can see
	// it, instead of leaving positions open with no explanation.
	var notices []string
	for _, v := range e.Venues() {
		if caps, ok := e.capabilities(v); ok && !caps.SupportsSettlement {
			notices = append(notices, fmt.Sprintf("%s 暂不支持自动结算", v.Label()))
		}
	}
	if notice := strings.Join(notices, "; "); notice != "" {
		if runtime.Error != "" {
			runtime.Error += "; " + notice
		} else {
			runtime.Error = notice
		}
	}
	return runtime
}

// venueRuntimes builds one status row per registered venue.
// venueNetworkReporter is implemented by executors that can say which network
// they are trading on right now.
type venueNetworkReporter interface {
	Testnet() bool
}

func (e *Engine) venueRuntimes() []VenueRuntime {
	runtimes := make([]VenueRuntime, 0, len(e.Venues()))
	for _, v := range e.Venues() {
		status := VenueRuntime{Venue: v}
		if caps, ok := e.capabilities(v); ok {
			status.Testnet = caps.Testnet
		}
		exec := e.venues.Executor(v)
		// Prefer the network the executor is actually pointed at: capabilities
		// are frozen at registration, so a credential save that switches networks
		// would otherwise leave the badge lying about which chain is live.
		if reporter, ok := exec.(venueNetworkReporter); ok {
			status.Testnet = reporter.Testnet()
		}
		if exec == nil {
			status.Error = ErrVenueReadOnly.Error()
			runtimes = append(runtimes, status)
			continue
		}
		status.Configured = exec.Configured()
		status.Signer = exec.Signer()
		status.Maker = exec.Maker()
		status.Error = exec.ReadyError()
		status.Ready = status.Configured && status.Error == ""
		runtimes = append(runtimes, status)
	}
	return runtimes
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
	return account
}

// LiveStatus reports the Polymarket executor state independently of the
// currently selected default account. It is the legacy health-check shape;
// SystemRuntime carries the per-venue view.
func (e *Engine) LiveStatus() (configured, ready bool, message string) {
	return e.LiveStatusFor(market.VenuePolymarket)
}

// LiveStatusFor reports one venue's executor state.
func (e *Engine) LiveStatusFor(v market.Venue) (configured, ready bool, message string) {
	exec, err := e.executor(v)
	if err != nil {
		return false, false, ErrVenueReadOnly.Error()
	}
	configured = exec.Configured()
	message = exec.ReadyError()
	return configured, configured && message == "", message
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

// Scan refreshes public data and processes every account, including automatic
// orders. It is used by the background engine and tests.
func (e *Engine) Scan(ctx context.Context) error {
	_, err := e.scan(ctx, ScanOptions{Execute: true})
	return err
}

// ScanOnly refreshes public data, marks positions and writes equity snapshots,
// but never places an order. Manual scans use this so a user who only wants to
// refresh the radar cannot accidentally trigger live automatic trading.
func (e *Engine) ScanOnly(ctx context.Context) error {
	_, err := e.scan(ctx, ScanOptions{})
	return err
}

// ManualScan runs a user-initiated scan. A non-zero accountID scopes automatic
// entries and stop-loss checks to that account, and an explicit execute choice
// overrides the account's background AutoTrade switch for this one pass. The
// summary reports how many positions were opened and how many held markets
// were skipped so the radar can explain the result.
func (e *Engine) ManualScan(ctx context.Context, accountID int64, execute bool, v market.Venue) (ScanSummary, error) {
	return e.scan(ctx, ScanOptions{Execute: execute, AccountID: accountID, Manual: accountID > 0, Venue: v})
}

func (e *Engine) scan(ctx context.Context, options ScanOptions) (ScanSummary, error) {
	summary := ScanSummary{AccountID: options.AccountID, Execute: options.Execute}
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()

	if options.AccountID > 0 {
		if _, err := e.repo.AccountByID(ctx, options.AccountID); err != nil {
			return summary, err
		}
	}

	accounts, err := e.repo.ListAccounts(ctx)
	if err != nil {
		return summary, err
	}
	// Each account owns one strategy block. Resolving them before the market
	// scan makes the radar account-specific and lets the venue list be a union:
	// an account that enabled Predict.fun must be scanned even when another
	// account only trades Polymarket.
	type accountPlan struct {
		account  Account
		settings Settings
	}
	plans := make([]accountPlan, 0, len(accounts))
	fallback := e.Settings()
	scanSettings := fallback
	enabledVenueSettings := make(map[market.Venue]VenueSettings, len(market.VenueOrder))
	maxDays := 0
	for _, account := range accounts {
		accountSettings, err := e.settingsForAccount(ctx, account.ID)
		if err != nil {
			return summary, err
		}
		plans = append(plans, accountPlan{account: account, settings: accountSettings})
		if accountSettings.MaxDays > maxDays {
			maxDays = accountSettings.MaxDays
		}
		for _, v := range e.Venues() {
			effective := accountSettings.For(v)
			if !effective.Enabled {
				continue
			}
			entry, exists := enabledVenueSettings[v]
			if !exists {
				entry = VenueSettings{Enabled: true}
			}
			// The scan only needs to admit anything at least one account
			// accepts; findOpportunity re-checks each account's own limits in
			// the per-account loop below.
			if effective.MinLiquidity < entry.MinLiquidity {
				entry.MinLiquidity = effective.MinLiquidity
			}
			enabledVenueSettings[v] = entry
		}
	}
	if len(plans) == 0 {
		// A brand-new database can show a radar before any account exists; use
		// the global defaults for that installation.
		if fallback.MaxDays > maxDays {
			maxDays = fallback.MaxDays
		}
		for _, v := range e.Venues() {
			if fallback.For(v).Enabled {
				enabledVenueSettings[v] = VenueSettings{Enabled: true}
			}
		}
	}
	if maxDays <= 0 {
		maxDays = fallback.MaxDays
	}
	scanSettings.MaxDays = maxDays
	scanSettings.Venues = enabledVenueSettings
	now := time.Now().UTC()
	venues := e.venueList(scanSettings, options.Venue)
	markets := make([]market.Market, 0)
	byID := make(map[string]market.Market)
	scanned := make([]market.Venue, 0, len(venues))
	failures := make([]string, 0)
	succeeded := 0
	for _, v := range venues {
		source := e.data(v)
		if source == nil {
			failures = append(failures, fmt.Sprintf("%s: 未注册行情源", v.Label()))
			continue
		}
		// Each venue gets its own deadline so one slow upstream cannot consume
		// the scan budget of another venue.
		scanCtx, cancel := venueScanContext(ctx, len(venues))
		items, listErr := source.ListOpen(scanCtx, scanSettings.MaxDays)
		cancel()
		if listErr != nil {
			// Error isolation: a failing venue is recorded and skipped, but the
			// opportunities of every other venue survive this scan.
			failures = append(failures, fmt.Sprintf("%s 行情获取失败: %v", v.Label(), listErr))
			continue
		}
		succeeded++
		scanned = append(scanned, v)
		for _, item := range items {
			// The registry guarantees this data source only returns its own
			// venue's markets, so stamp it explicitly instead of trusting a
			// provider that forgot the field.
			item.Venue = v
			if item.Currency == "" {
				item.Currency = currencyForVenue(v)
			}
			markets = append(markets, item)
			byID[marketKey(v, item.ID)] = item
		}
	}
	venueFailure := strings.Join(failures, "; ")
	if venueFailure != "" {
		e.recordScan(now, errors.New(venueFailure))
		if succeeded == 0 {
			return summary, fmt.Errorf("%w: %s", ErrUpstream, venueFailure)
		}
	} else {
		e.recordScan(now, nil)
	}

	opportunitiesByAccount := make(map[int64][]Opportunity, len(plans))
	unionByKey := make(map[string]Opportunity)
	for _, plan := range plans {
		items := make([]Opportunity, 0)
		for _, item := range markets {
			if !plan.settings.For(item.EffectiveVenue()).Enabled {
				continue
			}
			for _, outcome := range item.Outcomes {
				opportunity, candidateErr := findOpportunity(item, outcome.Name, outcome.TokenID, plan.settings, now)
				if candidateErr == nil {
					items = append(items, opportunity)
				}
			}
		}
		// Resolve categories and this account's sports policy only for rows that
		// already passed its price/liquidity/volatility filters.
		items = e.classifyOpportunities(ctx, items, byID, plan.settings)
		opportunitiesByAccount[plan.account.ID] = items
		for _, item := range items {
			unionByKey[opportunityKey(item)] = item
		}
	}

	// Language enrichment is account independent: localize the union once, then
	// copy the localized text back into every account's view.
	union := make([]Opportunity, 0, len(unionByKey))
	for _, item := range unionByKey {
		union = append(union, item)
	}
	union = e.localizeOpportunities(ctx, union)
	localized := make(map[string]Opportunity, len(union))
	for _, item := range union {
		localized[opportunityKey(item)] = item
	}
	for accountID, items := range opportunitiesByAccount {
		for i := range items {
			if localizedItem, ok := localized[opportunityKey(items[i])]; ok {
				items[i] = localizedItem
			}
		}
		sortOpportunities(items)
		opportunitiesByAccount[accountID] = items
	}

	e.mu.Lock()
	// A scan can be scoped to one venue (the radar chip, or a per-venue scan
	// button) or to one account (a manual scan). Replacing every view would
	// make other rows disappear until the next full scan, so in scope accounts
	// merge only the venues this pass covered and out-of-scope accounts keep
	// their previous view.
	views := make(map[int64][]Opportunity, len(opportunitiesByAccount))
	for accountID, items := range opportunitiesByAccount {
		previous := e.opportunitiesByAccount[accountID]
		if options.AccountID > 0 && accountID != options.AccountID {
			views[accountID] = previous
			continue
		}
		views[accountID] = mergeOpportunities(previous, items, scanned, options.Venue != "")
	}
	e.opportunitiesByAccount = views
	e.opportunities = flattenOpportunities(views)
	// The reported venue list must describe what the radar holds now, not just
	// what this pass touched.
	e.scannedVenues = coveredVenues(e.opportunities, scanned)
	e.lastSyncAt = timePointer(now)
	e.mu.Unlock()

	var processingErrors []string
	var focusErrors []string
	for _, plan := range plans {
		account := plan.account
		appendError := func(message string) {
			processingErrors = append(processingErrors, message)
			if options.AccountID == 0 || options.AccountID == account.ID {
				focusErrors = append(focusErrors, message)
			}
		}
		unresolved := 0
		if account.Mode == AccountModeLive {
			remaining, err := e.reconcileLiveOrders(ctx, account, byID)
			unresolved = remaining
			if err != nil {
				appendError(fmt.Sprintf("%s 对账: %v", account.Name, err))
			}
		}
		if err := e.refreshPositions(ctx, account.ID, byID); err != nil {
			appendError(fmt.Sprintf("%s: %v", account.Name, err))
			// Do not place new orders against positions whose marks or
			// settlement state could not be refreshed.
			unresolved++
		}
		// A user-initiated execute pass only touches the selected account;
		// the other accounts are still marked to market and snapshotted above.
		scoped := options.AccountID == 0 || options.AccountID == account.ID
		// An explicit manual request overrides the background AutoTrade switch
		// for this one pass. Background scans keep respecting it.
		autoEnabled := account.AutoTrade || options.Manual
		if options.Execute && scoped && (e.isRunning() || options.Manual) {
			if unresolved > 0 {
				appendError(fmt.Sprintf("%s: 存在未对账或未刷新的持仓，跳过本轮止损与自动交易", account.Name))
			} else {
				// Stop-loss is independent of the auto-entry switch: an operator
				// who only buys manually still needs an automatic exit. It runs
				// before new entries so a breached position is reduced first.
				stopErr := e.runStopLoss(ctx, account, plan.settings)
				if stopErr != nil {
					appendError(fmt.Sprintf("%s 止损: %v", account.Name, stopErr))
				}
				// Do not add risk while an exit is failing.
				if autoEnabled && stopErr == nil {
					outcome, tradeErr := e.autoTrade(ctx, account, opportunitiesByAccount[account.ID], plan.settings)
					summary.OrdersPlaced += outcome.Placed
					summary.SkippedHeld += outcome.SkippedHeld
					if tradeErr != nil {
						appendError(fmt.Sprintf("%s: %v", account.Name, tradeErr))
					}
				}
			}
		}
		if err := e.snapshot(ctx, account); err != nil {
			appendError(fmt.Sprintf("%s: %v", account.Name, err))
		}
	}
	summary.Error = strings.Join(focusErrors, "; ")
	processing := strings.Join(processingErrors, "; ")
	// Keep the per-venue scan failure visible even when account processing
	// succeeded; an operator must be able to see which venue is degraded.
	switch {
	case venueFailure == "":
		e.setProcessingError(processing)
	case processing == "":
		e.setProcessingError(venueFailure)
	default:
		e.setProcessingError(venueFailure + "; " + processing)
	}
	return summary, nil
}

// localizeOpportunities enriches each venue's rows from that venue's provider.
// Venues without a LocalizedProvider (Predict.fun) are left untouched.
func (e *Engine) localizeOpportunities(ctx context.Context, opportunities []Opportunity) []Opportunity {
	idsByVenue := make(map[market.Venue][]string)
	seen := make(map[string]bool, len(opportunities))
	for _, opportunity := range opportunities {
		v := opportunity.EffectiveVenue()
		key := marketKey(v, opportunity.MarketID)
		if seen[key] {
			continue
		}
		seen[key] = true
		idsByVenue[v] = append(idsByVenue[v], opportunity.MarketID)
	}
	for v, ids := range idsByVenue {
		provider, ok := e.data(v).(market.LocalizedProvider)
		if !ok || provider == nil {
			continue
		}
		localized, err := e.marketsLocale(ctx, provider, ids, "zh")
		if err != nil {
			continue
		}
		byID := make(map[string]market.Market, len(localized))
		for _, item := range localized {
			byID[marketKey(v, item.ID)] = item
		}
		for i := range opportunities {
			if opportunities[i].EffectiveVenue() != v {
				continue
			}
			localizedItem, ok := byID[marketKey(v, opportunities[i].MarketID)]
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
	return opportunities
}

// marketsLocale gives translation its own short deadline. A stalled locale
// request must not delay price discovery or an automatic entry.
func (e *Engine) marketsLocale(ctx context.Context, provider market.LocalizedProvider, ids []string, locale string) ([]market.Market, error) {
	localeCtx, cancel := context.WithTimeout(ctx, localizationTimeout)
	defer cancel()
	return provider.MarketsLocale(localeCtx, ids, locale)
}

// sortOpportunities orders the radar the way the UI renders it: soonest to
// resolve first, highest gross yield first within a day.
func sortOpportunities(opportunities []Opportunity) {
	// Stable so equal rows keep their scan order between refreshes: an unstable
	// sort makes the table reshuffle for no reason and breaks the row identity
	// the UI keeps across a poll.
	sort.SliceStable(opportunities, func(i, j int) bool {
		if opportunities[i].EndDate.Equal(opportunities[j].EndDate) {
			return opportunities[i].GrossYield > opportunities[j].GrossYield
		}
		return opportunities[i].EndDate.Before(opportunities[j].EndDate)
	})
}

// mergeOpportunities combines a scan result with the previous radar. A full scan
// (every requested venue was scanned) replaces it; a scoped scan replaces only
// the rows of the venues it covered, because dropping the others would look like
// those markets vanished.
// mergeOpportunities keeps the radar coherent across scoped scans. A full scan
// replaces the radar outright (a venue that failed simply keeps no rows, which
// is honest for a full pass). A scoped scan replaces only the venues it actually
// covered, because dropping the other venues' rows would empty the table the
// moment the operator clicks a venue chip.
//
// The decision is driven by an explicit flag rather than by comparing venue
// counts: a scoped scan requests exactly one venue, so "everything requested
// succeeded" describes both a one-venue scoped scan and a two-venue full scan,
// and inferring the scope from the counts silently emptied the radar for the
// former.
func mergeOpportunities(previous, current []Opportunity, scanned []market.Venue, scoped bool) []Opportunity {
	if !scoped {
		return current
	}
	covered := make(map[market.Venue]struct{}, len(scanned))
	for _, v := range scanned {
		covered[v] = struct{}{}
	}
	merged := make([]Opportunity, 0, len(previous)+len(current))
	for _, item := range previous {
		if _, replaced := covered[item.EffectiveVenue()]; !replaced {
			merged = append(merged, item)
		}
	}
	merged = append(merged, current...)
	sortOpportunities(merged)
	return merged
}

// coveredVenues lists the venues present in the radar, in scan order where
// possible, so a scoped scan does not under-report the venues on screen.
func coveredVenues(items []Opportunity, scanned []market.Venue) []market.Venue {
	present := make(map[market.Venue]struct{}, len(scanned))
	for _, v := range scanned {
		present[v] = struct{}{}
	}
	for _, item := range items {
		present[item.EffectiveVenue()] = struct{}{}
	}
	ordered := make([]market.Venue, 0, len(present))
	for _, v := range market.VenueOrder {
		if _, ok := present[v]; ok {
			ordered = append(ordered, v)
			delete(present, v)
		}
	}
	// Any venue outside the canonical order still has to be reported.
	rest := make([]market.Venue, 0, len(present))
	for v := range present {
		rest = append(rest, v)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	return append(ordered, rest...)
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
	positions, err := e.repo.ListPositions(ctx, accountID, "open", "")
	if err != nil {
		return err
	}
	var errs []string
	for _, position := range positions {
		v := position.EffectiveVenue()
		item, ok := markets[marketKey(v, position.MarketID)]
		if !ok {
			source := e.data(v)
			if source == nil {
				errs = append(errs, fmt.Sprintf("%s: 未注册 %s 行情源", position.MarketID, v.Label()))
				continue
			}
			getCtx, cancel := context.WithTimeout(ctx, positionRefreshTimeout)
			item, err = source.Get(getCtx, position.MarketID)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", position.MarketID, err))
				continue
			}
			item.Venue = v
			if item.Currency == "" {
				item.Currency = currencyForVenue(v)
			}
		}
		if item.EffectiveVenue() != v {
			// A position must never be marked or settled with another venue's
			// price feed, even if an upstream returned a colliding id.
			errs = append(errs, fmt.Sprintf("%s: 市场所属场所与持仓不一致", position.MarketID))
			continue
		}
		outcome, ok := item.OutcomeByToken(position.TokenID)
		if !ok {
			errs = append(errs, position.MarketID+": 持仓 token 已不存在")
			continue
		}
		// Settle only when this venue can resolve from reference data. A venue
		// without a settlement feed keeps its positions open instead of being
		// guessed closed from another exchange's prices.
		if e.supportsSettlement(v) {
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
				// A partial stop-loss exit may have booked realized PnL already;
				// the remaining cost basis is settled on top of it.
				position.RealizedPNL += payout - position.Cost
				kind := position.Kind
				if kind == "" {
					kind = "manual"
				}
				order := Order{
					AccountID: accountID, Venue: v, Currency: position.EffectiveCurrency(),
					PositionID: &position.ID, MarketID: position.MarketID, Question: position.Question,
					Outcome: position.Outcome, TokenID: position.TokenID, Kind: kind, Type: "settle",
					Amount: payout, Price: position.CurrentPrice, Shares: position.Shares, Status: "settled",
					Reason: fmt.Sprintf("依据公开结算数据按 %.4f 结算", settlementPrice), CreatedAt: closedAt,
				}
				if err := e.repo.SettlePosition(ctx, position, order); err != nil {
					errs = append(errs, err.Error())
				}
				continue
			}
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

// autoTradeOutcome summarizes one account's automatic entry pass.
type autoTradeOutcome struct {
	Placed      int
	SkippedHeld int
}

func (e *Engine) autoTrade(ctx context.Context, account Account, opportunities []Opportunity, settings Settings) (autoTradeOutcome, error) {
	// One account belongs to one venue, so only that venue's rows may be
	// traded. Mixing them would spend USDC on a USDT market.
	venue := account.EffectiveVenue()
	scoped := make([]Opportunity, 0, len(opportunities))
	for _, opportunity := range opportunities {
		if opportunity.EffectiveVenue() == venue {
			scoped = append(scoped, opportunity)
		}
	}
	if account.Mode == AccountModeLive {
		return e.autoLiveTrade(ctx, account, scoped, settings)
	}
	return e.autoPaperTrade(ctx, account, scoped, settings)
}

// paperFee is the simulated taker fee for one venue. The venue's own reported
// rate wins; otherwise the venue's effective settings rate is used. Polymarket
// keeps its historical notional-rate model, Predict.fun uses
// rate × min(p, 1-p) × shares, so a cheap Predict.fun fill is not underpriced.
func paperFee(opportunity Opportunity, settings Settings, shares, price float64) float64 {
	effective := settings.For(opportunity.EffectiveVenue())
	rate := opportunity.FeeRateBPS
	if rate <= 0 {
		rate = effective.FeeBPS
	}
	return venue.FeeModelFor(opportunity.EffectiveVenue(), rate)(shares, price)
}

// autoEntryWindow returns the configured pre-close entry window for an
// opportunity. Crypto markets use their own window in seconds: they are often
// hourly and settle on a price print, so a minutes window is too coarse for
// them. Sports markets use a shorter minutes window because overtime and
// late-game swings can still change the result.
func autoEntryWindow(opportunity Opportunity, settings Settings) time.Duration {
	// Crypto wins over the generic window: a market classified as Crypto is
	// never a sports game in this taxonomy, so the two branches cannot conflict.
	if opportunity.Category == string(market.CategoryCrypto) {
		return time.Duration(settings.CryptoEntryWindowSeconds) * time.Second
	}
	minutes := settings.EntryWindowMinutes
	if opportunity.SportsGame || opportunity.Category == string(market.CategorySports) {
		minutes = settings.SportsEntryWindowMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// withinAutoEntryWindow keeps automatic entries inside the last configured
// window before a market closes, when a 0.95–0.99 tail price has little time
// left to reverse. A zero window disables the gate. Manual orders are never
// gated.
func withinAutoEntryWindow(opportunity Opportunity, settings Settings, now time.Time) bool {
	window := autoEntryWindow(opportunity, settings)
	if window <= 0 {
		return true
	}
	remaining := opportunity.EndDate.Sub(now)
	return remaining > 0 && remaining <= window
}

func (e *Engine) autoPaperTrade(ctx context.Context, account Account, opportunities []Opportunity, settings Settings) (outcome autoTradeOutcome, err error) {
	effective := settings.For(account.EffectiveVenue())
	if !effective.Enabled {
		return outcome, nil
	}
	positions, err := e.repo.ListPositions(ctx, account.ID, "all", "")
	if err != nil {
		return outcome, err
	}
	// Every position, open or already settled, is the durable "already
	// ordered" mark for its market. One automatic entry per market is enough:
	// a second outcome of the same binary market is the mirror bet, and a
	// settled market must never be re-entered by a later scan. The key is
	// namespaced by venue because token/market ids are only venue-local.
	orderedMarkets := make(map[string]bool, len(positions))
	heldMarkets := make(map[string]bool, len(positions))
	skippedMarkets := make(map[string]bool)
	defer func() { outcome.SkippedHeld = len(skippedMarkets) }()
	marketExposure := make(map[string]float64)
	eventExposure := make(map[string]float64)
	totalExposure, realized := 0.0, 0.0
	for _, position := range positions {
		key := marketKey(position.EffectiveVenue(), position.MarketID)
		orderedMarkets[key] = true
		heldMarkets[key] = true
		realized += position.RealizedPNL
		if position.Status != "open" {
			continue
		}
		marketExposure[key] += position.Cost
		if event := eventKey(position.EffectiveVenue(), position.EventID); event != "" {
			eventExposure[event] += position.Cost
		}
		totalExposure += position.Cost
	}
	baseline := account.BaselineEquity
	if baseline <= 0 {
		baseline = settings.InitialBankroll
	}
	cash := baseline + realized - totalExposure
	now := time.Now().UTC()
	for _, opportunity := range opportunities {
		key := marketKey(opportunity.EffectiveVenue(), opportunity.MarketID)
		if orderedMarkets[key] {
			if heldMarkets[key] {
				skippedMarkets[key] = true
			}
			continue
		}
		if !withinAutoEntryWindow(opportunity, settings, now) {
			continue
		}
		fillPrice := opportunity.Price * (1 + settings.SlippageBPS/10000)
		if fillPrice <= 0 || fillPrice > settings.MaxPrice || fillPrice >= 1 {
			continue
		}
		amount := effective.OrderSize
		shares := amount / fillPrice
		fee := paperFee(opportunity, settings, shares, fillPrice)
		cost := amount + fee
		if totalExposure+cost > effective.MaxExposure+1e-9 {
			break
		}
		if cost > cash+1e-9 {
			break
		}
		if marketExposure[key]+cost > effective.MaxPosition+1e-9 {
			continue
		}
		// Every market of one event settles on the same outcome, so a cap per
		// event keeps one election or game from absorbing the whole bankroll in
		// a single scan.
		event := eventKey(opportunity.EffectiveVenue(), opportunity.EventID)
		if event != "" && settings.MaxEventExposure > 0 &&
			eventExposure[event]+cost > settings.MaxEventExposure+1e-9 {
			continue
		}
		position, err := e.executePaperBuy(ctx, account, opportunity, "auto", amount, settings)
		if err != nil {
			// A risk rejection is already logged as an order; it must not stop
			// the engine from evaluating the remaining candidates this round.
			if errors.Is(err, ErrInvalidOrder) {
				continue
			}
			return outcome, err
		}
		outcome.Placed++
		orderedMarkets[key] = true
		marketExposure[key] += position.Cost
		if event != "" {
			eventExposure[event] += position.Cost
		}
		totalExposure += position.Cost
		cash -= position.Cost
	}
	return outcome, nil
}

func (e *Engine) autoLiveTrade(ctx context.Context, account Account, opportunities []Opportunity, settings Settings) (outcome autoTradeOutcome, err error) {
	effective := settings.For(account.EffectiveVenue())
	if !effective.Enabled {
		return outcome, nil
	}
	exec, execErr := e.executor(account.EffectiveVenue())
	if execErr != nil {
		return outcome, fmt.Errorf("%w", ErrVenueReadOnly)
	}
	if !exec.Configured() {
		return outcome, fmt.Errorf("%w: %s 实盘自动交易已开启，但%s", ErrVenueNotConfigured, account.EffectiveVenue().Label(), exec.ReadyError())
	}
	if message := exec.ReadyError(); message != "" {
		return outcome, fmt.Errorf("%w: %s 实盘自动交易已开启，但实盘凭据未就绪: %s", ErrVenueNotConfigured, account.EffectiveVenue().Label(), message)
	}
	positions, err := e.repo.ListPositions(ctx, account.ID, "all", "")
	if err != nil {
		return outcome, err
	}
	orderedMarkets := make(map[string]bool)
	heldMarkets := make(map[string]bool, len(positions))
	skippedMarkets := make(map[string]bool)
	defer func() { outcome.SkippedHeld = len(skippedMarkets) }()
	marketExposure := make(map[string]float64)
	eventExposure := make(map[string]float64)
	totalExposure, realized := 0.0, 0.0
	for _, position := range positions {
		key := marketKey(position.EffectiveVenue(), position.MarketID)
		orderedMarkets[key] = true
		heldMarkets[key] = true
		realized += position.RealizedPNL
		if position.Status == "open" {
			marketExposure[key] += position.Cost
			if event := eventKey(position.EffectiveVenue(), position.EventID); event != "" {
				eventExposure[event] += position.Cost
			}
			totalExposure += position.Cost
		}
	}
	if account.BaselineEquity <= 0 {
		account = e.ensureLiveBaseline(ctx, account, totalExposure, realized)
	}
	strategyCash := account.BaselineEquity + realized - totalExposure
	snapshot, err := exec.Collateral(ctx)
	if err != nil {
		return outcome, fmt.Errorf("%w: 读取实盘余额失败: %v", ErrLiveUnavailable, err)
	}
	cash := math.Min(strategyCash, snapshot.Amount)
	now := time.Now().UTC()
	for _, opportunity := range opportunities {
		key := marketKey(opportunity.EffectiveVenue(), opportunity.MarketID)
		// The set is keyed by venue+market: all positions, including settled
		// ones, mark the market as already ordered so auto trading cannot
		// re-enter it.
		if orderedMarkets[key] {
			if heldMarkets[key] {
				skippedMarkets[key] = true
			}
			continue
		}
		if !withinAutoEntryWindow(opportunity, settings, now) {
			continue
		}
		if opportunity.ConditionID == "" {
			continue
		}
		if opportunity.IsV2() {
			// Protocol V2 markets cannot be signed by this build. Skip them
			// instead of writing a rejected order on every scan; a manual order
			// still reports the reason explicitly.
			continue
		}
		budget := effective.OrderSize
		estimatedFee, feeErr := exec.EstimateBuyFee(ctx, opportunity.ConditionID, budget, opportunity.Price)
		if feeErr != nil {
			return outcome, fmt.Errorf("%w: 估算实盘手续费失败: %v", ErrLiveUnavailable, feeErr)
		}
		if estimatedFee < 0 || math.IsNaN(estimatedFee) || math.IsInf(estimatedFee, 0) {
			return outcome, fmt.Errorf("%w: 实盘手续费估算无效", ErrLiveUnavailable)
		}
		cost := budget + estimatedFee
		if totalExposure+cost > effective.MaxExposure+1e-9 {
			break
		}
		if marketExposure[key]+cost > effective.MaxPosition+1e-9 {
			continue
		}
		event := eventKey(opportunity.EffectiveVenue(), opportunity.EventID)
		if event != "" && settings.MaxEventExposure > 0 &&
			eventExposure[event]+cost > settings.MaxEventExposure+1e-9 {
			continue
		}
		if cost > cash+1e-9 {
			return outcome, fmt.Errorf("%w: 实盘策略资金不足（含预估手续费）", ErrInvalidOrder)
		}
		if snapshot.Allowance > 0 && cost > snapshot.Allowance+1e-9 {
			break
		}
		position, err := e.executeLiveBuy(ctx, account, opportunity, "auto", budget, settings)
		if err != nil {
			if errors.Is(err, ErrInvalidOrder) || errors.Is(err, ErrLiveUnavailable) || errors.Is(err, ErrVenueNotConfigured) {
				continue
			}
			return outcome, err
		}
		outcome.Placed++
		orderedMarkets[key] = true
		marketExposure[key] += position.Cost
		if event != "" {
			eventExposure[event] += position.Cost
		}
		totalExposure += position.Cost
		cash -= position.Cost
	}
	return outcome, nil
}

// runStopLoss checks every open position of an account against the configured
// stop ratio and submits a market sell when the mark has fallen to the trigger.
// Polymarket has no native stop order, so this is a local monitor: the scan
// interval bounds how quickly a breach is noticed, and a gap can fill far below
// the trigger. Only accounts with automatic trading enabled are allowed to sell
// without an explicit click.
func (e *Engine) runStopLoss(ctx context.Context, account Account, settings Settings) error {
	if !settings.StopLossEnabled {
		return nil
	}
	positions, err := e.repo.ListPositions(ctx, account.ID, "open", "")
	if err != nil {
		return err
	}
	var errs []string
	for _, position := range positions {
		if position.Shares <= 0 || position.EntryPrice <= 0 || position.CurrentPrice <= 0 {
			continue
		}
		// A sell is a limit order the venue floors by shares, so a position
		// bought for a few dollars cannot be exited at all. Say so instead of
		// submitting an order the exchange rejects on every scan.
		trigger := position.EntryPrice * settings.StopLossRatio
		if position.CurrentPrice > trigger+1e-9 {
			continue
		}
		if caps, ok := e.capabilities(position.EffectiveVenue()); ok && caps.MinSellShares > 0 &&
			position.Shares+1e-9 < caps.MinSellShares {
			errs = append(errs, fmt.Sprintf("持仓 %d: %.4f 份低于 %s 卖单最小 %.0f 份，无法自动止损，只能持有到结算",
				position.ID, position.Shares, caps.Label, caps.MinSellShares))
			continue
		}
		reason := fmt.Sprintf("触发止损：参考价 %.4f ≤ 入场价 %.4f × %.0f%%（止损线 %.4f）",
			position.CurrentPrice, position.EntryPrice, settings.StopLossRatio*100, trigger)
		var sellErr error
		if account.Mode == AccountModeLive {
			_, sellErr = e.executeLiveSell(ctx, account, position, reason)
		} else {
			_, sellErr = e.executePaperSell(ctx, account, position, reason, settings)
		}
		if sellErr != nil {
			errs = append(errs, fmt.Sprintf("持仓 %d: %v", position.ID, sellErr))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// applySellFill turns a sell fill into an updated position and its order. A
// partial fill reduces the shares and cost basis proportionally and leaves the
// position open; a full fill closes it with exit_kind='stop'. The status stays
// the ordinary lost value so older database CHECK constraints remain valid.
func applySellFill(position Position, fill LiveFill, action, reason string, now time.Time) (Position, Order, error) {
	if math.IsNaN(fill.Cost) || math.IsInf(fill.Cost, 0) || math.IsNaN(fill.Shares) || math.IsInf(fill.Shares, 0) {
		return Position{}, Order{}, fmt.Errorf("%w: 卖出回报金额无效", ErrUpstream)
	}
	if position.Shares <= 0 || position.Cost <= 0 {
		return Position{}, Order{}, fmt.Errorf("%w: 持仓份额或成本无效", ErrInvalidOrder)
	}
	sold := math.Min(fill.Shares, position.Shares)
	if sold <= 0 || fill.Cost <= 0 || fill.Price <= 0 || fill.Price >= 1 {
		return Position{}, Order{}, fmt.Errorf("%w: 卖出回报没有有效成交", ErrUpstream)
	}
	fraction := sold / position.Shares
	costSold := position.Cost * fraction
	updated := position
	updated.Fee += fill.Fee
	updated.RealizedPNL += fill.Cost - costSold
	updated.CurrentPrice = fill.Price
	remaining := position.Shares - sold
	if remaining <= 1e-9 {
		updated.Status = "lost"
		updated.ExitKind = "stop"
		closedAt := now
		updated.ClosedAt = &closedAt
		updated.UnrealizedPNL = 0
	} else {
		updated.Shares = remaining
		updated.Cost = position.Cost - costSold
		updated.UnrealizedPNL = remaining*fill.Price - updated.Cost
	}
	order := Order{
		AccountID: position.AccountID, Venue: position.EffectiveVenue(), Currency: position.EffectiveCurrency(),
		PositionID: &position.ID, MarketID: position.MarketID,
		Question: position.Question, Outcome: position.Outcome, TokenID: position.TokenID,
		Kind: "auto", Type: "settle", Action: action, Amount: fill.Cost, Price: fill.Price, Shares: sold,
		Fee: fill.Fee, Status: "filled", Reason: reason, CreatedAt: now,
	}
	return updated, order, nil
}

// reportedFeeRate is the fee rate the radar currently reports for a token, or 0
// when the market is no longer listed.
func (e *Engine) reportedFeeRate(v market.Venue, tokenID string) float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, item := range e.opportunities {
		if item.TokenID == tokenID && item.EffectiveVenue() == v {
			return item.FeeRateBPS
		}
	}
	return 0
}

func (e *Engine) executePaperSell(ctx context.Context, account Account, position Position, reason string, settings Settings) (Position, error) {
	fillPrice := position.CurrentPrice * (1 - settings.SlippageBPS/10000)
	if fillPrice <= 0 || fillPrice >= 1 {
		return Position{}, e.rejectSellOrder(ctx, account.ID, position, "模拟止损卖出价无效")
	}
	effective := settings.For(position.EffectiveVenue())
	// Charge the rate the entry was priced with: the venue's reported rate while
	// the market is still on the radar, otherwise the settings default. Charging
	// the default on the way out would price a Predict.fun exit ~13x cheaper than
	// its entry and inflate every simulated stop-loss.
	rate := e.reportedFeeRate(position.EffectiveVenue(), position.TokenID)
	if rate <= 0 {
		rate = effective.FeeBPS
	}
	fee := venue.FeeModelFor(position.EffectiveVenue(), rate)(position.Shares, fillPrice)
	gross := fillPrice * position.Shares
	fill := LiveFill{Shares: position.Shares, Cost: gross - fee, Price: fillPrice, Fee: fee, Status: "filled"}
	updated, order, err := applySellFill(position, fill, "stop", reason+"；模拟成交；未发送任何真实订单", time.Now().UTC())
	if err != nil {
		return Position{}, err
	}
	if err := e.repo.SellPosition(ctx, updated, order); err != nil {
		return Position{}, err
	}
	return updated, nil
}

func (e *Engine) executeLiveSell(ctx context.Context, account Account, position Position, reason string) (Position, error) {
	exec, execErr := e.executor(position.EffectiveVenue())
	if execErr != nil {
		return Position{}, e.rejectSellOrder(ctx, account.ID, position, "该场所当前只读，无法提交止损卖单")
	}
	if !exec.Configured() {
		return Position{}, e.rejectSellOrder(ctx, account.ID, position,
			fmt.Sprintf("%s 实盘未配置：%s", position.EffectiveVenue().Label(), exec.ReadyError()))
	}
	if message := exec.ReadyError(); message != "" {
		return Position{}, e.rejectSellOrder(ctx, account.ID, position,
			"实盘未就绪："+message)
	}
	if position.ConditionID == "" || position.TokenID == "" {
		return Position{}, e.rejectSellOrder(ctx, account.ID, position,
			"缺少 condition_id 或 token_id，无法提交止损卖单")
	}
	var pending Order
	fill, err := exec.Sell(ctx, position.TokenID, position.ConditionID, position.Shares, func(orderID string) error {
		created, createErr := e.repo.CreateOrder(ctx, Order{
			AccountID: account.ID, Venue: position.EffectiveVenue(), Currency: position.EffectiveCurrency(),
			PositionID: &position.ID, MarketID: position.MarketID,
			Question: position.Question, Outcome: position.Outcome, TokenID: position.TokenID,
			Kind: "auto", Type: "settle", Action: "stop", Price: position.CurrentPrice, Shares: position.Shares,
			Status: "pending", Reason: "实盘止损卖单已签名，等待成交回报",
			ExternalID: orderID, CreatedAt: time.Now().UTC(),
		})
		if createErr == nil {
			pending = created
		}
		return createErr
	})
	if err != nil {
		if pending.ID != 0 {
			status, message := "rejected", "实盘止损下单失败："+err.Error()
			explicitRejection := strings.EqualFold(fill.Status, "rejected") || strings.EqualFold(fill.Status, "canceled") ||
				strings.EqualFold(fill.Status, "cancelled") || strings.EqualFold(fill.Status, "expired")
			if strings.EqualFold(fill.Status, "unknown") || (fill.OrderID != "" && !explicitRejection) {
				status, message = "unknown", "实盘止损结果未知，等待对账："+err.Error()
			}
			_ = e.repo.MarkOrderStatus(ctx, pending.ID, status, message)
		} else {
			_ = e.rejectSellOrder(ctx, account.ID, position, "实盘止损下单失败："+err.Error())
		}
		if errors.Is(err, ErrInvalidOrder) {
			return Position{}, err
		}
		return Position{}, fmt.Errorf("%w: 实盘止损失败: %v", ErrUpstream, err)
	}
	if fill.Shares <= 0 || fill.Cost <= 0 {
		if pending.ID != 0 {
			status, message := "rejected", "实盘止损订单未产生成交（状态 "+fill.Status+"）"
			if strings.EqualFold(fill.Status, "unknown") {
				status = "unknown"
			}
			_ = e.repo.MarkOrderStatus(ctx, pending.ID, status, message)
			return Position{}, fmt.Errorf("%w: %s", ErrUpstream, message)
		}
		return Position{}, e.rejectSellOrder(ctx, account.ID, position, "实盘止损订单未产生成交")
	}
	now := time.Now().UTC()
	// venue.Fill.Cost is the gross proceeds; the taker fee is deducted from what
	// the seller actually receives. Clamp so an implausible fee surfaces as an
	// invalid fill instead of a negative amount.
	fill.Cost = math.Max(fill.Cost-fill.Fee, 0)
	updated, order, err := applySellFill(position, fill, "stop", position.EffectiveVenue().Label()+" CLOB 止损成交", now)
	if err != nil {
		return Position{}, err
	}
	if pending.ID != 0 {
		updated, err = e.repo.FinalizeLiveSell(ctx, pending.ID, updated, fill)
		if err != nil {
			return Position{}, fmt.Errorf("实盘止损已成交，但本地账务写入失败: %w", err)
		}
		return updated, nil
	}
	// Defensive fallback for an executor that does not call onSigned.
	if err := e.repo.SellPosition(ctx, updated, order); err != nil {
		return Position{}, fmt.Errorf("实盘止损已成交，但本地账务写入失败: %w", err)
	}
	return updated, nil
}

func (e *Engine) rejectSellOrder(ctx context.Context, accountID int64, position Position, reason string) error {
	_, _ = e.repo.CreateOrder(ctx, Order{
		AccountID: accountID, Venue: position.EffectiveVenue(), Currency: position.EffectiveCurrency(),
		PositionID: &position.ID, MarketID: position.MarketID,
		Question: position.Question, Outcome: position.Outcome, TokenID: position.TokenID,
		Kind: "auto", Type: "settle", Action: "stop", Price: position.CurrentPrice, Shares: position.Shares,
		Status: "rejected", Reason: reason, CreatedAt: time.Now().UTC(),
	})
	if strings.Contains(reason, "实盘未配置") || strings.HasPrefix(reason, "实盘未就绪") {
		return &venueError{kind: ErrVenueNotConfigured, message: reason}
	}
	if strings.Contains(reason, "只读") {
		return fmt.Errorf("%w", ErrVenueReadOnly)
	}
	return fmt.Errorf("%w: %s", ErrInvalidOrder, reason)
}

type OrderRequest struct {
	MarketID string  `json:"market_id" binding:"required"`
	Outcome  string  `json:"outcome" binding:"required"`
	TokenID  string  `json:"token_id" binding:"required"`
	Amount   float64 `json:"amount" binding:"required"`
	// Venue is optional. When supplied it must match the account's venue; when
	// omitted the account's venue decides which provider resolves the market.
	Venue market.Venue `json:"venue,omitempty"`
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
	v := account.EffectiveVenue()
	if request.Venue != "" && effectiveVenue(request.Venue) != v {
		return Position{}, fmt.Errorf("%w", ErrVenueMismatch)
	}
	settings, err := e.settingsForAccount(ctx, account.ID)
	if err != nil {
		return Position{}, err
	}
	if !settings.For(v).Enabled {
		return Position{}, fmt.Errorf("%w: %s 当前已在参数设置中禁用", ErrInvalidOrder, v.Label())
	}
	source := e.data(v)
	if source == nil {
		return Position{}, fmt.Errorf("%w: 未注册 %s 行情源", ErrUpstream, v.Label())
	}
	item, err := source.Get(ctx, request.MarketID)
	if err != nil {
		return Position{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	// The registry guarantees this data source only returns its own venue's
	// markets, so stamp it instead of trusting a provider that omitted it.
	item.Venue = v
	if item.Currency == "" {
		item.Currency = currencyForVenue(v)
	}
	opportunity, err := findOpportunity(item, request.Outcome, request.TokenID, settings, time.Now().UTC())
	if err != nil {
		return Position{}, err
	}
	if opportunity.EffectiveVenue() != v {
		return Position{}, fmt.Errorf("%w", ErrVenueMismatch)
	}
	if err := enforceSportsPolicy(e.classifyMarket(ctx, item), settings.SportsPolicy); err != nil {
		return Position{}, err
	}
	if account.Mode == AccountModeLive {
		return e.executeLiveBuy(ctx, account, opportunity, "manual", request.Amount, settings)
	}
	return e.executePaperBuy(ctx, account, opportunity, "manual", request.Amount, settings)
}

func (e *Engine) executePaperBuy(ctx context.Context, account Account, opportunity Opportunity, kind string, amount float64, settings Settings) (Position, error) {
	effective := settings.For(opportunity.EffectiveVenue())
	if !effective.Enabled {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, opportunity.EffectiveVenue().Label()+" 当前已在参数设置中禁用", "")
	}
	fillPrice := opportunity.Price * (1 + settings.SlippageBPS/10000)
	if fillPrice <= 0 || fillPrice > settings.MaxPrice || fillPrice >= 1 {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "计入滑点后超过最高买入价", "")
	}
	shares := amount / fillPrice
	fee := paperFee(opportunity, settings, shares, fillPrice)
	cost := amount + fee
	positions, err := e.repo.ListPositions(ctx, account.ID, "all", "")
	if err != nil {
		return Position{}, err
	}
	marketExposure, eventExposure, totalExposure, realized := 0.0, 0.0, 0.0, 0.0
	key := marketKey(opportunity.EffectiveVenue(), opportunity.MarketID)
	event := eventKey(opportunity.EffectiveVenue(), opportunity.EventID)
	for _, position := range positions {
		realized += position.RealizedPNL
		if position.Status == "open" {
			totalExposure += position.Cost
			if marketKey(position.EffectiveVenue(), position.MarketID) == key {
				marketExposure += position.Cost
			}
			if event != "" && eventKey(position.EffectiveVenue(), position.EventID) == event {
				eventExposure += position.Cost
			}
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
	case totalExposure+cost > effective.MaxExposure+1e-9:
		reason = "超过总敞口上限"
	case marketExposure+cost > effective.MaxPosition+1e-9:
		reason = "超过单市场敞口上限"
	case event != "" && settings.MaxEventExposure > 0 && eventExposure+cost > settings.MaxEventExposure+1e-9:
		// One event settles as a single bet; a batch of its markets must not
		// consume more than its share of the bankroll.
		reason = "超过单事件敞口上限"
	}
	if reason != "" {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, reason, "")
	}

	now := time.Now().UTC()
	position := Position{
		AccountID: account.ID, Venue: opportunity.EffectiveVenue(), Currency: opportunity.EffectiveCurrency(),
		MarketID: opportunity.MarketID, EventID: opportunity.EventID, ConditionID: opportunity.ConditionID,
		Question: opportunity.Question, Outcome: opportunity.Outcome, TokenID: opportunity.TokenID,
		Side: "BUY", Kind: kind, EntryPrice: fillPrice, CurrentPrice: opportunity.Price,
		Shares: shares, Cost: cost, Fee: fee, Status: "open", OpenedAt: now,
		EndDate: opportunity.EndDate, UnrealizedPNL: opportunity.Price*shares - cost,
	}
	position, _, err = e.repo.CreatePositionWithOrder(ctx, position, Order{
		AccountID: account.ID, Venue: position.EffectiveVenue(), Currency: position.EffectiveCurrency(),
		MarketID: position.MarketID, Question: position.Question,
		Outcome: position.Outcome, TokenID: position.TokenID, Kind: kind, Type: "buy",
		Amount: cost, Price: fillPrice, Shares: shares, Fee: fee, Status: "filled",
		Reason: "模拟成交；未发送任何真实订单", CreatedAt: now,
	})
	if err != nil {
		return Position{}, err
	}
	return position, nil
}

func (e *Engine) executeLiveBuy(ctx context.Context, account Account, opportunity Opportunity, kind string, amount float64, settings Settings) (Position, error) {
	v := opportunity.EffectiveVenue()
	if v != account.EffectiveVenue() {
		return Position{}, fmt.Errorf("%w", ErrVenueMismatch)
	}
	exec, execErr := e.executor(v)
	if execErr != nil {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, ErrVenueReadOnly.Error(), "")
	}
	if !exec.Configured() {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			fmt.Sprintf("%s 实盘未配置：%s", v.Label(), exec.ReadyError()), "")
	}
	if message := exec.ReadyError(); message != "" {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			"实盘未就绪："+message, "")
	}
	if opportunity.ConditionID == "" {
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			"缺少 condition_id，无法加载实盘手续费", "")
	}
	if opportunity.IsV2() {
		// Protocol V2 orders settle through ExchangeV3, which requires a
		// different EIP-712 domain (version "3"), pUSD/PositionManager approvals
		// and CONDITIONAL-V2 balances. Signing one with the CTFExchangeV2 domain
		// would be rejected by the exchange after the order is already on the
		// wire, so refuse it here with an actionable message instead.
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount,
			"该市场属于 Polymarket Protocol V2（positionId + ExchangeV3），当前实盘执行器只支持 CTF（v1）市场；请改用模拟账户或等待 V2 签名支持", "")
	}
	effective := settings.For(v)
	positions, err := e.repo.ListPositions(ctx, account.ID, "all", "")
	if err != nil {
		return Position{}, err
	}
	key := marketKey(v, opportunity.MarketID)
	marketExposure, totalExposure, realized := 0.0, 0.0, 0.0
	for _, position := range positions {
		realized += position.RealizedPNL
		if position.Status == "open" {
			totalExposure += position.Cost
			if marketKey(position.EffectiveVenue(), position.MarketID) == key {
				marketExposure += position.Cost
			}
		}
	}
	if account.BaselineEquity <= 0 {
		account = e.ensureLiveBaseline(ctx, account, totalExposure, realized)
	}
	strategyCash := account.BaselineEquity + realized - totalExposure
	snapshot, cashErr := exec.Collateral(ctx)
	if cashErr != nil {
		_ = e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "无法读取实盘余额："+cashErr.Error(), "")
		return Position{}, fmt.Errorf("%w: 无法读取实盘余额: %v", ErrLiveUnavailable, cashErr)
	}
	estimatedFee, feeErr := exec.EstimateBuyFee(ctx, opportunity.ConditionID, amount, opportunity.Price)
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
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘授权额度不足，请先在 "+v.Label()+" 完成授权", "")
	case totalExposure+totalCost > effective.MaxExposure+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "超过总敞口上限（含预估手续费）", "")
	case marketExposure+totalCost > effective.MaxPosition+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "超过单市场敞口上限（含预估手续费）", "")
	}

	var pending Order
	fill, err := exec.Buy(ctx, opportunity.TokenID, opportunity.ConditionID, amount, settings.MaxPrice, func(orderID string) error {
		created, createErr := e.repo.CreateOrder(ctx, Order{
			AccountID: account.ID, Venue: v, Currency: opportunity.EffectiveCurrency(),
			MarketID: opportunity.MarketID, Question: opportunity.Question,
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
			// Keep the exchange-side id even without a row: it is the only handle
			// reconciliation has on an order that reached the venue.
			_ = e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘下单失败："+err.Error(), fill.OrderID)
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
	// venue.Fill.Cost is the gross notional; the buyer pays the taker fee on top,
	// so the position's basis (and therefore the exposure cap it counts against)
	// is the fee-inclusive amount.
	fill.Cost += fill.Fee
	position := Position{
		AccountID: account.ID, Venue: v, Currency: opportunity.EffectiveCurrency(),
		MarketID: opportunity.MarketID, EventID: opportunity.EventID, ConditionID: opportunity.ConditionID,
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
		AccountID: account.ID, Venue: v, Currency: opportunity.EffectiveCurrency(),
		MarketID: position.MarketID, Question: position.Question,
		Outcome: position.Outcome, TokenID: position.TokenID, Kind: kind, Type: "buy",
		Amount: fill.Cost, Price: fill.Price, Shares: fill.Shares, Fee: fill.Fee, Status: "filled",
		Reason: v.Label() + " CLOB 实盘成交", ExternalID: fill.OrderID, CreatedAt: now,
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
	positions, err := e.repo.ListPositions(ctx, account.ID, "all", "")
	if err != nil {
		return len(orders), err
	}
	positionsByID := make(map[int64]Position, len(positions))
	for _, position := range positions {
		positionsByID[position.ID] = position
	}
	var errs []string
	remaining := 0
	for _, order := range orders {
		if order.ExternalID == "" {
			remaining++
			continue
		}
		orderVenue := order.EffectiveVenue()
		exec, execErr := e.executor(orderVenue)
		if execErr != nil {
			errs = append(errs, fmt.Sprintf("订单 %s 对账失败: %v", order.ExternalID, execErr))
			remaining++
			continue
		}
		if !exec.Configured() {
			errs = append(errs, fmt.Sprintf("订单 %s 对账失败: %s", order.ExternalID, exec.ReadyError()))
			remaining++
			continue
		}
		item, ok := markets[marketKey(orderVenue, order.MarketID)]
		if !ok {
			source := e.data(orderVenue)
			if source == nil {
				errs = append(errs, fmt.Sprintf("订单 %s 读取市场失败: 未注册行情源", order.ExternalID))
				remaining++
				continue
			}
			item, err = source.Get(ctx, order.MarketID)
			if err != nil {
				errs = append(errs, fmt.Sprintf("订单 %s 读取市场失败: %v", order.ExternalID, err))
				remaining++
				continue
			}
			item.Venue = orderVenue
			if item.Currency == "" {
				item.Currency = currencyForVenue(orderVenue)
			}
		}
		fill, found, lookupErr := exec.LookupOrder(ctx, order.ExternalID, item.ConditionID)
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
		if fill.Shares > 0 && fill.Cost > 0 && order.Action == "stop" {
			positionID := int64(0)
			if order.PositionID != nil {
				positionID = *order.PositionID
			}
			position, positionOK := positionsByID[positionID]
			if !positionOK || position.Status != "open" {
				if markErr := e.repo.MarkOrderStatus(ctx, order.ID, "rejected", "止损订单对应的持仓已不存在，请人工核对"); markErr != nil {
					errs = append(errs, markErr.Error())
					remaining++
				}
				continue
			}
			// LookupOrder reports the gross proceeds; net the taker fee the same
			// way the direct sell path does.
			fill.Cost = math.Max(fill.Cost-fill.Fee, 0)
			updated, _, applyErr := applySellFill(position, fill, "stop", orderVenue.Label()+" CLOB 止损成交（对账）", time.Now().UTC())
			if applyErr != nil {
				errs = append(errs, fmt.Sprintf("订单 %s 对账失败: %v", order.ExternalID, applyErr))
				remaining++
				continue
			}
			if _, finalizeErr := e.repo.FinalizeLiveSell(ctx, order.ID, updated, fill); finalizeErr != nil {
				errs = append(errs, fmt.Sprintf("订单 %s 对账写入失败: %v", order.ExternalID, finalizeErr))
				remaining++
			}
			continue
		}
		if fill.Shares > 0 && fill.Cost > 0 {
			// LookupOrder reports the gross notional; a buy pays the taker fee on
			// top, exactly as the direct buy path does.
			fill.Cost += fill.Fee
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
				AccountID: account.ID, Venue: orderVenue, Currency: order.EffectiveCurrency(),
				MarketID: order.MarketID, ConditionID: item.ConditionID,
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
		AccountID: accountID, Venue: opportunity.EffectiveVenue(), Currency: opportunity.EffectiveCurrency(),
		MarketID: opportunity.MarketID, Question: opportunity.Question,
		Outcome: opportunity.Outcome, TokenID: opportunity.TokenID, Kind: kind, Type: "buy",
		Amount: amount, Price: opportunity.Price, Status: "rejected", Reason: reason,
		ExternalID: externalID, CreatedAt: time.Now().UTC(),
	})
	if strings.Contains(reason, "实盘未配置") {
		return &venueError{kind: ErrVenueNotConfigured, message: reason}
	}
	if strings.Contains(reason, "只读") || strings.Contains(reason, "禁用") {
		return fmt.Errorf("%w", ErrVenueReadOnly)
	}
	return fmt.Errorf("%w: %s", ErrInvalidOrder, reason)
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
