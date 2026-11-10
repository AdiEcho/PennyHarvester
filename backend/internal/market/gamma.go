package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"pennyharvester/backend/internal/netproxy"
)

const maxResponseBytes = 8 << 20

// Broad views (volume and liquidity) describe the whole book and change far
// more slowly than the near-end view. Reusing them for a minute removes four of
// the six requests a steady-state scan used to make, which is what lets a 10s
// cadence fit inside the upstream latency.
const (
	broadViewTTL        = 60 * time.Second
	maxViewCacheEntries = 64
	// One view has at most two sequential keyset pages. A cap below the scan
	// budget keeps a stalled upstream from consuming the whole cycle; the
	// near-end view then retries on the next tick instead of blocking it.
	viewTimeout = 5 * time.Second
	// The tail view reads events rather than markets, so it carries every
	// market nested inside an event. Its payload is larger and its pages are
	// the ones that must survive a busy settlement boundary.
	tailViewTimeout = 8 * time.Second
	// tailHorizon bounds the tail view. Every settlement boundary creates a
	// tie group of hundreds of markets that share one endDate (sports props and
	// exact-score lines), and the 5-minute crypto up/down markets settle at the
	// same instant. Ordering markets by endDate therefore lets that crowd fill
	// every page of the near-end view, which is why the tail view reads events
	// ordered by endDate: the same crowd is a few dozen events, and the crypto
	// windows stay on page one.
	tailHorizon = 20 * time.Minute
	tailPages   = 4
	// An event page carries every nested market, so a page of 100 events can
	// exceed the 8MB response cap at a settlement boundary. Half that keeps each
	// page well under the cap while four pages still cover the horizon.
	tailPageSize = 50
)

type gammaView struct {
	order     string
	ascending string
	pages     int
	cache     bool
	// events reads /events/keyset and keeps the nested markets. Only the tail
	// view sets it, because that is the only view that must survive an endDate
	// tie group crowding out the markets it was built to find.
	events bool
	// horizon, when set, replaces the maxDays end_date_max so the view covers a
	// fixed window instead of the whole radar horizon.
	horizon time.Duration
}

type gammaViewCache struct {
	at      time.Time
	markets []Market
}

type GammaClient struct {
	mu             sync.RWMutex
	baseURL        string
	defaultBaseURL string
	proxy          *netproxy.Manager
	client         *http.Client

	cacheMu   sync.Mutex
	viewCache map[string]gammaViewCache
}

// NewGammaClient keeps the public Gamma API on the outbound egress configured
// in the system settings. A nil manager keeps the Go default behaviour, which
// is what the tests use. The base URL can be switched at runtime from the web
// UI; the startup value remains the fallback when the override is cleared.
func NewGammaClient(baseURL string, proxy *netproxy.Manager) *GammaClient {
	normalized := normalizeBaseURL(baseURL)
	return &GammaClient{
		baseURL:        normalized,
		defaultBaseURL: normalized,
		proxy:          proxy,
		client:         &http.Client{Timeout: 15 * time.Second},
		viewCache:      make(map[string]gammaViewCache),
	}
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func (g *GammaClient) BaseURL() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.baseURL
}

func (g *GammaClient) DefaultBaseURL() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.defaultBaseURL
}

// SetBaseURL applies a web-managed override. An empty value restores the
// startup URL.
func (g *GammaClient) SetBaseURL(raw string) error {
	normalized := normalizeBaseURL(raw)
	if normalized == "" {
		normalized = g.DefaultBaseURL()
	}
	if normalized != "" {
		parsed, err := url.Parse(normalized)
		if err != nil {
			return fmt.Errorf("Gamma 行情地址无效: %w", err)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
		default:
			return fmt.Errorf("Gamma 行情地址仅支持 http 或 https")
		}
		if parsed.Host == "" {
			return fmt.Errorf("Gamma 行情地址缺少主机与端口")
		}
	}
	g.mu.Lock()
	g.baseURL = normalized
	g.mu.Unlock()
	g.cacheMu.Lock()
	g.viewCache = make(map[string]gammaViewCache)
	g.cacheMu.Unlock()
	return nil
}

