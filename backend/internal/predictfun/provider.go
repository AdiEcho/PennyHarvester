package predictfun

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/venue"
)

const (
	// endsAtTTL is how long a category's endsAt is trusted. The list endpoint
	// has no end date, so every market would otherwise cost one category request
	// on every scan. Ten minutes matches the planning doc's cache floor.
	endsAtTTL = 10 * time.Minute
	// endsAtNegativeTTL bounds retries for a category that returned no usable
	// date. Failing faster would hammer the API during one scan; caching forever
	// would hide a fixed upstream.
	endsAtNegativeTTL = time.Minute
	// defaultMinNotional is Predict.fun's minimum order value in USDT.
	defaultMinNotional = 1.0
	// statsTTL is how long a market's statistics are trusted. The list payload
	// reports stats as null, so each market that needs a liquidity figure costs
	// one request; caching keeps a 60-second scan interval from re-reading them.
	statsTTL = time.Minute
	// statsWorkers bounds the per-market statistics fan-out. The API allows 240
	// requests per minute, and a scan of a few hundred markets shares that
	// budget with the list and category reads.
	statsWorkers = 8
	// maxStatsPerScan caps one scan's statistics reads so a large market list
	// cannot consume the whole rate limit.
	maxStatsPerScan = 250
)

// Provider implements venue.MarketData for Predict.fun. It also owns the
// token-id to market-metadata index the executor needs, because that mapping is
// only available from the market list: the order endpoints take a token id, and
// there is no "look up by token" route.
type Provider struct {
	client *Client

	ttl         time.Duration
	negativeTTL time.Duration
	now         func() time.Time

	mu          sync.Mutex
	endsAt      map[string]endsAtEntry
	stats       map[string]statsEntry
	byToken     map[string]marketMeta
	byCondition map[string]marketMeta
}

type endsAtEntry struct {
	endsAt    time.Time
	ok        bool
	fetchedAt time.Time
}

// statsEntry is one cached /v1/markets/{id}/stats response.
type statsEntry struct {
	liquidity float64
	volume    float64
	fetchedAt time.Time
}

// marketMeta is the subset of a market an order needs. It is captured while
// mapping the market list so signing never has to re-read a market.
type marketMeta struct {
	MarketID     string
	ConditionID  string
	Question     string
	YesTokenID   string
	NoTokenID    string
	NegRisk      bool
	Tick         float64
	FeeRateBps   float64
	FeeRateKnown bool
	MinNotional  float64
	EndDate      time.Time
}

// outcomeFor maps a token id to the side it trades. outcomes[0] is Yes
// (indexSet 1) and outcomes[1] is No (indexSet 2) on every probed market.
func (m marketMeta) outcomeFor(tokenID string) (Outcome, bool) {
	switch strings.TrimSpace(tokenID) {
	case "":
		return "", false
	case m.YesTokenID:
		return OutcomeYes, true
	case m.NoTokenID:
		return OutcomeNo, true
	default:
		return "", false
	}
}

// Provider implements venue.MarketData.
var _ venue.MarketData = (*Provider)(nil)

// NewProvider builds the market-data provider. The client may be read-only; the
// executor checks credentials separately.
func NewProvider(client *Client) *Provider {
	provider := &Provider{
		client:      client,
		ttl:         endsAtTTL,
		negativeTTL: endsAtNegativeTTL,
		now:         time.Now,
		endsAt:      map[string]endsAtEntry{},
		stats:       map[string]statsEntry{},
		byToken:     map[string]marketMeta{},
		byCondition: map[string]marketMeta{},
	}
	// A network or endpoint change invalidates the resolution dates and the
	// token index captured from the previous host.
	client.OnReconfigure(provider.Reset)
	return provider
}

// SetBaseURL overrides the REST root; an empty value restores the default for
// the current network. The engine discovers this through its optional
// dataSourceRuntime assertion.
func (p *Provider) SetBaseURL(raw string) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("Predict.fun 行情未配置")
	}
	return p.client.SetBaseURL(raw)
}

