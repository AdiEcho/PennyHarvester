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

type GammaClient struct {
	mu             sync.RWMutex
	baseURL        string
	defaultBaseURL string
	proxy          *netproxy.Manager
	client         *http.Client
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
	return nil
}

func (g *GammaClient) httpClient() *http.Client {
	if g.proxy != nil {
		return g.proxy.HTTPClient(15 * time.Second)
	}
	return g.client
}

func (g *GammaClient) Name() string { return "Polymarket Gamma" }

type gammaMarket struct {
	ID                  string          `json:"id"`
	ConditionID         string          `json:"conditionId"`
	Question            string          `json:"question"`
	GroupItemTitle      string          `json:"groupItemTitle"`
	Slug                string          `json:"slug"`
	Outcomes            json.RawMessage `json:"outcomes"`
	OutcomePrices       json.RawMessage `json:"outcomePrices"`
	ClobTokenIDs        json.RawMessage `json:"clobTokenIds"`
	EndDate             string          `json:"endDate"`
	LiquidityNum        float64         `json:"liquidityNum"`
	VolumeNum           float64         `json:"volumeNum"`
	Liquidity           json.Number     `json:"liquidity"`
	Volume              json.Number     `json:"volume"`
	Active              bool            `json:"active"`
	Closed              bool            `json:"closed"`
	UmaResolutionStatus string          `json:"umaResolutionStatus"`
	Events              []struct {
		Slug  string `json:"slug"`
		Title string `json:"title"`
	} `json:"events"`
}

func (g *GammaClient) ListOpen(ctx context.Context, maxDays int) ([]Market, error) {
	now := time.Now().UTC()
	markets := make([]Market, 0, 100)
	for offset := 0; offset < 500; offset += 100 {
		query := url.Values{
			"active":       {"true"},
			"closed":       {"false"},
			"limit":        {"100"},
			"offset":       {strconv.Itoa(offset)},
			"order":        {"endDate"},
			"ascending":    {"true"},
			"end_date_min": {now.Format(time.RFC3339)},
			"end_date_max": {now.AddDate(0, 0, maxDays).Format(time.RFC3339)},
		}
		var raw []gammaMarket
		if err := g.getJSON(ctx, "/markets?"+query.Encode(), &raw); err != nil {
			return nil, err
		}
		for _, item := range raw {
			parsed, err := parseGammaMarket(item)
			if err == nil {
				markets = append(markets, parsed)
			}
		}
		if len(raw) < 100 {
			break
		}
	}
	return markets, nil
}

// MarketsLocale fetches a batch of markets with Polymarket's localized text.
// Gamma applies the locale to user-generated fields such as question and
// outcomes; ids are batched to stay within the API's identifier limit.
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
		var raw []gammaMarket
		if err := g.getJSON(ctx, "/markets?"+query.Encode(), &raw); err != nil {
			return nil, err
		}
		for _, item := range raw {
			parsed, err := parseGammaMarket(item)
			if err == nil {
				markets = append(markets, parsed)
			}
		}
	}
	return markets, nil
}

func (g *GammaClient) Get(ctx context.Context, id string) (Market, error) {
	var raw gammaMarket
	if err := g.getJSON(ctx, "/markets/"+url.PathEscape(id), &raw); err != nil {
		return Market{}, err
	}
	return parseGammaMarket(raw)
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
	var names, tokenIDs []string
	var prices []float64
	if err := decodeStringArray(raw.Outcomes, &names); err != nil {
		return Market{}, fmt.Errorf("解析 outcomes: %w", err)
	}
	if err := decodeStringArray(raw.ClobTokenIDs, &tokenIDs); err != nil {
		return Market{}, fmt.Errorf("解析 clobTokenIds: %w", err)
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
	if len(raw.Events) > 0 {
		if raw.Events[0].Slug != "" {
			slug = raw.Events[0].Slug
		}
		eventTitle = raw.Events[0].Title
	}
	return Market{
		ID: raw.ID, ConditionID: raw.ConditionID, Question: raw.Question,
		GroupItemTitle: raw.GroupItemTitle, EventTitle: eventTitle,
		Slug: slug, Outcomes: outcomes, EndDate: endDate,
		Liquidity: liquidity, Volume: volume, Active: raw.Active,
		Closed: raw.Closed, ResolutionStatus: raw.UmaResolutionStatus,
	}, nil
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