func (g *GammaClient) httpClient() *http.Client {
	if g.proxy != nil {
		return g.proxy.HTTPClient(15 * time.Second)
	}
	return g.client
}

func (g *GammaClient) Name() string { return "Polymarket Gamma" }

// Venue reports which exchange this provider reads. Every market this client
// returns is a Polymarket market, which is what lets the venue registry route
// it without a per-call argument.
func (g *GammaClient) Venue() Venue { return VenuePolymarket }

type gammaMarket struct {
	ID             string          `json:"id"`
	ConditionID    string          `json:"conditionId"`
	Question       string          `json:"question"`
	GroupItemTitle string          `json:"groupItemTitle"`
	Slug           string          `json:"slug"`
	Outcomes       json.RawMessage `json:"outcomes"`
	OutcomePrices  json.RawMessage `json:"outcomePrices"`
	// Version selects which outcome identifier field is authoritative: "v1"
	// (CTF) uses clobTokenIds, "v2" (Polymarket Protocol V2) uses positionIds.
	// A market can populate both, so the version decides, not the presence.
	Version       string          `json:"version"`
	ClobTokenIDs  json.RawMessage `json:"clobTokenIds"`
	PositionIDs   json.RawMessage `json:"positionIds"`
	EndDate       string          `json:"endDate"`
	GameStartTime string          `json:"gameStartTime"`
	// ResolutionStatus is the V2 settlement lifecycle (inactive/active/resolved);
	// V1 markets report the UMA proposal instead.
	ResolutionStatus    string      `json:"resolutionStatus"`
	UmaResolutionStatus string      `json:"umaResolutionStatus"`
	OneDayPriceChange   *float64    `json:"oneDayPriceChange"`
	Tags                []Tag       `json:"tags"`
	LiquidityNum        float64     `json:"liquidityNum"`
	VolumeNum           float64     `json:"volumeNum"`
	Liquidity           json.Number `json:"liquidity"`
	Volume              json.Number `json:"volume"`
	Active              bool        `json:"active"`
	Closed              bool        `json:"closed"`
	// AcceptingOrders says whether the book is open for new orders. A market can
	// stay active and unclosed while orders are paused, so this is the field to
	// gate trading on.
	AcceptingOrders *bool `json:"acceptingOrders"`
	// NegRisk and OrderPriceMinTickSize are venue metadata the trading layer
	// needs: negative-risk markets settle through a different flow, and the
	// tick size decides how a price must be rounded before signing.
	NegRisk               bool            `json:"negRisk"`
	OrderPriceMinTickSize *float64        `json:"orderPriceMinTickSize"`
	Events                []gammaEventRef `json:"events"`
}

// gammaEventRef is the event stamp on a market. A market read through
// /events/keyset carries no events field, so the tail view re-stamps it from
// its parent event; the engine needs the event ID for tag lookups and the
// event slug is what the archive keys markets on.
type gammaEventRef struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

// gammaEvent is the subset of /events/keyset this client reads. Nested markets
// are the reason the tail view exists: one row per event instead of one row per
// market keeps a settlement boundary from crowding out the crypto windows.
type gammaEvent struct {
	ID      string        `json:"id"`
	Slug    string        `json:"slug"`
	Title   string        `json:"title"`
	Tags    []Tag         `json:"tags"`
	Markets []gammaMarket `json:"markets"`
}

type gammaEventTags struct {
	ID   string `json:"id"`
	Tags []Tag  `json:"tags"`
}