// BaseURL is the REST root in use.
func (p *Provider) BaseURL() string {
	if p == nil || p.client == nil {
		return ""
	}
	return p.client.BaseURL()
}

// DefaultBaseURL is the REST root implied by the current network.
func (p *Provider) DefaultBaseURL() string {
	if p == nil || p.client == nil {
		return DefaultBaseURL
	}
	return p.client.DefaultBaseURL()
}

// Reset drops every cache derived from the previous configuration.
func (p *Provider) Reset() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.endsAt = map[string]endsAtEntry{}
	p.stats = map[string]statsEntry{}
	p.byToken = map[string]marketMeta{}
	p.byCondition = map[string]marketMeta{}
	p.mu.Unlock()
}

// Venue reports which exchange this provider reads.
func (p *Provider) Venue() market.Venue { return market.VenuePredictFun }

// Name is the human-facing provider name shown next to sync status.
func (p *Provider) Name() string { return "Predict.fun" }

// skipMarket explains why a market was deliberately not surfaced. It is an
// error so Get can answer precisely, while ListOpen treats it as a skip.
type skipMarket struct{ reason string }

// Error returns the human-readable skip reason.
func (e skipMarket) Error() string { return e.reason }

// ListOpen reads open Predict.fun markets up to maxDays ahead.
//
// The list endpoint has no end date, so each distinct category slug costs one
// cached category request. A market whose end date cannot be resolved is
// skipped, never guessed: the engine treats a zero EndDate as invalid, and an
// invented one would trade into a market that may already be settled.
func (p *Provider) ListOpen(ctx context.Context, maxDays int) ([]market.Market, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("Predict.fun 行情未配置")
	}
	now := p.now().UTC()
	mapped := make([]market.Market, 0, maxPageSize)
	seen := make(map[string]bool, marketScanCap)
	failures := make([]string, 0, 5)
	cursor := ""

	for page := 0; page < marketScanCap/maxPageSize; page++ {
		markets, next, err := p.client.Markets(ctx, MarketQuery{
			First:  maxPageSize,
			Cursor: cursor,
			Status: statusOpen,
		})
		if err != nil {
			if len(mapped) == 0 {
				return nil, fmt.Errorf("读取 Predict.fun 市场列表: %w", err)
			}
			failures = appendFailure(failures, err.Error())
			break
		}
		for _, raw := range markets {
			id := strings.TrimSpace(raw.ID.String())
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			item, mapErr := p.mapMarket(ctx, raw, now, maxDays)
			if mapErr != nil {
				// A deliberate skip (yield-bearing, unknown end date, outside the
				// scan window) is an expected outcome of a market list that mixes
				// tradable and untradable markets. Only a real read failure should
				// turn "no markets at all" into an error.
				var skip skipMarket
				if !errors.As(mapErr, &skip) {
					failures = appendFailure(failures, mapErr.Error())
				}
				continue
			}
			mapped = append(mapped, item)
			if len(seen) >= marketScanCap {
				break
			}
		}
		if len(seen) >= marketScanCap {
			break
		}
		cursor = strings.TrimSpace(next)
		if cursor == "" {
			break
		}
	}

	if len(mapped) == 0 && len(failures) > 0 {
		return nil, fmt.Errorf("Predict.fun 没有可用市场: %s", strings.Join(failures, "; "))
	}
	p.fillLiquidity(ctx, mapped)
	return mapped, nil
}

