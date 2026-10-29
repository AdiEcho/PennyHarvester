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
	"time"
)

const maxResponseBytes = 8 << 20

type GammaClient struct {
	baseURL string
	client  *http.Client
}

func NewGammaClient(baseURL string) *GammaClient {
	return &GammaClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *GammaClient) Name() string { return "Polymarket Gamma" }

type gammaMarket struct {
	ID                  string          `json:"id"`
	ConditionID         string          `json:"conditionId"`
	Question            string          `json:"question"`
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
		Slug string `json:"slug"`
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

func (g *GammaClient) Get(ctx context.Context, id string) (Market, error) {
	var raw gammaMarket
	if err := g.getJSON(ctx, "/markets/"+url.PathEscape(id), &raw); err != nil {
		return Market{}, err
	}
	return parseGammaMarket(raw)
}

func (g *GammaClient) getJSON(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("创建行情请求: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PennyHarvester/0.1 (public-data paper-mode)")
	resp, err := g.client.Do(req)
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
	if len(raw.Events) > 0 && raw.Events[0].Slug != "" {
		slug = raw.Events[0].Slug
	}
	return Market{
		ID: raw.ID, ConditionID: raw.ConditionID, Question: raw.Question,
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