func (g *GammaClient) ListOpen(ctx context.Context, maxDays int) ([]Market, error) {
	now := time.Now().UTC()
	endMin := now.Format(time.RFC3339)
	endMax := now.AddDate(0, 0, maxDays).Format(time.RFC3339)

	// Polymarket's earliest-ending markets are overwhelmingly sports games. A
	// single end-date-ordered scan can therefore return no politics, crypto or
	// finance candidates at all. Read a few complementary views and merge them:
	// near-end markets for the tail strategy, plus the most active and deepest
	// books for cross-category coverage. Tags are included in each payload so
	// classification does not need one request per event.
	//
	// The near-end view must be fresh on every scan. The two broad views only
	// provide cross-category coverage, so they are cached for broadViewTTL and
	// fetched concurrently with the near-end view. A cold scan still visits all
	// six pages; a warm scan visits only the near-end pages plus the tail view.
	views := []gammaView{
		{order: "endDate", ascending: "true", pages: 2},
		{order: "endDate", ascending: "true", pages: tailPages, horizon: tailHorizon, events: true},
		{order: "volume24hr", ascending: "false", pages: 2, cache: true},
		{order: "liquidity", ascending: "false", pages: 2, cache: true},
	}

	baseURL := g.BaseURL()
	cacheKey := func(view gammaView) string {
		return fmt.Sprintf("%s|%d|%s|%s|%d|%s", baseURL, maxDays, view.order, view.ascending, view.pages, view.horizon)
	}

	type viewResult struct {
		items []Market
		err   error
	}
	results := make([]viewResult, len(views))
	var wait sync.WaitGroup
	for i, view := range views {
		if view.cache {
			if items, ok := g.cachedView(cacheKey(view), broadViewTTL); ok {
				results[i] = viewResult{items: items}
				continue
			}
		}
		wait.Add(1)
		go func(index int, view gammaView) {
			defer wait.Done()
			viewEnd := endMax
			if view.horizon > 0 {
				viewEnd = now.Add(view.horizon).Format(time.RFC3339)
			}
			timeout := viewTimeout
			if view.events {
				timeout = tailViewTimeout
			}
			viewCtx, cancel := context.WithTimeout(ctx, timeout)
			var items []Market
			var err error
			if view.events {
				items, err = g.listEventMarkets(viewCtx, endMin, viewEnd, view.order, view.ascending, view.pages)
			} else {
				items, err = g.listMarkets(viewCtx, endMin, viewEnd, view.order, view.ascending, view.pages)
			}
			cancel()
			if err != nil && view.cache {
				// A failed broad refresh must not blank the radar. Use the last
				// good copy if one exists; the near-end view still reports fresh
				// trading candidates.
				if stale, ok := g.cachedView(cacheKey(view), 0); ok {
					items, err = stale, nil
				}
			}
			if err == nil && view.cache {
				g.storeView(cacheKey(view), items)
			}
			results[index] = viewResult{items: items, err: err}
		}(i, view)
	}
	wait.Wait()

	seen := make(map[string]bool)
	markets := make([]Market, 0, 800)
	var failures []string
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.err.Error())
			continue
		}
		for _, item := range result.items {
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			markets = append(markets, item)
		}
	}
	if len(markets) == 0 && len(failures) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return markets, nil
}

// cachedView returns a cached view. A zero ttl accepts a stale entry, which is
// the fallback used when a broad refresh fails.
func (g *GammaClient) cachedView(key string, ttl time.Duration) ([]Market, bool) {
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	entry, ok := g.viewCache[key]
	if !ok {
		return nil, false
	}
	if ttl > 0 && time.Since(entry.at) > ttl {
		return nil, false
	}
	return entry.markets, true
}

func (g *GammaClient) storeView(key string, markets []Market) {
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	if len(g.viewCache) >= maxViewCacheEntries {
		g.viewCache = make(map[string]gammaViewCache)
	}
	g.viewCache[key] = gammaViewCache{at: time.Now(), markets: markets}
}

