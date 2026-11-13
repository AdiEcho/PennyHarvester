package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/venue"
)

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
	v = effectiveVenue(v)
	approver, label, err := e.approver(v)
	if err != nil {
		return venue.Approval{}, err
	}
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	if err := e.requireConfiguration(); err != nil {
		return venue.Approval{}, err
	}
	// A signed approval that has not resolved yet owns this wallet's nonce. A
	// second grant would sign different nonces against an unknown chain state,
	// so refuse until scan recovery has classified the first transaction.
	if _, ok, err := e.repo.ActiveChainOperation(ctx, venueApprovalAccountID, v, "approve", nil); err != nil {
		return venue.Approval{}, err
	} else if ok {
		return venue.Approval{}, ErrChainOperationActive
	}
	exec, err := e.executor(v)
	if err != nil {
		return venue.Approval{}, fmt.Errorf("%w: %s 只读运行，无法执行链上授权", ErrVenueReadOnly, label)
	}
	journal := newApprovalJournal(ctx, e, v, exec)
	approval, err := approver.GrantApprovals(ctx, journal.recorder)
	if err != nil {
		hash := journal.fail(err)
		return venue.Approval{}, fmt.Errorf("%w: %s 链上授权失败: %w", ErrUpstream, label, withTransactionHash(err, hash))
	}
	journal.succeed()
	return approval, nil
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
	// Resolve durable chain operations before taking the trade lock. Recovery
	// makes bounded RPC calls and must never block a redemption UI request; a
	// failed lookup leaves the operation active for the next scan.
	e.recoverChainOperations(ctx)
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
	staleMarkets := make(map[string]bool)
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
			if item.Stale {
				staleMarkets[marketKey(v, item.ID)] = true
			}
		}
	}
	venueFailure := strings.Join(failures, "; ")
	if venueFailure != "" {
		e.recordScan(now, errors.New(venueFailure))
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
				stopErr := e.runStopLoss(ctx, account, plan.settings, staleMarkets, byID)
				if stopErr != nil {
					appendError(fmt.Sprintf("%s 止损: %v", account.Name, stopErr))
				}
				// Do not add risk while an exit is failing.
				if autoEnabled && stopErr == nil && len(opportunitiesByAccount[account.ID]) > 0 {
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
	if venueFailure != "" && succeeded == 0 {
		// Every venue failed discovery. Maintenance above still reconciled
		// orders and refreshed positions from per-market reads, but the caller
		// must see that this scan produced no radar data.
		return summary, fmt.Errorf("%w: %s", ErrUpstream, venueFailure)
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
	// Final ledger writes must survive a cancelled HTTP request: marks and
	// settlements were already computed from upstream data, so losing the write
	// would leave positions and cash inconsistent.
	writeCtx, cancelWrites := context.WithTimeout(context.WithoutCancel(ctx), ledgerWriteTimeout)
	defer cancelWrites()
	unresolvedOrders, err := e.repo.ListUnresolvedOrders(writeCtx, accountID)
	if err != nil {
		return fmt.Errorf("加载未确认订单: %w", err)
	}
	unresolvedByPosition := make(map[int64]int, len(unresolvedOrders))
	for _, order := range unresolvedOrders {
		if order.PositionID != nil {
			unresolvedByPosition[*order.PositionID]++
		}
	}
	// Resolve the markets the scan view did not carry in one batch per venue. A
	// position in a market that already ended but is still open upstream never
	// appears in the open-market scan, so the old code spent one GET per
	// position inside the scan deadline. On a 10s cadence that deadline expired
	// mid-loop and every remaining position reported the same "context deadline
	// exceeded", which also blocked stop-loss and entries for the account.
	// Marking is independent of radar freshness, so the batch gets its own
	// budget instead of whatever the list views left over.
	prefetched := make(map[string]market.Market, len(positions))
	missing := make(map[market.Venue][]string)
	seen := make(map[string]bool, len(positions))
	for _, position := range positions {
		v := position.EffectiveVenue()
		key := marketKey(v, position.MarketID)
		if _, ok := markets[key]; ok || seen[key] {
			continue
		}
		seen[key] = true
		missing[v] = append(missing[v], position.MarketID)
	}
	var errs []string
	for v, ids := range missing {
		source := e.data(v)
		if source == nil {
			errs = append(errs, fmt.Sprintf("%s: 未注册 %s 行情源", strings.Join(ids, ","), v.Label()))
			continue
		}
		sort.Strings(ids)
		if batch, ok := source.(market.BatchProvider); ok {
			batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), positionBatchTimeout)
			items, batchErr := batch.GetMany(batchCtx, ids)
			cancel()
			for _, id := range ids {
				item, ok := items[id]
				if !ok {
					errs = append(errs, fmt.Sprintf("%s: 行情未返回该市场 %s", v.Label(), id))
					continue
				}
				item.Venue = v
				if item.Currency == "" {
					item.Currency = currencyForVenue(v)
				}
				prefetched[marketKey(v, id)] = item
			}
			if batchErr != nil {
				errs = append(errs, fmt.Sprintf("%s 持仓行情: %v", v.Label(), batchErr))
			}
			continue
		}
		for _, id := range ids {
			getCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), positionRefreshTimeout)
			item, getErr := source.Get(getCtx, id)
			cancel()
			if getErr != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", id, getErr))
				continue
			}
			item.Venue = v
			if item.Currency == "" {
				item.Currency = currencyForVenue(v)
			}
			prefetched[marketKey(v, id)] = item
		}
	}
	for _, position := range positions {
		v := position.EffectiveVenue()
		item, ok := markets[marketKey(v, position.MarketID)]
		if !ok {
			item, ok = prefetched[marketKey(v, position.MarketID)]
		}
		if !ok {
			// The batch already reported why this market is unavailable, so a
			// book of stale positions keeps one error line per market instead of
			// one per position.
			continue
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
		// A cached, expired snapshot is display-only: never book a final payout
		// from it. The position stays open until a fresh scan confirms closure.
		if item.Stale {
			unrealized := outcome.Price*position.Shares - position.Cost
			if err := e.repo.UpdateMark(writeCtx, position.ID, outcome.Price, unrealized); err != nil {
				errs = append(errs, err.Error())
			}
			continue
		}
		// Settle only when this venue can resolve from reference data. A venue
		// without a settlement feed keeps its positions open instead of being
		// guessed closed from another exchange's prices.
		if e.supportsSettlement(v) {
			if settlementPrice, resolved := item.SettlementPrice(position.TokenID); resolved {
				if pending := unresolvedByPosition[position.ID]; pending > 0 {
					// A stopped sell may already have moved these shares. Freeze the
					// mark at the resolved value instead of paying out a second time;
					// the order stays unresolved until reconciliation books or voids it.
					position.CurrentPrice = settlementPrice
					unrealized := settlementPrice*position.Shares - position.Cost
					if err := e.repo.UpdateMark(writeCtx, position.ID, settlementPrice, unrealized); err != nil {
						errs = append(errs, err.Error())
					}
					errs = append(errs, fmt.Sprintf("%s: 持仓已结算但存在未确认卖单，已冻结结算等待对账（未确认订单 %d 笔）", position.MarketID, pending))
					continue
				}
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
					PositionID: &position.ID, MarketID: position.MarketID, EventID: position.EventID,
					Question: position.Question, Outcome: position.Outcome, TokenID: position.TokenID, Kind: kind, Type: "settle",
					Amount: payout, Price: position.CurrentPrice, Shares: position.Shares, Status: "settled",
					Reason: fmt.Sprintf("依据公开结算数据按 %.4f 结算", settlementPrice), CreatedAt: closedAt,
				}
				if err := e.repo.SettlePosition(writeCtx, position, order); err != nil {
					errs = append(errs, err.Error())
				}
				continue
			}
		}
		unrealized := outcome.Price*position.Shares - position.Cost
		if err := e.repo.UpdateMark(writeCtx, position.ID, outcome.Price, unrealized); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
