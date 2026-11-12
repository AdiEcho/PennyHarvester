package live

// Authenticated CLOB reads that the third-party client does not cover.
//
// Two documented responses cannot be read through the SDK's structs:
//
//   - GET /balance-allowance now answers with `allowances`, a map of spender
//     address to amount, because a wallet has one approval per exchange
//     (CTFExchange for CTF markets, ExchangeV3 for Polymarket Protocol V2). The
//     legacy scalar `allowance` field is gone from the spec, so reading it
//     alone reports zero and silently disables the pre-trade allowance guard.
//   - GET /data/order/{orderID} returns the order's limit price, not the price
//     its matches actually executed at. The per-fill rows live in /data/trades.
//
// Both endpoints require the same L2 HMAC authentication the CLOB client uses,
// which is implemented here so the values can be decoded in full.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// defaultCLOBURL mirrors the public host so a Config built without a URL still
// produces an actionable error instead of an empty one.
const defaultCLOBURL = "https://clob.polymarket.com"

// maxL2ResponseBytes bounds an authenticated read.
const maxL2ResponseBytes = 4 << 20

// maxTradePages bounds how many /data/trades pages a single reconciliation may
// walk. Exceeding the cap makes the result incomplete; the absolute number is a
// cost bound, not an assumption about how many fills one order may have.
const maxTradePages = 5

// tradeEndCursor is the CLOB pagination sentinel (base64 of "-1"). Sending it
// back is rejected as an invalid cursor, so pagination must stop on it instead
// of requesting one more page.
const tradeEndCursor = "LTE="

// clobHTTPError keeps the HTTP status of a failed CLOB call visible so callers
// can distinguish "this order does not exist" (404) from a real failure.
type clobHTTPError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

func (e *clobHTTPError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	if body == "" {
		return fmt.Sprintf("Polymarket CLOB 返回 HTTP %d（%s %s）", e.StatusCode, e.Method, e.Path)
	}
	return fmt.Sprintf("Polymarket CLOB 返回 HTTP %d（%s %s）: %s", e.StatusCode, e.Method, e.Path, body)
}

// l2Get performs an L2-authenticated CLOB GET.
//
// The signature covers timestamp + method + path and every header value comes
// from the credentials the client already derived, so this cannot drift from
// how orders authenticate: an L2 secret that signs an order signs this too. The
// path is signed without its query string, matching the CLOB client.
func (e *Executor) l2Get(ctx context.Context, path string, query url.Values, target any) error {
	if e == nil {
		return fmt.Errorf("未配置实盘执行器")
	}
	// ensure() derives the L2 credentials when they are not configured, and
	// validates that the private key and funder belong together.
	if _, err := e.ensure(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	credentials := e.credentials
	cfg := e.cfg
	e.mu.Unlock()
	if credentials == nil || credentials.APIKey == "" || credentials.Secret == "" || credentials.Passphrase == "" {
		return fmt.Errorf("缺少 CLOB L2 API 凭据，无法读取账户数据")
	}
	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(cfg.PrivateKey), "0x"))
	if err != nil {
		return fmt.Errorf("POLYMARKET_PRIVATE_KEY 无效: %w", err)
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.CLOBURL), "/")
	if base == "" {
		base = defaultCLOBURL
	}
	endpoint := base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	timestamp := time.Now().Unix()
	signature, err := l2Signature(credentials.Secret, timestamp, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("创建 CLOB 请求: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("POLY_ADDRESS", crypto.PubkeyToAddress(privateKey.PublicKey).Hex())
	request.Header.Set("POLY_SIGNATURE", signature)
	request.Header.Set("POLY_TIMESTAMP", strconv.FormatInt(timestamp, 10))
	request.Header.Set("POLY_API_KEY", credentials.APIKey)
	request.Header.Set("POLY_PASSPHRASE", credentials.Passphrase)

	client := guardedHTTPClient(20*time.Second, cfg.Proxy)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("请求 Polymarket CLOB: %w", err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxL2ResponseBytes))
	if readErr != nil {
		return fmt.Errorf("读取 Polymarket CLOB 响应: %w", readErr)
	}
	if response.StatusCode != http.StatusOK {
		return &clobHTTPError{StatusCode: response.StatusCode, Method: http.MethodGet, Path: path, Body: string(body)}
	}
	if target == nil {
		return nil
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("解析 %s 响应: %w", path, err)
	}
	return nil
}