// listMarkets pages one Gamma view with /markets/keyset. Polymarket replaced
// offset paging with opaque cursors (GET /markets/keyset, 2026-04) and rejects
// offset on the keyset route, so this walks next_cursor instead. The server
// ignores `active`, which is why ListOpen does not send it; the engine filters
// on the parsed Active flag.
func (g *GammaClient) listMarkets(ctx context.Context, endMin, endMax, order, ascending string, pages int) ([]Market, error) {
	markets := make([]Market, 0, pages*100)
	cursor := ""
	for page := 0; page < pages; page++ {
		query := url.Values{
			"closed":       {"false"},
			"limit":        {"100"},
			"order":        {order},
			"ascending":    {ascending},
			"include_tag":  {"true"},
			"end_date_min": {endMin},
			"end_date_max": {endMax},
		}
		if cursor != "" {
			query.Set("after_cursor", cursor)
		}
		var response struct {
			Markets    []gammaMarket `json:"markets"`
			NextCursor string        `json:"next_cursor"`
		}
		if err := g.getJSON(ctx, "/markets/keyset?"+query.Encode(), &response); err != nil {
			// Page one is still useful when the cursor page stalls. Returning
			// the parsed rows keeps the near-end view populated for this scan.
			if len(markets) > 0 {
				return markets, nil
			}
			return nil, err
		}
		for _, item := range response.Markets {
			if parsed, err := parseGammaMarket(item); err == nil {
				markets = append(markets, parsed)
			}
		}
		if response.NextCursor == "" || response.NextCursor == cursor || len(response.Markets) == 0 {
			break
		}
		cursor = response.NextCursor
	}
	return markets, nil
}

// listEventMarkets pages /events/keyset and keeps the markets nested in each
// event. Polymarket sorts markets by endDate alone, so at a settlement
// boundary the first pages are hundreds of sports props that all end at the same
// instant; the crypto up/down windows that settle then are pushed past the page
// cap and the tail strategy never sees them. Grouping by event first turns that
// crowd into a few dozen rows, so the same window stays on page one.
//
// A market nested in an event omits both its events stamp and its tags, so the
// parent event supplies them before parsing. Without the stamp the engine
// cannot resolve event tags, and without the tags the crypto window falls back
// to the much wider default entry window.
func (g *GammaClient) listEventMarkets(ctx context.Context, endMin, endMax, order, ascending string, pages int) ([]Market, error) {
	markets := make([]Market, 0, pages*200)
	cursor := ""
	for page := 0; page < pages; page++ {
		query := url.Values{
			"closed":       {"false"},
			"limit":        {strconv.Itoa(tailPageSize)},
			"order":        {order},
			"ascending":    {ascending},
			"end_date_min": {endMin},
			"end_date_max": {endMax},
		}
		if cursor != "" {
			query.Set("after_cursor", cursor)
		}
		var response struct {
			Events     []gammaEvent `json:"events"`
			NextCursor string       `json:"next_cursor"`
		}
		if err := g.getJSON(ctx, "/events/keyset?"+query.Encode(), &response); err != nil {
			// Page one is still useful when the cursor page stalls, exactly as in
			// listMarkets: keep whatever arrived rather than blanking the view.
			if len(markets) > 0 {
				return markets, nil
			}
			return nil, err
		}
		for _, event := range response.Events {
			stamp := gammaEventRef{ID: event.ID, Slug: event.Slug, Title: event.Title}
			for _, item := range event.Markets {
				if len(item.Events) == 0 {
					item.Events = []gammaEventRef{stamp}
				}
				if len(item.Tags) == 0 {
					item.Tags = event.Tags
				}
				if parsed, err := parseGammaMarket(item); err == nil {
					markets = append(markets, parsed)
				}
			}
		}
		if response.NextCursor == "" || response.NextCursor == cursor || len(response.Events) == 0 {
			break
		}
		cursor = response.NextCursor
	}
	return markets, nil
}