// fillLiquidity fills the liquidity and volume the list endpoint leaves null.
// fillLiquidity refreshes the books behind a page of markets. Real Predict.fun
// payloads carry `stats: null` on the listing, which would leave every market at
// zero liquidity and hide the whole venue behind MinLiquidity. The per-market
// stats call reports the ask-side depth, so a scan can judge depth without
// pulling a full order book for every market.
//
// The fetched figure only ever fills a gap: a market whose listing already
// reported depth keeps that number, because the ask-side figure alone would
// hide a thin-but-tradable market that has most of its depth on the bid.
func (p *Provider) fillLiquidity(ctx context.Context, items []market.Market) {
	now := p.now()
	p.mu.Lock()
	missing := make([]string, 0, len(items))
	for _, item := range items {
		if item.Liquidity > 0 && item.Volume > 0 {
			continue
		}
		if entry, ok := p.stats[item.ID]; ok && now.Sub(entry.fetchedAt) < statsTTL {
			continue
		}
		missing = append(missing, item.ID)
	}
	p.mu.Unlock()
	if len(missing) > maxStatsPerScan {
		missing = missing[:maxStatsPerScan]
	}
	if len(missing) > 0 {
		semaphore := make(chan struct{}, statsWorkers)
		var wait sync.WaitGroup
		for _, id := range missing {
			wait.Add(1)
			go func(id string) {
				defer wait.Done()
				semaphore <- struct{}{}
				defer func() { <-semaphore }()
				stats, err := p.client.Stats(ctx, id)
				if err != nil {
					return
				}
				p.mu.Lock()
				p.stats[id] = statsEntry{liquidity: stats.LiquidityValue(), volume: stats.VolumeValue(), fetchedAt: now}
				p.mu.Unlock()
			}(id)
		}
		wait.Wait()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range items {
		entry, ok := p.stats[items[i].ID]
		if !ok {
			continue
		}
		if items[i].Liquidity <= 0 {
			items[i].Liquidity = entry.liquidity
		}
		if items[i].Volume <= 0 {
			items[i].Volume = entry.volume
		}
	}
}

func appendFailure(failures []string, message string) []string {
	if len(failures) >= 5 {
		return failures
	}
	return append(failures, message)
}

// Get reads one market and resolves its end date the same way ListOpen does.
func (p *Provider) Get(ctx context.Context, id string) (market.Market, error) {
	if p == nil || p.client == nil {
		return market.Market{}, fmt.Errorf("Predict.fun 行情未配置")
	}
	raw, err := p.client.Market(ctx, strings.TrimSpace(id))
	if err != nil {
		return market.Market{}, err
	}
	item, err := p.mapMarket(ctx, raw, p.now().UTC(), 0)
	if err != nil {
		return market.Market{}, fmt.Errorf("Predict.fun 市场 %s 不可用: %w", id, err)
	}
	return item, nil
}

// mapMarket converts one API market. A non-nil error means the market must not
// be surfaced; the message explains why.
func (p *Provider) mapMarket(ctx context.Context, raw Market, now time.Time, maxDays int) (market.Market, error) {
	// Yield-bearing markets settle through a separate contract family this build
	// cannot sign for. Refusing them here means the executor never sees a token
	// it would sign against the wrong exchange.
	if raw.IsYieldBearing {
		return market.Market{}, skipMarket{reason: fmt.Sprintf("市场 %s 为 isYieldBearing，当前版本不支持", raw.ID)}
	}
	if len(raw.Outcomes) < 2 {
		return market.Market{}, skipMarket{reason: fmt.Sprintf("市场 %s 不是二元市场", raw.ID)}
	}

	endsAt, ok, err := p.categoryEndsAt(ctx, raw.CategorySlug)
	if err != nil {
		return market.Market{}, err
	}
	if !ok {
		return market.Market{}, skipMarket{reason: fmt.Sprintf("市场 %s 无法确定到期时间", raw.ID)}
	}
	if maxDays > 0 && endsAt.After(now.AddDate(0, 0, maxDays)) {
		return market.Market{}, skipMarket{reason: fmt.Sprintf("市场 %s 到期时间超出扫描窗口", raw.ID)}
	}

	tick := tickFromPrecision(raw.DecimalPrecision)
	feeRate, feeKnown := feeRateFromMarket(raw)
	outcomes := make([]market.Outcome, 0, 2)
	for _, outcome := range raw.Outcomes[:2] {
		outcomes = append(outcomes, market.Outcome{
			Name:    strings.TrimSpace(outcome.Name),
			TokenID: strings.TrimSpace(outcome.OnChainID),
			Price:   outcomeMid(outcome, tick),
		})
	}

	liquidity, volume := 0.0, 0.0
	if raw.Stats != nil {
		liquidity = raw.Stats.LiquidityValue()
		volume = raw.Stats.VolumeValue()
	}

	status := strings.TrimSpace(raw.Status)
	item := market.Market{
		ID:               strings.TrimSpace(raw.ID.String()),
		ConditionID:      strings.TrimSpace(raw.ConditionID),
		Question:         firstNonEmpty(strings.TrimSpace(raw.Title), strings.TrimSpace(raw.Question)),
		EventTitle:       strings.TrimSpace(raw.Title),
		Outcomes:         outcomes,
		EndDate:          endsAt,
		Liquidity:        liquidity,
		Volume:           volume,
		Active:           strings.EqualFold(strings.TrimSpace(raw.TradingStatus), statusOpen),
		Closed:           isClosedStatus(status, raw.TradingStatus),
		ResolutionStatus: status,
		Venue:            market.VenuePredictFun,
		Currency:         market.CurrencyUSDT,
		TickSize:         tick,
		MinNotional:      defaultMinNotional,
		FeeRateBPS:       feeRate,
		NegRisk:          raw.IsNegRisk,
		Category:         humanizeCategory(raw.CategorySlug),
		// Predict.fun has no public market slug or page this build can link to,
		// so Slug and LinkURL stay empty rather than inventing a URL.
	}

	meta := marketMeta{
		MarketID:     item.ID,
		ConditionID:  item.ConditionID,
		Question:     item.Question,
		NegRisk:      raw.IsNegRisk,
		Tick:         tick,
		FeeRateBps:   feeRate,
		FeeRateKnown: feeKnown,
		MinNotional:  defaultMinNotional,
		EndDate:      endsAt,
	}
	if len(outcomes) == 2 {
		meta.YesTokenID = outcomes[0].TokenID
		meta.NoTokenID = outcomes[1].TokenID
	}
	p.remember(meta)
	return item, nil
}

// outcomeMid is the display price of one side: the mid when both sides quote,
// otherwise the only quoted side, otherwise zero. It is indicative only; a
// trade always prices from the orderbook at submit time.
func outcomeMid(outcome MarketOutcome, tick float64) float64 {
	decimals := 0
	if tick > 0 {
		decimals = priceDecimalsFor(tick)
	}
	bid := parsePrice(outcome.BestBid.Price, decimals)
	ask := parsePrice(outcome.BestAsk.Price, decimals)
	switch {
	case bid > 0 && ask > 0:
		return (bid + ask) / 2
	case bid > 0:
		return bid
	case ask > 0:
		return ask
	default:
		return 0
	}
}

// tickFromPrecision converts decimalPrecision to a price increment: precision 2
// means a 0.01 tick, precision 3 a 0.001 tick. An unparseable precision yields
// zero, which tells callers "not reported" instead of inventing 0.01 for a
// market that may not accept it.
func tickFromPrecision(precision Number) float64 {
	if strings.TrimSpace(precision.String()) == "" {
		return 0
	}
	value, err := precision.Int64()
	if err != nil || value < 0 || value > 9 {
		return 0
	}
	return math.Pow10(int(-value))
}

// feeRateFromMarket reports the market's taker fee rate and whether the field
// was present. A market that omits feeRateBps is not free; the executor refuses
// to price it rather than signing a fee of zero.
func feeRateFromMarket(raw Market) (float64, bool) {
	if strings.TrimSpace(raw.FeeRateBps.String()) == "" {
		return 0, false
	}
	rate, err := raw.FeeRateBps.Float64()
	if err != nil || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0, false
	}
	return rate, true
}