// l2Signature reproduces the CLOB L2 signature: base64url(HMAC-SHA256(secret,
// timestamp + method + path + body)). The secret itself is base64url encoded by
// the exchange.
func l2Signature(secret string, timestamp int64, method, path string, body []byte) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(secret)
	}
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(secret)
	}
	if err != nil {
		return "", fmt.Errorf("解码 CLOB API Secret: %w", err)
	}
	mac := hmac.New(sha256.New, decoded)
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10) + method + path))
	_, _ = mac.Write(body)
	return base64.URLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// collateralUnits parses a fixed-point collateral amount. The CLOB reports
// balances and allowances in 6-decimal base units.
func collateralUnits(raw json.Number) (float64, error) {
	trimmed := strings.TrimSpace(raw.String())
	if trimmed == "" {
		return 0, nil
	}
	units, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("解析金额 %q: %w", trimmed, err)
	}
	if units < 0 {
		return 0, fmt.Errorf("金额不能为负: %q", trimmed)
	}
	return units / 1_000_000, nil
}

// maxAllowance picks the largest approval the wallet granted. The relevant
// spender is the exchange that settles the trade, and a wallet only ever
// approves the exchanges it trades on, so the maximum answers "can this wallet
// spend that much collateral" without hard-coding per-protocol contract
// addresses.
func maxAllowance(allowances map[string]json.Number) (float64, error) {
	largest := 0.0
	for spender, raw := range allowances {
		value, err := collateralUnits(raw)
		if err != nil {
			return 0, fmt.Errorf("spender %s: %w", spender, err)
		}
		if value > largest {
			largest = value
		}
	}
	return largest, nil
}

// clobTrade is one fill row of GET /data/trades.
type clobTrade struct {
	ID           string `json:"id"`
	TakerOrderID string `json:"taker_order_id"`
	Price        string `json:"price"`
	Size         string `json:"size"`
	Status       string `json:"status"`
}

// tradeLegStatus is the finality of one trade row. MATCHED and MINED are
// intermediate states: a match can still be retried, mined or failed, so only
// CONFIRMED is booked. An unrecognized status is treated as pending on purpose;
// it is not proof of a fill.
type tradeLegStatus int

const (
	tradeLegPending tradeLegStatus = iota
	tradeLegConfirmed
	tradeLegFailed
)

// normalizeTradeStatus maps the REST and OpenAPI spellings of a trade status
// onto its finality. The exchange prefixes some responses with TRADE_STATUS_;
// stripping it keeps TRADE_STATUS_FAILED and FAILED the same failed leg. The
// CLOB trade enum has one successful terminal state, CONFIRMED; MATCHED, MINED,
// RETRYING and unknown values are non-final.
func normalizeTradeStatus(raw string) tradeLegStatus {
	status := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(raw)), "TRADE_STATUS_")
	switch {
	case status == "CONFIRMED":
		return tradeLegConfirmed
	case strings.HasPrefix(status, "FAILED"):
		return tradeLegFailed
	default:
		return tradeLegPending
	}
}

// orderTrades is the evidence gathered from /data/trades for one order. Only
// confirmed legs contribute shares and notional; failed and pending legs are
// counted so an incomplete fill set can be surfaced instead of booked.
type orderTrades struct {
	ExpectedShares    float64
	ConfirmedShares   float64
	ConfirmedNotional float64
	ConfirmedLegs     int
	ConfirmedLevels   []fillLevel
	PendingLegs       int
	FailedLegs        int
	DuplicateLegs     int
	Pages             int
	Complete          bool
}

// reconciliationError describes why the evidence cannot be booked as a final
// fill. It includes the confirmed/expected quantities and the non-final leg
// counts so callers and operators can see what is missing.
func (t orderTrades) reconciliationError() error {
	details := []string{fmt.Sprintf("已确认 %.8g/%.8g 份", t.ConfirmedShares, t.ExpectedShares)}
	if t.PendingLegs > 0 {
		details = append(details, fmt.Sprintf("未终态 %d 笔", t.PendingLegs))
	}
	if t.FailedLegs > 0 {
		details = append(details, fmt.Sprintf("失败 %d 笔", t.FailedLegs))
	}
	if t.DuplicateLegs > 0 {
		details = append(details, fmt.Sprintf("重复 %d 笔已去重", t.DuplicateLegs))
	}
	return fmt.Errorf("成交记录不完整：%s", strings.Join(details, "，"))
}