// MarketsLocale fetches a batch of markets with Polymarket's localized text.
// Gamma applies the locale to user-generated fields such as question and
// outcomes. locale is documented on /markets/keyset (not on /markets), and ids
// are batched to stay within the 100-identifier limit.
func (g *GammaClient) MarketsLocale(ctx context.Context, ids []string, locale string) ([]Market, error) {
	locale = strings.TrimSpace(locale)
	if locale == "" || len(ids) == 0 {
		return nil, nil
	}
	markets := make([]Market, 0, len(ids))
	for start := 0; start < len(ids); start += 100 {
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		query := url.Values{"locale": {locale}, "limit": {"100"}}
		for _, id := range ids[start:end] {
			if id != "" {
				query.Add("id", id)
			}
		}
		if len(query["id"]) == 0 {
			continue
		}
		var response struct {
			Markets []gammaMarket `json:"markets"`
		}
		if err := g.getJSON(ctx, "/markets/keyset?"+query.Encode(), &response); err != nil {
			return nil, err
		}
		for _, item := range response.Markets {
			if parsed, err := parseGammaMarket(item); err == nil {
				markets = append(markets, parsed)
			}
		}
	}
	return markets, nil
}

func (g *GammaClient) Get(ctx context.Context, id string) (Market, error) {
	var raw gammaMarket
	if err := g.getJSON(ctx, "/markets/"+url.PathEscape(id)+"?include_tag=true", &raw); err != nil {
		return Market{}, err
	}
	return parseGammaMarket(raw)
}

// EventTags resolves Polymarket event tags in batches. The engine uses tags to
// classify opportunities and to identify sports games whose result can change
// in overtime. The endpoint returns full event objects, so callers should only
// ask for the events behind opportunities that already passed the price and
// liquidity filters.
func (g *GammaClient) EventTags(ctx context.Context, eventIDs []string) (map[string][]Tag, error) {
	unique := make([]string, 0, len(eventIDs))
	seen := make(map[string]bool, len(eventIDs))
	for _, id := range eventIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	result := make(map[string][]Tag, len(unique))
	const batchSize = 10
	for start := 0; start < len(unique); start += batchSize {
		end := start + batchSize
		if end > len(unique) {
			end = len(unique)
		}
		query := url.Values{"limit": {strconv.Itoa(end - start)}}
		for _, id := range unique[start:end] {
			query.Add("id", id)
		}
		var raw []gammaEventTags
		if err := g.getJSON(ctx, "/events?"+query.Encode(), &raw); err != nil {
			return nil, err
		}
		for _, item := range raw {
			result[item.ID] = item.Tags
		}
	}
	return result, nil
}

func (g *GammaClient) getJSON(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL()+path, nil)
	if err != nil {
		return fmt.Errorf("创建行情请求: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PennyHarvester/0.1 (public-data paper-mode)")
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("读取 Polymarket 公开行情: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Polymarket Gamma 返回 %s", resp.Status)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("解析公开行情: %w", err)
	}
	return nil
}

func parseGammaMarket(raw gammaMarket) (Market, error) {
	var names []string
	var prices []float64
	if err := decodeStringArray(raw.Outcomes, &names); err != nil {
		return Market{}, fmt.Errorf("解析 outcomes: %w", err)
	}
	tokenIDs, err := gammaOutcomeIDs(raw)
	if err != nil {
		return Market{}, err
	}
	if err := decodeFloatArray(raw.OutcomePrices, &prices); err != nil {
		return Market{}, fmt.Errorf("解析 outcomePrices: %w", err)
	}
	if len(names) == 0 || len(names) != len(prices) || len(names) != len(tokenIDs) {
		return Market{}, fmt.Errorf("市场 %s 的结果字段长度不一致", raw.ID)
	}
	endDate, err := time.Parse(time.RFC3339, raw.EndDate)
	if err != nil {
		return Market{}, fmt.Errorf("市场 %s 的结束时间无效: %w", raw.ID, err)
	}

	// V2 markets report a settlement lifecycle in resolutionStatus and leave
	// umaResolutionStatus empty; V1 markets do the opposite. Prefer whichever
	// field the market actually carries.
	resolutionStatus := strings.TrimSpace(raw.ResolutionStatus)
	if resolutionStatus == "" {
		resolutionStatus = raw.UmaResolutionStatus
	}

	outcomes := make([]Outcome, len(names))
	for i := range names {
		outcomes[i] = Outcome{Name: names[i], TokenID: tokenIDs[i], Price: prices[i]}
	}
	liquidity := raw.LiquidityNum
	if liquidity == 0 && raw.Liquidity != "" {
		liquidity, _ = raw.Liquidity.Float64()
	}
	volume := raw.VolumeNum
	if volume == 0 && raw.Volume != "" {
		volume, _ = raw.Volume.Float64()
	}
	slug := raw.Slug
	eventTitle := ""
	eventID := ""
	if len(raw.Events) > 0 {
		eventID = raw.Events[0].ID
		if raw.Events[0].Slug != "" {
			slug = raw.Events[0].Slug
		}
		eventTitle = raw.Events[0].Title
	}
	return Market{
		ID: raw.ID, ConditionID: raw.ConditionID, Question: raw.Question,
		GroupItemTitle: raw.GroupItemTitle, EventTitle: eventTitle, EventID: eventID,
		Slug: slug, Outcomes: outcomes, EndDate: endDate, Tags: raw.Tags,
		GameStartTime: parseGammaTime(raw.GameStartTime), PriceChange24h: raw.OneDayPriceChange,
		Liquidity: liquidity, Volume: volume, Active: raw.Active,
		Closed: raw.Closed, ResolutionStatus: resolutionStatus,
		Version: strings.ToLower(strings.TrimSpace(raw.Version)), AcceptingOrders: raw.AcceptingOrders,
		Venue: VenuePolymarket, Currency: CurrencyUSDC, NegRisk: raw.NegRisk,
		TickSize: gammaTickSize(raw.OrderPriceMinTickSize), MinNotional: 1,
	}, nil
}