// isClosedStatus treats a resolved/closed lifecycle as terminal. TradingStatus
// CLOSED is also terminal even when the lifecycle field lags behind.
func isClosedStatus(status, tradingStatus string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "resolved", "closed", "settled", "finalized", "cancelled", "canceled":
		return true
	}
	return strings.EqualFold(strings.TrimSpace(tradingStatus), "CLOSED")
}

// categoryEndsAt resolves a category's endsAt with a per-slug cache. A slug that
// is missing from the payload is cached as "unknown" for a short TTL so one
// broken category cannot cause an HTTP request per market per scan.
func (p *Provider) categoryEndsAt(ctx context.Context, slug string) (time.Time, bool, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return time.Time{}, false, nil
	}
	now := p.now()
	p.mu.Lock()
	if entry, found := p.endsAt[slug]; found {
		ttl := p.ttl
		if !entry.ok {
			ttl = p.negativeTTL
		}
		if now.Sub(entry.fetchedAt) < ttl {
			p.mu.Unlock()
			return entry.endsAt, entry.ok, nil
		}
	}
	p.mu.Unlock()

	category, err := p.client.Category(ctx, slug)
	if err != nil {
		p.storeEndsAt(slug, time.Time{}, false, now)
		return time.Time{}, false, fmt.Errorf("读取 Predict.fun 分类 %s: %w", slug, err)
	}
	rawEndsAt := strings.TrimSpace(category.EndsAt)
	if rawEndsAt == "" {
		p.storeEndsAt(slug, time.Time{}, false, now)
		return time.Time{}, false, nil
	}
	endsAt, parseErr := time.Parse(time.RFC3339, rawEndsAt)
	if parseErr != nil || endsAt.IsZero() {
		p.storeEndsAt(slug, time.Time{}, false, now)
		return time.Time{}, false, fmt.Errorf("Predict.fun 分类 %s 的 endsAt 不是有效的 RFC3339 时间", slug)
	}
	endsAt = endsAt.UTC()
	p.storeEndsAt(slug, endsAt, true, now)
	return endsAt, true, nil
}