// tradesReconcile compares the confirmed shares with the exchange's matched
// quantity, allowing only floating-point noise between decimal strings.
func tradesReconcile(confirmed, expected float64) bool {
	return math.Abs(confirmed-expected) <= 1e-9*math.Max(1, math.Abs(expected))
}

// makerAddress is the wallet that owns the CLOB position: the funder for proxy,
// Safe and deposit accounts and the signer for an EOA.
func (e *Executor) makerAddress() string {
	if maker := strings.TrimSpace(e.Maker()); maker != "" {
		return maker
	}
	return strings.TrimSpace(e.Signer())
}

// tradesForOrder enumerates the fills of one order from GET /data/trades and
// keeps only CONFIRMED legs.
//
// This is what reconciliation needs: /data/order only reports the order's limit
// price, so booking size_matched × price charges a FAK buy at its worst allowed
// price instead of the price its matches actually paid. The whole cursor chain
// is followed because one order's fills can span pages; stopping at the first
// page that mentions the order permanently loses the rest of the fill. Every
// page, cap or malformed-row failure returns an error alongside a result whose
// Complete flag stays false, so the caller never books partial evidence as
// final. Repeated trade IDs are counted once; identical repeats are harmless,
// conflicting repeats cannot be proven and fail the reconciliation.
func (e *Executor) tradesForOrder(ctx context.Context, orderID, conditionID string, expectedShares float64) (orderTrades, error) {
	result := orderTrades{ExpectedShares: expectedShares}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return result, nil
	}
	query := url.Values{}
	if conditionID != "" {
		query.Set("market", conditionID)
	}
	if maker := e.makerAddress(); maker != "" {
		query.Set("maker_address", maker)
	}
	seen := make(map[string]clobTrade)
	cursor := ""
	for {
		if result.Pages >= maxTradePages {
			return result, fmt.Errorf("成交记录超过 %d 页上限，无法确认完整成交", maxTradePages)
		}
		if cursor != "" {
			query.Set("next_cursor", cursor)
		}
		var response struct {
			Data       []clobTrade `json:"data"`
			NextCursor string      `json:"next_cursor"`
		}
		if err := e.l2Get(ctx, "/data/trades", query, &response); err != nil {
			return result, fmt.Errorf("读取订单成交（第 %d 页）: %w", result.Pages+1, err)
		}
		result.Pages++
		for _, trade := range response.Data {
			if !strings.EqualFold(strings.TrimSpace(trade.TakerOrderID), orderID) {
				continue
			}
			id := strings.TrimSpace(trade.ID)
			if id == "" {
				return result, fmt.Errorf("订单 %s 的成交缺少 ID，无法去重并确认完整成交", orderID)
			}
			if previous, ok := seen[id]; ok {
				result.DuplicateLegs++
				if normalizeTradeStatus(previous.Status) != normalizeTradeStatus(trade.Status) ||
					parseAmount(previous.Size) != parseAmount(trade.Size) ||
					parseAmount(previous.Price) != parseAmount(trade.Price) {
					return result, fmt.Errorf("成交 %s 重复出现且内容冲突，无法确认完整成交", id)
				}
				continue
			}
			seen[id] = trade
			size, price := parseAmount(trade.Size), parseAmount(trade.Price)
			if size <= 0 || price <= 0 || math.IsNaN(size) || math.IsNaN(price) || math.IsInf(size, 0) || math.IsInf(price, 0) {
				return result, fmt.Errorf("成交 %s 的数量或价格无效（size=%q price=%q）", id, trade.Size, trade.Price)
			}
			switch normalizeTradeStatus(trade.Status) {
			case tradeLegConfirmed:
				result.ConfirmedLegs++
				result.ConfirmedShares += size
				result.ConfirmedNotional += size * price
				result.ConfirmedLevels = append(result.ConfirmedLevels, fillLevel{shares: size, price: price})
			case tradeLegFailed:
				// A failed leg never contributes shares, but it still makes the
				// order's fill set non-final.
				result.FailedLegs++
			default:
				result.PendingLegs++
			}
		}
		next := strings.TrimSpace(response.NextCursor)
		if next == "" || next == tradeEndCursor {
			break
		}
		if next == cursor {
			return result, fmt.Errorf("成交分页游标 %q 未前进，无法确认完整成交", next)
		}
		cursor = next
	}
	if result.FailedLegs > 0 || result.PendingLegs > 0 || !tradesReconcile(result.ConfirmedShares, expectedShares) {
		return result, result.reconciliationError()
	}
	result.Complete = true
	return result, nil
}