// gammaTickSize clamps the reported tick to the values the CLOB actually
// accepts. An unreported or nonsensical tick yields 0 so callers fall back to
// their own default instead of signing an unreprensentable price.
func gammaTickSize(raw *float64) float64 {
	if raw == nil || *raw <= 0 || *raw > 0.1 {
		return 0
	}
	return *raw
}

// gammaOutcomeIDs resolves the trading identifier Gamma publishes for each
// outcome. Polymarket Protocol V2 markets trade position IDs and set
// clobTokenIds to null, while CTF markets publish a JSON-encoded clobTokenIds.
// Version decides which field to read because a market can carry both; treating
// presence as the selector would mix a V2 position ID with a CTF token ID.
func gammaOutcomeIDs(raw gammaMarket) ([]string, error) {
	version := strings.ToLower(strings.TrimSpace(raw.Version))
	switch version {
	case "v2":
		return decodeOutcomeIDs(raw.PositionIDs, "positionIds")
	case "v1":
		return decodeOutcomeIDs(raw.ClobTokenIDs, "clobTokenIds")
	case "":
		// Older mirrors do not send version. Keep working with whichever field
		// they populated, preferring the CTF field.
		ids, err := decodeOutcomeIDs(raw.ClobTokenIDs, "clobTokenIds")
		if err != nil || len(ids) > 0 {
			return ids, err
		}
		return decodeOutcomeIDs(raw.PositionIDs, "positionIds")
	default:
		return nil, fmt.Errorf("市场 %s 的协议版本 %q 不受支持", raw.ID, raw.Version)
	}
}

func decodeOutcomeIDs(raw json.RawMessage, field string) ([]string, error) {
	var ids []string
	if err := decodeStringArray(raw, &ids); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", field, err)
	}
	return ids, nil
}

func parseGammaTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05Z07",
		"2006-01-02 15:04:05Z07:00",
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return &parsed
		}
	}
	return nil
}

// Gamma has returned both JSON arrays and JSON-encoded arrays for these fields.
func decodeStringArray(raw json.RawMessage, target *[]string) error {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		return json.Unmarshal(raw, target)
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return err
	}
	return json.Unmarshal([]byte(encoded), target)
}

func decodeFloatArray(raw json.RawMessage, target *[]float64) error {
	var values []json.Number
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
	} else {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(encoded), &values); err != nil {
			return err
		}
	}
	result := make([]float64, len(values))
	for i, value := range values {
		parsed, err := value.Float64()
		if err != nil {
			return err
		}
		result[i] = parsed
	}
	*target = result
	return nil
}