func (p *Provider) storeEndsAt(slug string, endsAt time.Time, ok bool, now time.Time) {
	p.mu.Lock()
	p.endsAt[slug] = endsAtEntry{endsAt: endsAt, ok: ok, fetchedAt: now}
	p.mu.Unlock()
}

// remember registers a market's trading metadata for the executor.
func (p *Provider) remember(meta marketMeta) {
	if meta.YesTokenID == "" && meta.NoTokenID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if meta.YesTokenID != "" {
		p.byToken[meta.YesTokenID] = meta
	}
	if meta.NoTokenID != "" {
		p.byToken[meta.NoTokenID] = meta
	}
	if meta.ConditionID != "" {
		p.byCondition[strings.ToLower(meta.ConditionID)] = meta
	}
}

func (p *Provider) lookupToken(tokenID string) (marketMeta, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	meta, ok := p.byToken[strings.TrimSpace(tokenID)]
	return meta, ok
}

func (p *Provider) lookupCondition(conditionID string) (marketMeta, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	meta, ok := p.byCondition[strings.ToLower(strings.TrimSpace(conditionID))]
	return meta, ok
}

// marketMeta resolves the trading metadata for a token (and optionally checks
// it against the condition id the caller passed). A miss triggers one bounded
// re-index of the open market list: after a restart the executor may be asked
// to trade before the scanner has run.
func (p *Provider) marketMeta(ctx context.Context, tokenID, conditionID string) (marketMeta, error) {
	if p == nil || p.client == nil {
		return marketMeta{}, fmt.Errorf("Predict.fun 行情未配置，无法确定市场元数据")
	}
	tokenID = strings.TrimSpace(tokenID)
	conditionID = strings.TrimSpace(conditionID)
	if tokenID == "" && conditionID == "" {
		return marketMeta{}, fmt.Errorf("缺少 token 与 condition，无法确定 Predict.fun 市场")
	}
	if meta, ok := p.resolveMeta(tokenID, conditionID); ok {
		return meta, nil
	}
	if err := p.indexRemote(ctx); err != nil {
		return marketMeta{}, err
	}
	if meta, ok := p.resolveMeta(tokenID, conditionID); ok {
		return meta, nil
	}
	label := tokenID
	if label == "" {
		label = conditionID
	}
	return marketMeta{}, fmt.Errorf("未找到 %s 对应的 Predict.fun 市场元数据，请先刷新行情", label)
}

