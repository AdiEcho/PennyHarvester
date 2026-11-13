package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/venue"
)

// RedeemPosition burns a settled position's shares back into collateral. The
// engine records the attempt before the transaction is sent, so a crash between
// signing and the receipt still leaves an auditable order row, and it refuses a
// second redemption of the same position: the transaction costs gas and a
// duplicate would revert on chain after being paid for.
func (e *Engine) RedeemPosition(ctx context.Context, accountID, positionID int64) (venue.RedeemResult, error) {
	if err := e.requireConfiguration(); err != nil {
		return venue.RedeemResult{}, err
	}
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
	// Approving and ordering both mutate chain state for the same wallet, so a
	// redemption takes the same lock as an order. Taking it before the lookup
	// and pending insert makes duplicate detection and intent creation one
	// critical section: two concurrent clicks cannot both observe "no intent"
	// and then broadcast two transactions.
	e.tradeMu.Lock()
	defer e.tradeMu.Unlock()
	// A redemption already on its way must not be re-sent: the second
	// transaction would burn nothing and still cost gas. The chain stays the
	// real authority, but the intent row is the local guard. The orders table
	// stores a redemption as a settle action named "redeem".
	if _, ok, err := e.repo.ActiveChainOperation(ctx, accountID, v, "redeem", &position.ID); err != nil {
		return venue.RedeemResult{}, err
	} else if ok {
		return venue.RedeemResult{}, ErrChainOperationActive
	}
	if existing, ok, err := e.repo.RedeemIntent(ctx, accountID, position.ID); err != nil {
		return venue.RedeemResult{}, err
	} else if ok {
		return venue.RedeemResult{}, fmt.Errorf("%w: 该仓位已经提交过赎回（工单 %d）", ErrInvalidOrder, existing.ID)
	}
	op, err := e.beginChainOperation(ctx, accountID, v, chainIDOf(exec), "redeem", exec.Signer(), &position.ID, position.MarketID, position.TokenID)
	if err != nil {
		return venue.RedeemResult{}, err
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
		_ = e.finishChainOperation(ctx, op.ID, "reverted", "创建赎回工单失败，交易未发送")
		return venue.RedeemResult{}, err
	}
	signedHash := ""
	recorder := func(tx venue.ChainTxSigned) error {
		from := strings.TrimSpace(tx.From)
		if from == "" {
			from = op.FromAddress
		}
		chainID := tx.ChainID
		if chainID == 0 {
			chainID = op.ChainID
		}
		nonce := tx.Nonce
		if err := e.recordChainHash(ctx, op.ID, tx.Hash, from, &nonce, chainID); err != nil {
			return err
		}
		signedHash = tx.Hash
		return nil
	}
	result, err := redeemer.Redeem(ctx, venue.RedeemRequest{
		MarketID:    position.MarketID,
		ConditionID: position.ConditionID,
		TokenID:     position.TokenID,
		Shares:      position.Shares,
	}, recorder)
	txHash := transactionHashOf(err)
	if txHash == "" {
		txHash = signedHash
	}
	if txHash == "" {
		txHash = result.TxHash
	}
	if signedHash == "" && txHash != "" {
		// A venue adapter must journal before broadcast, but keep the engine's
		// own crash identity even if an older adapter only returns the hash on
		// its result or error.
		_ = e.recordChainHash(ctx, op.ID, txHash, exec.Signer(), nil, chainIDOf(exec))
	}
	if result.TxHash == "" {
		result.TxHash = txHash
	}
	if err == nil {
		_ = e.repo.MarkOrderStatus(ctx, intent.ID, "filled", clampReason("链上赎回交易已提交 "+txHash))
		_ = e.finishChainOperation(ctx, op.ID, "confirmed", "链上赎回交易已确认 "+txHash)
		return result, nil
	}
	// A signed transaction whose broadcast or receipt is uncertain must not
	// become a rejection: that would invite a second transaction with a new
	// nonce while the first may still land. Keep the row unresolved and
	// return the hash so an operator can check the chain.
	status := "rejected"
	reason := clampReason(err.Error())
	opStatus := "reverted"
	opReason := "签名或广播前失败，赎回交易未发送"
	switch {
	case transactionRevertedOf(err):
		opReason = "链上赎回交易已回滚 " + txHash
		if txHash != "" {
			reason = clampReason("链上赎回交易已回滚，交易 " + txHash)
		}
	case txHash != "":
		status = "unknown"
		opStatus = "unknown"
		reason = clampReason("广播或回执结果未知，交易 " + txHash + "，请先核对链上状态")
		opReason = reason
	}
	_ = e.repo.MarkOrderStatus(ctx, intent.ID, status, reason)
	_ = e.finishChainOperation(ctx, op.ID, opStatus, opReason)
	return result, withTransactionHash(fmt.Errorf("%w: %s 赎回失败: %w", ErrUpstream, label, err), txHash)
}

