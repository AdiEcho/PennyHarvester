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
	"errors"
	"fmt"
	"io"
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
// walk. Fills of one order are contiguous, so a few pages are plenty and an
// unbounded walk would make a cold reconcile expensive.
const maxTradePages = 5

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

	client := &http.Client{Timeout: 20 * time.Second}
	if cfg.Proxy != nil {
		client = cfg.Proxy.HTTPClient(20 * time.Second)
	}
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
	TakerOrderID string `json:"taker_order_id"`
	Price        string `json:"price"`
	Size         string `json:"size"`
	Status       string `json:"status"`
}

// makerAddress is the wallet that owns the CLOB position: the funder for proxy,
// Safe and deposit accounts and the signer for an EOA.
func (e *Executor) makerAddress() string {
	if maker := strings.TrimSpace(e.Maker()); maker != "" {
		return maker
	}
	return strings.TrimSpace(e.Signer())
}

// tradesForOrder sums the fills of one order from GET /data/trades.
//
// This is what reconciliation needs: /data/order only reports the order's limit
// price, so booking size_matched × price charges a FAK buy at its worst allowed
// price instead of the price its matches actually paid.
func (e *Executor) tradesForOrder(ctx context.Context, orderID, conditionID string) (shares, notional float64, found bool, err error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return 0, 0, false, nil
	}
	query := url.Values{}
	if conditionID != "" {
		query.Set("market", conditionID)
	}
	if maker := e.makerAddress(); maker != "" {
		query.Set("maker_address", maker)
	}
	cursor := ""
	for page := 0; page < maxTradePages; page++ {
		if cursor != "" {
			query.Set("next_cursor", cursor)
		}
		var response struct {
			Data       []clobTrade `json:"data"`
			NextCursor string      `json:"next_cursor"`
		}
		if err := e.l2Get(ctx, "/data/trades", query, &response); err != nil {
			// No trade history for this account yet is not a failure: the caller
			// keeps the conservative limit-price estimate.
			if isNotFound(err) {
				return 0, 0, false, nil
			}
			return 0, 0, false, err
		}
		for _, trade := range response.Data {
			if !strings.EqualFold(strings.TrimSpace(trade.TakerOrderID), orderID) {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(trade.Status), "FAILED") {
				continue
			}
			size, price := parseAmount(trade.Size), parseAmount(trade.Price)
			if size <= 0 || price <= 0 {
				continue
			}
			shares += size
			notional += size * price
			found = true
		}
		// Fills of one order are contiguous, so stop as soon as they are found.
		if found || response.NextCursor == "" || response.NextCursor == cursor || len(response.Data) == 0 {
			break
		}
		cursor = response.NextCursor
	}
	return shares, notional, found, nil
}

// isNotFound reports whether err is a CLOB 404, which means the route answered
// "nothing here" rather than a transport failure.
func isNotFound(err error) bool {
	var httpErr *clobHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound
}