func (p *Provider) resolveMeta(tokenID, conditionID string) (marketMeta, bool) {
	var (
		meta marketMeta
		ok   bool
	)
	switch {
	case tokenID != "":
		meta, ok = p.lookupToken(tokenID)
		if !ok && conditionID != "" {
			meta, ok = p.lookupCondition(conditionID)
		}
	case conditionID != "":
		meta, ok = p.lookupCondition(conditionID)
	}
	if !ok {
		return marketMeta{}, false
	}
	if tokenID != "" {
		if _, ok := meta.outcomeFor(tokenID); !ok {
			return marketMeta{}, false
		}
	}
	if conditionID != "" && meta.ConditionID != "" && !strings.EqualFold(meta.ConditionID, conditionID) {
		return marketMeta{}, false
	}
	return meta, true
}

// indexRemote re-reads the open market list (bounded) to rebuild the token
// index. It indexes trading metadata only; end dates stay unresolved until the
// scanner maps the market.
func (p *Provider) indexRemote(ctx context.Context) error {
	cursor := ""
	for page := 0; page < marketScanCap/maxPageSize; page++ {
		markets, next, err := p.client.Markets(ctx, MarketQuery{
			First:  maxPageSize,
			Cursor: cursor,
			Status: statusOpen,
		})
		if err != nil {
			if page == 0 {
				return fmt.Errorf("刷新 Predict.fun 市场元数据: %w", err)
			}
			return nil
		}
		for _, raw := range markets {
			meta, ok := metaFromMarket(raw)
			if ok {
				p.remember(meta)
			}
		}
		cursor = strings.TrimSpace(next)
		if cursor == "" {
			return nil
		}
	}
	return nil
}

// metaFromMarket builds trading metadata without resolving the end date, which
// keeps the re-index to the market list only.
func metaFromMarket(raw Market) (marketMeta, bool) {
	if raw.IsYieldBearing || len(raw.Outcomes) < 2 {
		return marketMeta{}, false
	}
	tick := tickFromPrecision(raw.DecimalPrecision)
	feeRate, feeKnown := feeRateFromMarket(raw)
	return marketMeta{
		MarketID:     strings.TrimSpace(raw.ID.String()),
		ConditionID:  strings.TrimSpace(raw.ConditionID),
		Question:     firstNonEmpty(strings.TrimSpace(raw.Title), strings.TrimSpace(raw.Question)),
		YesTokenID:   strings.TrimSpace(raw.Outcomes[0].OnChainID),
		NoTokenID:    strings.TrimSpace(raw.Outcomes[1].OnChainID),
		NegRisk:      raw.IsNegRisk,
		Tick:         tick,
		FeeRateBps:   feeRate,
		FeeRateKnown: feeKnown,
		MinNotional:  defaultMinNotional,
	}, true
}

// humanizeCategory turns a category slug into a display label. Predict.fun has
// no separate display name on the market resource, and the engine leaves
// Category empty when the provider cannot name one.
func humanizeCategory(slug string) string {
	trimmed := strings.TrimSpace(slug)
	if trimmed == "" {
		return ""
	}
	words := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	known := map[string]string{
		"ai":   "AI",
		"nft":  "NFT",
		"defi": "DeFi",
		"cex":  "CeFi",
		"dex":  "DEX",
	}
	for index, word := range words {
		if label, ok := known[strings.ToLower(word)]; ok {
			words[index] = label
			continue
		}
		runes := []rune(word)
		if len(runes) == 0 {
			continue
		}
		words[index] = strings.ToUpper(string(runes[0])) + string(runes[1:])
	}
	return strings.Join(words, " ")
}