// transactionHashOf reads the signed hash from a chain adapter error without
// importing the adapter (which would create an engine/venue dependency cycle).
func transactionHashOf(err error) string {
	var hasher interface{ TransactionHash() string }
	if errors.As(err, &hasher) {
		return hasher.TransactionHash()
	}
	return ""
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

// autoTradeOutcome summarizes one account's automatic entry pass.
type autoTradeOutcome struct {
	Placed      int
	SkippedHeld int
}

func (e *Engine) autoTrade(ctx context.Context, account Account, opportunities []Opportunity, settings Settings) (autoTradeOutcome, error) {
	if err := e.requireConfiguration(); err != nil {
		return autoTradeOutcome{}, err
	}
	// One account belongs to one venue, so only that venue's rows may be
	// traded. Mixing them would spend USDC on a USDT market.
	venue := account.EffectiveVenue()
	scoped := make([]Opportunity, 0, len(opportunities))
	for _, opportunity := range opportunities {
		if opportunity.EffectiveVenue() != venue {
			continue
		}
		if opportunity.Stale {
			// Keep stale rows on the radar, but they are not an execution basis.
			continue
		}
		scoped = append(scoped, opportunity)
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
		allowance, hasAllowance := snapshot.AllowanceFor(opportunity.NegRisk)
		if hasAllowance && cost > allowance+1e-9 {
			break
		}
		if !hasAllowance && snapshot.Allowance > 0 && cost > snapshot.Allowance+1e-9 {
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
func (e *Engine) runStopLoss(ctx context.Context, account Account, settings Settings, staleMarkets map[string]bool, markets map[string]market.Market) error {
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
		if staleMarkets[marketKey(position.EffectiveVenue(), position.MarketID)] {
			// A stale mark may be hours old. Exiting on it can turn a healthy
			// position into a loss, so wait for a fresh scan instead.
			errs = append(errs, fmt.Sprintf("持仓 %d: 行情陈旧，暂停止损判断", position.ID))
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
			// Prefer the fee schedule from this scan, including markets that no
			// longer sit on the radar. The radar only holds rows that pass the
			// strategy filters; a position can fall out of it while still open.
			rate := 0.0
			if item, ok := markets[marketKey(position.EffectiveVenue(), position.MarketID)]; ok {
				rate = item.FeeRateBPS
			}
			_, sellErr = e.executePaperSell(ctx, account, position, reason, settings, rate)
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
		PositionID: &position.ID, MarketID: position.MarketID, EventID: position.EventID,
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

func (e *Engine) executePaperSell(ctx context.Context, account Account, position Position, reason string, settings Settings, scanRate float64) (Position, error) {
	fillPrice := position.CurrentPrice * (1 - settings.SlippageBPS/10000)
	if fillPrice <= 0 || fillPrice >= 1 {
		return Position{}, e.rejectSellOrder(ctx, account.ID, position, "模拟止损卖出价无效")
	}
	effective := settings.For(position.EffectiveVenue())
	// Charge the rate the entry was priced with: the venue's reported rate while
	// the market is still on the radar, otherwise the settings default. Charging
	// the default on the way out would price a Predict.fun exit ~13x cheaper than
	// its entry and inflate every simulated stop-loss.
	rate := scanRate
	if rate <= 0 {
		rate = e.reportedFeeRate(position.EffectiveVenue(), position.TokenID)
	}
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
			PositionID: &position.ID, MarketID: position.MarketID, EventID: position.EventID,
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
		PositionID: &position.ID, MarketID: position.MarketID, EventID: position.EventID,
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
	if err := e.requireConfiguration(); err != nil {
		return Position{}, err
	}
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
	if opportunity.Stale {
		return Position{}, fmt.Errorf("%w: 行情陈旧，禁止下单", ErrInvalidOrder)
	}
	if opportunity.EffectiveVenue() != v {
		return Position{}, fmt.Errorf("%w", ErrVenueMismatch)
	}
	if err := enforceSportsPolicy(e.classifyMarket(ctx, item), settings.SportsPolicy); err != nil {
		return Position{}, err
	}
	if account.Mode == AccountModeLive {
		// Manual entries must see the exchange's latest fill state before their
		// own risk checks. A reconcile failure is fatal: continuing could reserve
		// exposure that is already spent by an order the ledger still calls open.
		if _, err := e.reconcileLiveOrders(ctx, account, nil); err != nil {
			return Position{}, fmt.Errorf("实盘对账失败，暂不允许新开仓: %w", err)
		}
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
	unresolved, err := e.repo.ListUnresolvedOrders(ctx, account.ID)
	if err != nil {
		// A ledger read failure must never look like zero pending risk.
		_ = e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "无法读取未确认订单，拒绝下单："+err.Error(), "")
		return Position{}, fmt.Errorf("读取未确认订单失败: %w", err)
	}
	risk := unresolvedOrderRisk(unresolved, opportunity.EffectiveVenue(), opportunity.MarketID, opportunity.EventID)
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
	totalExposure += risk.total
	marketExposure += risk.market
	eventExposure += risk.event
	baseline := account.BaselineEquity
	if baseline <= 0 {
		baseline = settings.InitialBankroll
	}
	cash := baseline + realized - totalExposure
	reason := ""
	switch {
	case risk.marketConflict:
		reason = "该市场存在未确认订单，暂不允许新开仓"
	case risk.eventConflict:
		reason = "该事件存在未确认订单，暂不允许新开仓"
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
		MarketID: position.MarketID, EventID: position.EventID, Question: position.Question,
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
	unresolved, err := e.repo.ListUnresolvedOrders(ctx, account.ID)
	if err != nil {
		_ = e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "无法读取未确认订单，拒绝下单："+err.Error(), "")
		return Position{}, fmt.Errorf("读取未确认订单失败: %w", err)
	}
	risk := unresolvedOrderRisk(unresolved, v, opportunity.MarketID, opportunity.EventID)
	positions, err := e.repo.ListPositions(ctx, account.ID, "all", "")
	if err != nil {
		return Position{}, err
	}
	key := marketKey(v, opportunity.MarketID)
	event := eventKey(v, opportunity.EventID)
	marketExposure, eventExposure, totalExposure, realized := 0.0, 0.0, 0.0, 0.0
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
	totalExposure += risk.total
	marketExposure += risk.market
	eventExposure += risk.event
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
	allowance, hasAllowance := snapshot.AllowanceFor(opportunity.NegRisk)
	switch {
	case risk.marketConflict:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "该市场存在未确认订单，暂不允许新开仓", "")
	case risk.eventConflict:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "该事件存在未确认订单，暂不允许新开仓", "")
	case totalCost > cash+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘策略资金不足（含预估手续费）", "")
	case hasAllowance && totalCost > allowance+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘授权额度不足，请先在 "+v.Label()+" 完成授权", "")
	case !hasAllowance && snapshot.Allowance > 0 && totalCost > snapshot.Allowance+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "实盘授权额度不足，请先在 "+v.Label()+" 完成授权", "")
	case totalExposure+totalCost > effective.MaxExposure+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "超过总敞口上限（含预估手续费）", "")
	case marketExposure+totalCost > effective.MaxPosition+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "超过单市场敞口上限（含预估手续费）", "")
	case event != "" && settings.MaxEventExposure > 0 && eventExposure+totalCost > settings.MaxEventExposure+1e-9:
		return Position{}, e.rejectOrder(ctx, account.ID, opportunity, kind, amount, "超过单事件敞口上限（含预估手续费）", "")
	}

	var pending Order
	fill, err := exec.Buy(ctx, opportunity.TokenID, opportunity.ConditionID, amount, settings.MaxPrice, func(orderID string) error {
		created, createErr := e.repo.CreateOrder(ctx, Order{
			AccountID: account.ID, Venue: v, Currency: opportunity.EffectiveCurrency(),
			MarketID: opportunity.MarketID, EventID: opportunity.EventID, Question: opportunity.Question,
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
		MarketID: position.MarketID, EventID: position.EventID, Question: position.Question,
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
	// Reconciliation frequently finishes after the HTTP client that triggered
	// it has gone away. The exchange lookup uses ctx; every ledger write below
	// uses a bounded context detached from that cancellation.
	writeCtx, cancelWrites := context.WithTimeout(context.WithoutCancel(ctx), ledgerWriteTimeout)
	defer cancelWrites()
	orders, err := e.repo.ListUnresolvedOrders(writeCtx, account.ID)
	if err != nil {
		return 0, err
	}
	if len(orders) == 0 {
		return 0, nil
	}
	positions, err := e.repo.ListPositions(writeCtx, account.ID, "all", "")
	if err != nil {
		return len(orders), err
	}
	positionsByID := make(map[int64]Position, len(positions))
	closedMarkets := make(map[string]bool)
	for _, position := range positions {
		positionsByID[position.ID] = position
		if position.Status != "open" {
			closedMarkets[marketKey(position.EffectiveVenue(), position.MarketID)] = true
		}
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
				if markErr := e.repo.MarkOrderStatus(writeCtx, order.ID, "rejected", "CLOB 未找到该订单，按未成交处理"); markErr != nil {
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
				// A late sell fill after settlement must not reopen or re-settle a
				// closed position. Keep the order unresolved and make the unbooked
				// shares visible to an operator.
				reason := fmt.Sprintf("持仓已结算/已关闭，迟到成交 %g 份未入账，请人工核对", fill.Shares)
				if markErr := e.repo.MarkOrderStatus(writeCtx, order.ID, "unknown", reason); markErr != nil {
					errs = append(errs, markErr.Error())
				}
				remaining++
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
			if _, finalizeErr := e.repo.FinalizeLiveSell(writeCtx, order.ID, updated, fill); finalizeErr != nil {
				errs = append(errs, fmt.Sprintf("订单 %s 对账写入失败: %v", order.ExternalID, finalizeErr))
				remaining++
			}
			continue
		}
		if fill.Shares > 0 && fill.Cost > 0 {
			// LookupOrder reports the gross notional; a buy pays the taker fee on
			// top, exactly as the direct buy path does.
			fill.Cost += fill.Fee
			closedPosition := false
			if order.PositionID != nil {
				position, ok := positionsByID[*order.PositionID]
				closedPosition = !ok || position.Status != "open"
			}
			if item.Closed || closedMarkets[marketKey(orderVenue, order.MarketID)] || closedPosition {
				// A fill reported after the market or its position closed cannot be
				// booked as a fresh position. Leave it unresolved instead of dropping
				// the shares silently.
				reason := fmt.Sprintf("市场或持仓已关闭，迟到买入成交 %g 份未入账，请人工核对", fill.Shares)
				if markErr := e.repo.MarkOrderStatus(writeCtx, order.ID, "unknown", reason); markErr != nil {
					errs = append(errs, markErr.Error())
				}
				remaining++
				continue
			}
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
				MarketID: order.MarketID, EventID: item.EventID, ConditionID: item.ConditionID,
				Question: order.Question, Outcome: order.Outcome, TokenID: order.TokenID,
				Side: "BUY", Kind: kind, EntryPrice: fill.Price, CurrentPrice: outcome.Price,
				Shares: fill.Shares, Cost: fill.Cost, Fee: fill.Fee, Status: "open", OpenedAt: openedAt,
				EndDate: item.EndDate, UnrealizedPNL: outcome.Price*fill.Shares - fill.Cost,
			}
			if _, finalizeErr := e.repo.FinalizeLiveBuy(writeCtx, order.ID, position, fill); finalizeErr != nil {
				errs = append(errs, fmt.Sprintf("订单 %s 对账写入失败: %v", order.ExternalID, finalizeErr))
				remaining++
			}
			continue
		}
		switch strings.ToLower(strings.TrimSpace(fill.Status)) {
		case "canceled", "cancelled", "rejected", "expired":
			if markErr := e.repo.MarkOrderStatus(writeCtx, order.ID, "rejected", "实盘订单未成交："+fill.Status); markErr != nil {
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
		MarketID: opportunity.MarketID, EventID: opportunity.EventID, Question: opportunity.Question,
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
