// Package predictfun is the Predict.fun (BNB Chain) venue adapter. It has four
// parts that are deliberately kept separate:
//
//   - Client: the REST surface (markets, orderbook, auth, orders).
//   - Provider: maps Predict.fun markets onto market.Market for the scanner.
//   - Executor: signs EIP-712 orders and submits them, implementing venue.Executor.
//   - OnChain: the ERC-20 / ERC-1155 approvals a live order needs.
//
// Two properties of this venue shape the code:
//
//  1. Only the Yes side of an orderbook is published. The No side is derived as
//     1-p and its depth is mirrored (see BookForOutcome), which is why a No
//     mid is indicative only.
//  2. Every amount that is signed is an 18-decimal integer, while this codebase
//     carries money as float64. Amounts are therefore converted at the adapter
//     boundary with venue.Scale/venue.Unscale and never with amount*1e18.
package predictfun

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"pennyharvester/backend/internal/netproxy"
)

// DefaultBaseURL and TestnetBaseURL are separate hosts, not paths: a testnet
// flag must never be able to reach production by a path rewrite.
const (
	DefaultBaseURL = "https://api.predict.fun"
	TestnetBaseURL = "https://api-testnet.predict.fun"

	// DefaultRPCURL and TestnetRPCURL are public BSC endpoints used only for
	// reads (balances, allowances) and the approval transactions. An operator
	// with a private endpoint sets Config.RPCURL.
	DefaultRPCURL = "https://bsc-dataseed.bnbchain.org"
	TestnetRPCURL = "https://data-seed-prebsc-1-s1.bnbchain.org:8545"
)

const (
	// statusOpen is the only tradingStatus that allows new orders. The other
	// values (MATCHING_NOT_ENABLED, CANCEL_ONLY, CLOSED) only allow cancelling
	// or nothing at all.
	statusOpen = "OPEN"

	// maxPageSize is the API cap for `first`.
	maxPageSize = 100
	// marketScanCap bounds one ListOpen pass so a cursor loop cannot run away.
	marketScanCap = 500
	// maxResponseBytes caps one response body. Market pages with 100 markets are
	// well below it; the cap exists so a wrong host cannot exhaust memory.
	maxResponseBytes = 4 << 20
	// errorBodyLimit is how much of a failing response body an error carries.
	errorBodyLimit = 200
)

// Config is the adapter configuration. It is intentionally plain data: the
// caller (config package / settings UI) does the environment and secret
// plumbing, and this package never reads the environment itself.
type Config struct {
	// BaseURL is the REST root, for example https://api.predict.fun. Empty
	// selects the host that matches Testnet.
	BaseURL string
	// APIKey is sent as x-api-key on every request.
	APIKey string
	// PrivateKey is the hex-encoded EVM key of the signer (0x optional). It
	// signs the auth message and every order, and it signs approval
	// transactions. It is a BNB Chain key: never the Polymarket key.
	PrivateKey string
	// Maker is the address holding the funds. It defaults to the signer. It is
	// only different for a smart wallet, which this build does not sign for.
	Maker string
	// Testnet selects chain id 97 and the testnet contract set.
	Testnet bool
	// Proxy is the shared egress manager. Nil means a direct connection.
	Proxy *netproxy.Manager
	// RPCURL is the BSC JSON-RPC endpoint used for balances, allowances and
	// approvals. Empty selects the public endpoint for the selected network.
	RPCURL string
}

// MarketQuery is one page request for GET /v1/markets.
type MarketQuery struct {
	// First is the page size; zero means 100 (the API maximum).
	First int
	// Cursor is the opaque cursor returned by the previous page.
	Cursor string
	// Status filters on tradingStatus, for example "OPEN".
	Status string
	// CategorySlug filters on a category slug.
	CategorySlug string
}

// Number is a JSON number or numeric string kept as its original text. The API
// mixes the two forms across endpoints, and a 77-digit onChainId or an 18-decimal
// amount cannot pass through float64 without losing digits. Keeping the text
// makes the loss impossible by construction.
type Number string

// UnmarshalJSON accepts a JSON number, a numeric string, and null.
func (n *Number) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*n = ""
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*n = Number(strings.TrimSpace(value))
		return nil
	}
	var value json.Number
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("predictfun: 期望数字，收到 %s", snippet(text))
	}
	*n = Number(value.String())
	return nil
}

// Quote is a price the venue reports in more than one shape: a bare number, a
// numeric string, or a {price,size} object. The market list uses all three
// across markets, so the decoder keeps the price and ignores the size.
type Quote struct {
	Price Number
	Size  Number
}

// UnmarshalJSON accepts a number, a numeric string, null, and {price,size}.
func (q *Quote) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if strings.HasPrefix(text, "{") {
		var payload struct {
			Price Number `json:"price"`
			Size  Number `json:"size"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return err
		}
		*q = Quote{Price: payload.Price, Size: payload.Size}
		return nil
	}
	var price Number
	if err := price.UnmarshalJSON(data); err != nil {
		return err
	}
	*q = Quote{Price: price}
	return nil
}

// String returns the raw text (empty when the field was absent).
func (n Number) String() string { return string(n) }

// IsZero reports whether the field was absent or explicitly zero.
func (n Number) IsZero() bool {
	text := strings.TrimSpace(string(n))
	return text == "" || text == "0" || text == "0.0"
}

// Float64 parses the text as a float. It is only used for display fields and
// prices, never for a value that is signed.
func (n Number) Float64() (float64, error) {
	text := strings.TrimSpace(string(n))
	if text == "" {
		return 0, fmt.Errorf("predictfun: 数字为空")
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("predictfun: 解析数字 %q: %w", text, err)
	}
	return value, nil
}

// BigInt parses the text as an integer. Amounts on the wire are already
// 18-decimal integers, so a fractional part here is a contract violation and
// must not be silently rounded.
func (n Number) BigInt() (*big.Int, error) {
	text := strings.TrimSpace(string(n))
	if text == "" {
		return new(big.Int), nil
	}
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, fmt.Errorf("predictfun: 期望整数，收到 %q", text)
	}
	return value, nil
}

// Int64 parses a small integer such as decimalPrecision.
func (n Number) Int64() (int64, error) {
	value, err := n.BigInt()
	if err != nil {
		return 0, err
	}
	if !value.IsInt64() {
		return 0, fmt.Errorf("predictfun: 整数 %s 超出范围", value)
	}
	return value.Int64(), nil
}

// Market is the Predict.fun market resource. Field names follow the live API.
// Amounts are Number, not float64: feeRateBps and decimalPrecision are small,
// but onChainId is a 77-digit decimal string and the order amounts that are
// derived from these fields are signed.
type Market struct {
	// ID is the numeric market id (the same id the REST paths use).
	ID               Number          `json:"id"`
	Title            string          `json:"title"`
	Question         string          `json:"question"`
	TradingStatus    string          `json:"tradingStatus"`
	Status           string          `json:"status"`
	IsVisible        bool            `json:"isVisible"`
	IsNegRisk        bool            `json:"isNegRisk"`
	IsYieldBearing   bool            `json:"isYieldBearing"`
	FeeRateBps       Number          `json:"feeRateBps"`
	MakerFeeBps      Number          `json:"makerFeeBps"`
	ConditionID      string          `json:"conditionId"`
	Outcomes         []MarketOutcome `json:"outcomes"`
	CategorySlug     string          `json:"categorySlug"`
	DecimalPrecision Number          `json:"decimalPrecision"`
	// PolymarketConditionIDs is the exact join key to a Polymarket market. It is
	// reported for cross-venue display only; this build does not trade the pair.
	PolymarketConditionIDs []string    `json:"polymarketConditionIds"`
	Resolution             *Resolution `json:"resolution"`
	Stats                  *Stats      `json:"stats"`
}

// MarketOutcome is one side of a binary Predict.fun market. IndexSet is 1 for
// Yes and 2 for No; OnChainID is the ERC-1155 token id, a decimal string that
// is far larger than 2^53.
type MarketOutcome struct {
	Name      string `json:"name"`
	IndexSet  Number `json:"indexSet"`
	OnChainID string `json:"onChainId"`
	Status    string `json:"status"`
	BestBid   Quote  `json:"bestBid"`
	BestAsk   Quote  `json:"bestAsk"`
}

// Resolution is the venue's verdict on a settled market. It is shaped exactly
// like a MarketOutcome: the live payload names the winning side directly
// ({"name":"Up","indexSet":1,"onChainId":"…","positionId":"…","status":"WON"})
// and the same outcome in `outcomes[]` carries status WON while the other side
// carries LOST. Verified against the public API on 2026-11-02 over 56 settled
// markets (57 WON / 55 LOST, exactly one winner each).
type Resolution struct {
	Name       string `json:"name"`
	IndexSet   Number `json:"indexSet"`
	OnChainID  string `json:"onChainId"`
	PositionID string `json:"positionId"`
	// Status is "WON" for the winning side.
	Status string `json:"status"`
}

// Stats is the per-market statistics object. The list endpoint reports it as
// null, so the provider reads /v1/markets/{id}/stats when it needs a number.
// The field names are not part of a published contract, so several spellings
// are decoded and the first non-zero one wins.
type Stats struct {
	TotalLiquidityUSD Number `json:"totalLiquidityUsd"`
	Liquidity         Number `json:"liquidity"`
	VolumeUSD         Number `json:"volumeUsd"`
	Volume            Number `json:"volume"`
	Volume24hUSD      Number `json:"volume24hUsd"`
	VolumeTotalUSD    Number `json:"volumeTotalUsd"`
}

// LiquidityValue returns the best available liquidity figure, or zero.
func (s Stats) LiquidityValue() float64 {
	for _, candidate := range []Number{s.TotalLiquidityUSD, s.Liquidity} {
		if !candidate.IsZero() {
			if value, err := candidate.Float64(); err == nil {
				return value
			}
		}
	}
	return 0
}

// VolumeValue returns the best available volume figure, or zero. Lifetime
// volume is preferred because it matches the Polymarket field the UI labels
// the same way; 24h volume is the fallback.
func (s Stats) VolumeValue() float64 {
	for _, candidate := range []Number{s.VolumeUSD, s.Volume, s.VolumeTotalUSD, s.Volume24hUSD} {
		if !candidate.IsZero() {
			if value, err := candidate.Float64(); err == nil {
				return value
			}
		}
	}
	return 0
}

// Level is one orderbook price level. The wire form is [price, size]; the
// object form {price, size} is also accepted so a future API that adds names
// does not break the book.
type Level struct {
	Price Number
	Size  Number
}

// UnmarshalJSON accepts [price, size] and {"price":…, "size":…}.
func (l *Level) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("predictfun: 空盘口档位")
	}
	if trimmed[0] == '{' {
		var object struct {
			Price Number `json:"price"`
			Size  Number `json:"size"`
		}
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return err
		}
		l.Price, l.Size = object.Price, object.Size
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return fmt.Errorf("predictfun: 盘口档位应为 [price,size]: %w", err)
	}
	if len(raw) < 2 {
		return fmt.Errorf("predictfun: 盘口档位需包含价格与数量")
	}
	if err := json.Unmarshal(raw[0], &l.Price); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &l.Size)
}

// Book is the native (Yes) orderbook. Predict.fun publishes only this side.
type Book struct {
	MarketID          Number  `json:"marketId"`
	UpdateTimestampMs Number  `json:"updateTimestampMs"`
	Asks              []Level `json:"asks"`
	Bids              []Level `json:"bids"`
}

// Category is the category resource. It is the only place the list API's
// markets can get an end date from, which is why the provider caches it per
// slug instead of requesting it per market.
type Category struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	EndsAt  string `json:"endsAt"`
	Markets []struct {
		ID Number `json:"id"`
	} `json:"markets"`
	Tags []Tag `json:"tags"`
}

// Tag is a category tag.
type Tag struct {
	ID    Number `json:"id"`
	Slug  string `json:"slug"`
	Label string `json:"label"`
}

// SignedOrder is the wire form of the EIP-712 Order struct plus its signature.
// Every uint256 is a decimal string: the 77-digit token id and an 18-decimal
// amount do not round-trip through a JSON number in every client.
type SignedOrder struct {
	Salt          string `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	Taker         string `json:"taker"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Expiration    string `json:"expiration"`
	Nonce         string `json:"nonce"`
	FeeRateBps    string `json:"feeRateBps"`
	Side          uint8  `json:"side"`
	SignatureType uint8  `json:"signatureType"`
	Signature     string `json:"signature"`
}

// SignedOrderRequest is the POST /v1/orders payload.
//
// Only Order is signed; the three companion fields are the hints the SDK sends
// with a marketable order (price in decimal form, the slippage that produced
// the signed amounts, and whether the taker amount is a minimum output). The
// server re-checks them against the signed struct, so a wrong hint is rejected
// rather than executed at a different price.
type SignedOrderRequest struct {
	Order          SignedOrder `json:"data"`
	PricePerShare  string      `json:"pricePerShare,omitempty"`
	SlippageBps    int         `json:"slippageBps,omitempty"`
	IsMinAmountOut *bool       `json:"isMinAmountOut,omitempty"`
}

// OrderAck is an order as the API reports it. Several optional spellings are
// decoded because the create and read endpoints are not documented field for
// field; treat the zero value as "not reported", never as zero.
type OrderAck struct {
	ID          Number `json:"id"`
	OrderID     Number `json:"orderId"`
	Status      string `json:"status"`
	MarketID    Number `json:"marketId"`
	ConditionID string `json:"conditionId"`
	// OnChainID is the traded token id when the API echoes it.
	OnChainID     string `json:"onChainId"`
	Side          Number `json:"side"`
	PricePerShare Number `json:"pricePerShare"`
	MakerAmount   Number `json:"makerAmount"`
	TakerAmount   Number `json:"takerAmount"`
	// AmountFilled and AmountMatched are the 18-decimal filled share counts
	// under two different upstream names.
	AmountFilled  Number `json:"amountFilled"`
	AmountMatched Number `json:"amountMatched"`
	Fee           Number `json:"fee"`
	FeeAmount     Number `json:"feeAmount"`
	TxHash        string `json:"txHash"`
	CreatedAt     string `json:"createdAt"`
	ErrorMessage  string `json:"errorMessage"`
}

// IDString returns the exchange order id under whichever field carries it.
func (a OrderAck) IDString() string {
	if id := strings.TrimSpace(a.ID.String()); id != "" && id != "0" {
		return id
	}
	return strings.TrimSpace(a.OrderID.String())
}

// FilledShares returns the filled share count, unscaled from 18 decimals.
// A wire value with a fractional part is tolerated here (a fill report is a
// display quantity, not a signed amount): assuming zero would mark a real fill
// as a rejection.
func (a OrderAck) FilledShares() float64 {
	raw := a.AmountFilled
	if raw.IsZero() {
		raw = a.AmountMatched
	}
	value, err := raw.BigInt()
	if err != nil || value.Sign() <= 0 {
		if decimal, floatErr := raw.Float64(); floatErr == nil && decimal > 0 {
			return decimal / 1e18
		}
		return 0
	}
	return unscale18(value)
}

// sideIsSell reports whether the ack describes a sell. The side is absent on
// some responses, in which case the caller falls back to what it asked for.
func (a OrderAck) sideIsSell(fallback bool) bool {
	// "" means the response carried no side at all, and it must fall back to the
	// caller's intent: decoding it as a number yields 0 (a buy) with no error,
	// which would swap the maker/taker amounts of a sell. An explicit "0" is a
	// real buy and must be honoured.
	if strings.TrimSpace(string(a.Side)) == "" {
		return fallback
	}
	side, err := a.Side.Int64()
	if err != nil {
		return fallback
	}
	return side == int64(SideSell)
}

// ImpliedPrice derives the execution price from the signed maker/taker amounts,
// which every order response echoes. It is the last resort before giving up on
// an otherwise fill-bearing response: a reconciliation with price 0 used to
// leave the order unresolved forever, which froze the account's stop-loss.
func (a OrderAck) ImpliedPrice(sell bool) float64 {
	shares, collateral, err := a.amountPair(sell)
	if err != nil || shares <= 0 || collateral <= 0 {
		return 0
	}
	return collateral / shares
}

// ImpliedShares derives the order size from the signed amounts. It is used when
// a filled response omits the executed quantity.
func (a OrderAck) ImpliedShares(sell bool) float64 {
	shares, _, err := a.amountPair(sell)
	if err != nil || shares <= 0 {
		return 0
	}
	return shares
}

// amountPair reads (shares, collateral) from the ack. On a buy the maker gives
// collateral and the taker gives shares; on a sell they swap.
func (a OrderAck) amountPair(sell bool) (shares, collateral float64, err error) {
	maker, err := a.MakerAmount.BigInt()
	if err != nil {
		return 0, 0, err
	}
	taker, err := a.TakerAmount.BigInt()
	if err != nil {
		return 0, 0, err
	}
	if sell {
		return unscale18(maker), unscale18(taker), nil
	}
	return unscale18(taker), unscale18(maker), nil
}

// FeePaid returns the fee the API charged, if it reported one. It is a
// 18-decimal collateral amount; the executor recomputes the fee from the market
// rate when this is absent.
func (a OrderAck) FeePaid() float64 {
	raw := a.Fee
	if raw.IsZero() {
		raw = a.FeeAmount
	}
	value, err := raw.BigInt()
	if err != nil || value.Sign() <= 0 {
		return 0
	}
	return unscale18(value)
}

// Position is one open position. It is decoded for diagnostics only; trading
// decisions come from the local ledger, and Predict.fun has no public
// resolution feed this build can settle from.
type Position struct {
	ID            Number `json:"id"`
	MarketID      Number `json:"marketId"`
	ConditionID   string `json:"conditionId"`
	OnChainID     string `json:"onChainId"`
	Outcome       string `json:"outcome"`
	Shares        Number `json:"shares"`
	Amount        Number `json:"amount"`
	AveragePrice  Number `json:"averagePrice"`
	Value         Number `json:"value"`
	UnrealizedPNL Number `json:"unrealizedPnl"`
	Status        string `json:"status"`
}

// APIError is a non-2xx or success:false response. It carries the HTTP status
// and a short body excerpt, never a request header, so a failure is diagnosable
// without leaking the API key.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

// Error renders the status and the body excerpt. It never includes a request
// header, so the API key cannot leak through an error string.
func (e *APIError) Error() string {
	detail := strings.TrimSpace(e.Body)
	if detail == "" {
		detail = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("Predict.fun %s %s 失败（HTTP %d）：%s", e.Method, e.Path, e.StatusCode, detail)
}

// NotFound reports a 404, which callers treat as "no such order" rather than a
// transport failure.
func (e *APIError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// IsNotFound reports whether err is an APIError with a 404 status.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.NotFound()
}

// settings is the normalized configuration shared by the REST client and the
// on-chain helper. Both must agree on the chain, the contract set and the key,
// so resolving it twice is worse than resolving it once.
type settings struct {
	// raw keeps the caller's Config so the on-chain helper can be built from the
	// exact same credentials and network selection.
	raw     Config
	baseURL string
	rpcURL  string
	apiKey  string
	chainID int64
	addr    Addresses
	key     *ecdsa.PrivateKey
	signer  common.Address
	maker   common.Address
	// makerImplicit records that the maker was never configured and simply
	// followed the signer. A key rotation must re-derive it, not keep the old
	// address, or signer != maker would silently disable live trading.
	makerImplicit bool
	testnet       bool
}

func resolveConfig(cfg Config) (settings, error) {
	chainID := ChainIDBnbMainnet
	if cfg.Testnet {
		chainID = ChainIDBnbTestnet
	}
	addr, ok := AddressesFor(chainID)
	if !ok {
		return settings{}, fmt.Errorf("Predict.fun 不支持链 %d", chainID)
	}
	resolved := settings{
		raw:     cfg,
		baseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		rpcURL:  strings.TrimRight(strings.TrimSpace(cfg.RPCURL), "/"),
		apiKey:  strings.TrimSpace(cfg.APIKey),
		chainID: chainID,
		addr:    addr,
		testnet: cfg.Testnet,
	}
	if resolved.baseURL == "" {
		if cfg.Testnet {
			resolved.baseURL = TestnetBaseURL
		} else {
			resolved.baseURL = DefaultBaseURL
		}
	}
	if resolved.rpcURL == "" {
		if cfg.Testnet {
			resolved.rpcURL = TestnetRPCURL
		} else {
			resolved.rpcURL = DefaultRPCURL
		}
	}

	rawKey := strings.TrimSpace(cfg.PrivateKey)
	rawKey = strings.TrimPrefix(strings.TrimPrefix(rawKey, "0x"), "0X")
	if rawKey != "" {
		key, err := crypto.HexToECDSA(rawKey)
		if err != nil {
			// Never echo the value: a malformed key is often a truncated real
			// key and half of it is still a secret.
			return settings{}, fmt.Errorf("PREDICTFUN_PRIVATE_KEY 格式无效（需要 64 位十六进制私钥）")
		}
		resolved.key = key
		resolved.signer = crypto.PubkeyToAddress(key.PublicKey)
	}

	rawMaker := strings.TrimSpace(cfg.Maker)
	resolved.makerImplicit = rawMaker == ""
	switch {
	case rawMaker == "":
		resolved.maker = resolved.signer
	case common.IsHexAddress(rawMaker):
		resolved.maker = common.HexToAddress(rawMaker)
	default:
		return settings{}, fmt.Errorf("PREDICTFUN_SMART_WALLET 地址无效: %s", snippet(rawMaker))
	}
	return resolved, nil
}

// Client is the Predict.fun REST client. It is safe for concurrent use: the
// access token is guarded by a mutex and refresh is serialized so a burst of
// 401s produces one Authenticate call, not one per request.
type Client struct {
	// cfg is swapped atomically when the settings page saves new endpoints or
	// credentials, so a scan that is mid-flight never reads a half-applied
	// chain id, contract set or currency.
	cfg  atomic.Pointer[settings]
	http *http.Client

	authMu sync.Mutex

	// hooks run after a successful reconfigure so caches derived from the old
	// network (resolution dates, token index) can be dropped.
	hookMu sync.RWMutex
	hooks  []func()

	mu        sync.Mutex
	token     string
	expiresAt time.Time

	// base is the configuration the process started with (environment only).
	// ResetLiveSettings returns to it, which is the only baseline the client can
	// reconstruct without a stored settings row.
	base Config

	// now, sleep and backoff are test seams. Production uses the wall clock and
	// a context-aware timer.
	now     func() time.Time
	sleep   func(time.Duration)
	backoff func(context.Context, time.Duration) error
}

// NewClient builds a REST client. A missing private key is allowed: market
// reads work without one, and the executor reports the missing key through
// Configured/ReadyError instead of refusing to construct a read-only client.
func NewClient(cfg Config) (*Client, error) {
	resolved, err := resolveConfig(cfg)
	if err != nil {
		return nil, err
	}
	timeout := 20 * time.Second
	var httpClient *http.Client
	if cfg.Proxy != nil {
		httpClient = cfg.Proxy.HTTPClient(timeout)
	} else {
		httpClient = &http.Client{Timeout: timeout}
	}
	client := &Client{
		http:  httpClient,
		now:   time.Now,
		sleep: time.Sleep,
		base:  cfg,
	}
	client.cfg.Store(&resolved)
	client.backoff = client.wait
	return client, nil
}

// current returns a consistent snapshot of the live configuration. Callers
// must not cache it across requests: a settings save may replace it at any
// time.
func (c *Client) current() settings {
	if c == nil {
		return settings{}
	}
	if snapshot := c.cfg.Load(); snapshot != nil {
		return *snapshot
	}
	return settings{}
}

// ChainID is the chain the client signs for.
func (c *Client) ChainID() int64 { return c.current().chainID }

// Addresses is the contract set for that chain.
func (c *Client) Addresses() Addresses { return c.current().addr }

// Testnet reports which network the client points at right now. That is not
// necessarily the network the process started with: a credential save can flip
// it, and the UI must not keep claiming "testnet" after that.
func (c *Client) Testnet() bool {
	if c == nil {
		return false
	}
	return c.current().testnet
}

// BaseURL is the REST root in use.
func (c *Client) BaseURL() string { return c.current().baseURL }

// DefaultBaseURL is the REST root implied by the current network.
func (c *Client) DefaultBaseURL() string {
	if c.current().testnet {
		return TestnetBaseURL
	}
	return DefaultBaseURL
}

// Signer returns the 0x address derived from the configured key, or "".
func (c *Client) Signer() string {
	cfg := c.current()
	if cfg.signer == (common.Address{}) {
		return ""
	}
	return cfg.signer.Hex()
}

// Maker returns the address holding the funds, defaulting to the signer.
func (c *Client) Maker() string {
	cfg := c.current()
	if cfg.maker == (common.Address{}) {
		return ""
	}
	return cfg.maker.Hex()
}

// Configured reports whether both credentials a live order needs are present.
func (c *Client) Configured() bool {
	cfg := c.current()
	return cfg.apiKey != "" && cfg.key != nil
}

// SetBaseURL points the client at another REST root. An empty value restores
// the default for the current network. The swap is validated before it becomes
// visible, and the token plus every derived cache are dropped afterwards.
func (c *Client) SetBaseURL(raw string) error {
	if c == nil {
		return fmt.Errorf("Predict.fun 客户端未初始化")
	}
	current := c.current()
	next := current.raw
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed != "" {
		parsed, err := url.Parse(trimmed)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("Predict.fun 行情地址无效: %s", snippet(trimmed))
		}
	}
	next.BaseURL = trimmed
	resolved, err := resolveConfig(next)
	if err != nil {
		return err
	}
	if resolved.baseURL == current.baseURL {
		return nil
	}
	c.apply(resolved)
	return nil
}

// ApplyLiveSettings replaces the credential set at runtime. An empty string
// keeps the current value, and a nil testnet leaves the network alone.
//
// testnet is a pointer because "not saved" and "saved as mainnet" are different
// intentions: a deployment started with PREDICTFUN_TESTNET=1 must not be
// flipped to mainnet (real USDT, mainnet contracts) by saving an unrelated
// field from the settings page. Only an explicit value switches networks.
func (c *Client) ApplyLiveSettings(apiKey, privateKey, smartWallet string, testnet *bool, baseURL string) error {
	if c == nil {
		return fmt.Errorf("Predict.fun 客户端未初始化")
	}
	current := c.current()
	next := current.raw
	if apiKey != "" {
		next.APIKey = apiKey
	} else {
		next.APIKey = current.apiKey
	}
	if privateKey != "" {
		next.PrivateKey = privateKey
	}
	switch {
	case smartWallet != "":
		next.Maker = smartWallet
	case current.makerImplicit:
		// No wallet was ever configured: let resolveConfig follow the new key
		// instead of pinning the previous signer's address.
		next.Maker = ""
	case next.Maker == "" && current.maker != (common.Address{}):
		next.Maker = current.maker.Hex()
	}
	switched := testnet != nil && *testnet != current.testnet
	if switched {
		next.Testnet = *testnet
		// A testnet host does not serve mainnet markets, so a network switch
		// drops the endpoint pinned for the other one. This has to happen before
		// the assignment below, and it has to ignore an incoming endpoint equal
		// to the current one: callers resolve the venue URL against the *old*
		// network, so honouring it would leave "mainnet chain id, testnet REST"
		// after the switch.
		next.BaseURL = ""
		if baseURL == current.baseURL {
			baseURL = ""
		}
	}
	if baseURL != "" {
		next.BaseURL = baseURL
	}
	resolved, err := resolveConfig(next)
	if err != nil {
		return err
	}
	c.apply(resolved)
	return nil
}

// ResetLiveSettings returns the client to the configuration the process started
// with, which is what a credential probe needs before re-applying the persisted
// settings: an empty field means "keep", so restoring with empty strings would
// silently retain the candidate that was just tested.
func (c *Client) ResetLiveSettings() error {
	if c == nil {
		return fmt.Errorf("Predict.fun 客户端未初始化")
	}
	resolved, err := resolveConfig(c.base)
	if err != nil {
		return err
	}
	c.apply(resolved)
	return nil
}

// OnReconfigure registers a callback that runs after the configuration is
// replaced. Providers use it to drop caches built from the previous network.
func (c *Client) OnReconfigure(fn func()) {
	if c == nil || fn == nil {
		return
	}
	c.hookMu.Lock()
	c.hooks = append(c.hooks, fn)
	c.hookMu.Unlock()
}

// apply makes a validated configuration visible, then drops the token and every
// derived cache. Callers must resolve the config first so a rejected value can
// never leave the client half-configured.
func (c *Client) apply(resolved settings) {
	c.cfg.Store(&resolved)
	c.mu.Lock()
	c.token = ""
	c.expiresAt = time.Time{}
	c.mu.Unlock()
	c.hookMu.RLock()
	hooks := append([]func(){}, c.hooks...)
	c.hookMu.RUnlock()
	for _, fn := range hooks {
		fn()
	}
}

// MaskedAPIKey returns the key with its tail removed so it can be compared
// against the dashboard without printing the secret.
func (c *Client) MaskedAPIKey() string {
	key := c.current().apiKey
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "…"
	}
	return key[:8] + "…"
}

// apiEnvelope is the response envelope every endpoint uses.
type apiEnvelope struct {
	Success bool            `json:"success"`
	Cursor  string          `json:"cursor"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Error   string          `json:"error"`
}

type apiRequest struct {
	method string
	path   string
	query  url.Values
	body   any
	// authed requests carry the bearer token and are retried once after a 401.
	authed bool
}

// roundTrip performs one logical request, including the two retries that are
// safe because they reuse the same payload:
//
//   - HTTP 429 backs off once (honouring Retry-After) and retries.
//   - HTTP 401 refreshes the access token once and retries.
//
// A 5xx is never retried, signed order or not: the order may already be live
// and the caller must reconcile instead of sending it again.
func (c *Client) roundTrip(ctx context.Context, request apiRequest, out any) (string, error) {
	var payload []byte
	if request.body != nil {
		encoded, err := json.Marshal(request.body)
		if err != nil {
			return "", fmt.Errorf("编码 Predict.fun %s 请求: %w", request.path, err)
		}
		payload = encoded
	}

	cfg := c.current()
	target := cfg.baseURL + request.path
	if len(request.query) > 0 {
		target += "?" + request.query.Encode()
	}

	renewedToken := false
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, request.method, target, reader)
		if err != nil {
			return "", fmt.Errorf("构建 Predict.fun %s 请求: %w", request.path, err)
		}
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if cfg.apiKey != "" {
			req.Header.Set("x-api-key", cfg.apiKey)
		}
		if request.authed {
			token, tokenErr := c.bearerToken(ctx)
			if tokenErr != nil {
				return "", tokenErr
			}
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return "", fmt.Errorf("请求 Predict.fun %s: %w", request.path, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
		if readErr != nil {
			return "", fmt.Errorf("读取 Predict.fun %s 响应: %w", request.path, readErr)
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt == 0:
			delay := retryDelay(resp.Header.Get("Retry-After"), c.now())
			if waitErr := c.backoff(ctx, delay); waitErr != nil {
				return "", waitErr
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized && request.authed && !renewedToken:
			c.invalidateToken()
			if authErr := c.Authenticate(ctx); authErr != nil {
				return "", authErr
			}
			renewedToken = true
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", &APIError{
				Method:     request.method,
				Path:       request.path,
				StatusCode: resp.StatusCode,
				Body:       snippet(string(raw)),
			}
		}

		var envelope apiEnvelope
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
			if err := json.Unmarshal(trimmed, &envelope); err != nil {
				return "", fmt.Errorf("解析 Predict.fun %s 响应: %w（%s）", request.path, err, snippet(string(raw)))
			}
		} else {
			envelope.Success = true
		}
		if !envelope.Success {
			detail := strings.TrimSpace(envelope.Message)
			if detail == "" {
				detail = strings.TrimSpace(envelope.Error)
			}
			if detail == "" {
				detail = snippet(string(raw))
			}
			return "", &APIError{
				Method:     request.method,
				Path:       request.path,
				StatusCode: resp.StatusCode,
				Body:       detail,
			}
		}
		if out != nil && len(bytes.TrimSpace(envelope.Data)) > 0 && string(bytes.TrimSpace(envelope.Data)) != "null" {
			if err := json.Unmarshal(envelope.Data, out); err != nil {
				return "", fmt.Errorf("解析 Predict.fun %s 数据: %w", request.path, err)
			}
		}
		return envelope.Cursor, nil
	}
}

// wait is the production backoff. It honours cancellation so a shutdown never
// waits out a rate-limit window.
func (c *Client) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryDelay turns a Retry-After header into a bounded wait. The cap keeps a
// hostile or broken header from stalling a request for minutes; the default is
// one second, which is shorter than the 240 req/min window it waits for.
func retryDelay(header string, now time.Time) time.Duration {
	const (
		fallback = time.Second
		maximum  = 30 * time.Second
	)
	value := strings.TrimSpace(header)
	if value == "" {
		return fallback
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		delay := time.Duration(seconds) * time.Second
		if delay < 0 {
			return 0
		}
		if delay > maximum {
			return maximum
		}
		return delay
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := when.Sub(now)
		if delay < 0 {
			return 0
		}
		if delay > maximum {
			return maximum
		}
		return delay
	}
	return fallback
}

func (c *Client) invalidateToken() {
	c.mu.Lock()
	c.token = ""
	c.expiresAt = time.Time{}
	c.mu.Unlock()
}

// bearerToken returns a usable access token, authenticating when the cache is
// empty or within 30 seconds of expiry.
func (c *Client) bearerToken(ctx context.Context) (string, error) {
	if c.current().key == nil {
		return "", fmt.Errorf("Predict.fun 实盘未配置：缺少 PREDICTFUN_PRIVATE_KEY")
	}
	c.mu.Lock()
	token, expiresAt := c.token, c.expiresAt
	c.mu.Unlock()
	if token != "" && (expiresAt.IsZero() || c.now().Add(30*time.Second).Before(expiresAt)) {
		return token, nil
	}
	if err := c.Authenticate(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		return "", fmt.Errorf("Predict.fun 登录未返回访问令牌")
	}
	return c.token, nil
}

// AuthMessage fetches the plain-text message to personal_sign. An empty address
// defaults to the configured signer.
func (c *Client) AuthMessage(ctx context.Context, address string) (string, error) {
	if strings.TrimSpace(address) == "" {
		address = c.Signer()
	}
	if address == "" {
		return "", fmt.Errorf("Predict.fun 实盘未配置：缺少签名地址")
	}
	var data struct {
		Message string `json:"message"`
	}
	_, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodGet,
		path:   "/v1/auth/message",
		query:  url.Values{"address": {address}},
	}, &data)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(data.Message) == "" {
		return "", fmt.Errorf("Predict.fun 未返回待签名消息")
	}
	return data.Message, nil
}

// Authenticate performs the two-step login: read the challenge, sign it with
// EIP-191 personal_sign, exchange it for a bearer token. The token is cached
// until shortly before its JWT expiry.
func (c *Client) Authenticate(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("Predict.fun 实盘未配置")
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	// Pin one configuration snapshot: signing with a key that was swapped out
	// between reading the address and hashing the message would produce a
	// signature from the wrong account.
	cfg := c.current()
	if cfg.key == nil {
		return fmt.Errorf("Predict.fun 实盘未配置：缺少 PREDICTFUN_PRIVATE_KEY")
	}
	signer := ""
	if cfg.signer != (common.Address{}) {
		signer = cfg.signer.Hex()
	}
	message, err := c.AuthMessage(ctx, signer)
	if err != nil {
		return fmt.Errorf("读取 Predict.fun 登录消息: %w", err)
	}
	signature, err := PersonalSign(cfg.key, message)
	if err != nil {
		return fmt.Errorf("签名 Predict.fun 登录消息: %w", err)
	}
	var data struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
		ExpiresIn Number `json:"expiresIn"`
	}
	_, err = c.roundTrip(ctx, apiRequest{
		method: http.MethodPost,
		path:   "/v1/auth",
		body: map[string]string{
			"signer":    c.Signer(),
			"message":   message,
			"signature": signature,
		},
	}, &data)
	if err != nil {
		return fmt.Errorf("Predict.fun 登录: %w", err)
	}
	token := strings.TrimSpace(data.Token)
	if token == "" {
		return fmt.Errorf("Predict.fun 登录未返回访问令牌")
	}
	expiresAt := c.tokenExpiry(token, data.ExpiresAt, data.ExpiresIn)
	c.mu.Lock()
	c.token = token
	c.expiresAt = expiresAt
	c.mu.Unlock()
	return nil
}

// tokenExpiry prefers the JWT's own exp claim, then an explicit expiresAt, then
// expiresIn, and finally a conservative five-minute cache window. Refreshing
// early is harmless; using an expired token is not.
func (c *Client) tokenExpiry(token, expiresAt string, expiresIn Number) time.Time {
	if expiry, ok := jwtExpiry(token); ok {
		return expiry
	}
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(expiresAt)); err == nil {
		return parsed
	}
	if seconds, err := expiresIn.Int64(); err == nil && seconds > 0 {
		return c.now().Add(time.Duration(seconds) * time.Second)
	}
	return c.now().Add(5 * time.Minute)
}

// jwtExpiry reads the exp claim out of a JWT without verifying it. The token is
// only used by this process and its signature is checked by the server, so
// parsing the claim for cache invalidation is safe.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0).UTC(), true
}

// Markets reads one page of markets. The returned cursor is empty on the last
// page.
func (c *Client) Markets(ctx context.Context, query MarketQuery) ([]Market, string, error) {
	first := query.First
	if first <= 0 || first > maxPageSize {
		first = maxPageSize
	}
	values := url.Values{"first": {strconv.Itoa(first)}}
	if query.Cursor != "" {
		values.Set("cursor", query.Cursor)
	}
	if query.Status != "" {
		values.Set("status", query.Status)
	}
	if query.CategorySlug != "" {
		values.Set("categorySlug", query.CategorySlug)
	}
	var raw json.RawMessage
	cursor, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodGet,
		path:   "/v1/markets",
		query:  values,
	}, &raw)
	if err != nil {
		return nil, "", err
	}
	markets, next, err := decodeMarketPage(raw)
	if err != nil {
		return nil, "", err
	}
	if cursor == "" {
		cursor = next
	}
	return markets, cursor, nil
}

// decodeMarketPage accepts both an array payload and the object form that a
// paginated endpoint usually uses, so a contract detail cannot break scanning.
func decodeMarketPage(raw json.RawMessage) ([]Market, string, error) {
	var page struct {
		Items      []Market `json:"items"`
		Markets    []Market `json:"markets"`
		Data       []Market `json:"data"`
		NextCursor string   `json:"nextCursor"`
		Cursor     string   `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page); err == nil && (len(page.Items) > 0 || len(page.Markets) > 0 || len(page.Data) > 0) {
		switch {
		case len(page.Items) > 0:
			return page.Items, firstNonEmpty(page.NextCursor, page.Cursor), nil
		case len(page.Markets) > 0:
			return page.Markets, firstNonEmpty(page.NextCursor, page.Cursor), nil
		default:
			return page.Data, firstNonEmpty(page.NextCursor, page.Cursor), nil
		}
	}
	var markets []Market
	if err := json.Unmarshal(raw, &markets); err != nil {
		return nil, "", fmt.Errorf("解析 Predict.fun 市场列表: %w", err)
	}
	return markets, "", nil
}

// Market reads one market by its numeric id.
func (c *Client) Market(ctx context.Context, id string) (Market, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return Market{}, fmt.Errorf("缺少 Predict.fun 市场 ID")
	}
	var raw json.RawMessage
	if _, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodGet,
		path:   "/v1/markets/" + url.PathEscape(trimmed),
	}, &raw); err != nil {
		return Market{}, err
	}
	var market Market
	if err := json.Unmarshal(raw, &market); err != nil {
		return Market{}, fmt.Errorf("解析 Predict.fun 市场: %w", err)
	}
	if strings.TrimSpace(market.ID.String()) == "" {
		// Some deployments wrap the resource as {"market": {...}}.
		var wrapped struct {
			Market Market `json:"market"`
		}
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return Market{}, fmt.Errorf("解析 Predict.fun 市场: %w", err)
		}
		market = wrapped.Market
	}
	if strings.TrimSpace(market.ID.String()) == "" {
		return Market{}, fmt.Errorf("Predict.fun 未返回市场 %s", trimmed)
	}
	return market, nil
}

// Orderbook reads the native (Yes) orderbook. There is no separate No book:
// the No side is 1-p with its depth mirrored, see BookForOutcome.
func (c *Client) Orderbook(ctx context.Context, marketID string) (Book, error) {
	trimmed := strings.TrimSpace(marketID)
	if trimmed == "" {
		return Book{}, fmt.Errorf("缺少 Predict.fun 市场 ID，无法读取盘口")
	}
	var book Book
	if _, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodGet,
		path:   "/v1/markets/" + url.PathEscape(trimmed) + "/orderbook",
	}, &book); err != nil {
		return Book{}, err
	}
	return book, nil
}

// Category reads one category, which is where the market list's end date comes
// from.
func (c *Client) Category(ctx context.Context, slug string) (Category, error) {
	trimmed := strings.TrimSpace(slug)
	if trimmed == "" {
		return Category{}, fmt.Errorf("缺少 Predict.fun 分类标识")
	}
	var category Category
	if _, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodGet,
		path:   "/v1/categories/" + url.PathEscape(trimmed),
	}, &category); err != nil {
		return Category{}, err
	}
	return category, nil
}

// Stats reads one market's statistics. It is optional data: the provider keeps
// working when this fails.
func (c *Client) Stats(ctx context.Context, marketID string) (Stats, error) {
	trimmed := strings.TrimSpace(marketID)
	if trimmed == "" {
		return Stats{}, fmt.Errorf("缺少 Predict.fun 市场 ID，无法读取统计")
	}
	var raw json.RawMessage
	if _, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodGet,
		path:   "/v1/markets/" + url.PathEscape(trimmed) + "/stats",
	}, &raw); err != nil {
		return Stats{}, err
	}
	var stats Stats
	if err := json.Unmarshal(raw, &stats); err != nil {
		return Stats{}, fmt.Errorf("解析 Predict.fun 市场统计: %w", err)
	}
	if stats.LiquidityValue() == 0 && stats.VolumeValue() == 0 {
		var wrapped struct {
			Stats Stats `json:"stats"`
		}
		if err := json.Unmarshal(raw, &wrapped); err == nil {
			return wrapped.Stats, nil
		}
	}
	return stats, nil
}

// PlaceOrder submits one signed order. The payload is built once by the caller;
// the 401/429 retries inside roundTrip resend the same signature, which is what
// makes a retry safe (a re-signed order would be a second order).
func (c *Client) PlaceOrder(ctx context.Context, request SignedOrderRequest) (OrderAck, error) {
	var raw json.RawMessage
	if _, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodPost,
		path:   "/v1/orders",
		body:   request,
		authed: true,
	}, &raw); err != nil {
		return OrderAck{}, err
	}
	return decodeOrderAck(raw)
}

// Order reads one order by id. A 404 is returned as *APIError; callers use
// IsNotFound to distinguish "unknown order" from a transport failure.
func (c *Client) Order(ctx context.Context, orderID string) (OrderAck, error) {
	trimmed := strings.TrimSpace(orderID)
	if trimmed == "" {
		return OrderAck{}, fmt.Errorf("缺少 Predict.fun 订单 ID")
	}
	var raw json.RawMessage
	if _, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodGet,
		path:   "/v1/orders/" + url.PathEscape(trimmed),
		authed: true,
	}, &raw); err != nil {
		return OrderAck{}, err
	}
	return decodeOrderAck(raw)
}

// decodeOrderAck tolerates both the flat order object and a {"order": {...}}
// wrapper.
func decodeOrderAck(raw json.RawMessage) (OrderAck, error) {
	var ack OrderAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return OrderAck{}, fmt.Errorf("解析 Predict.fun 订单: %w", err)
	}
	if ack.IDString() == "" {
		var wrapped struct {
			Order OrderAck `json:"order"`
		}
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return OrderAck{}, fmt.Errorf("解析 Predict.fun 订单: %w", err)
		}
		if wrapped.Order.IDString() != "" {
			ack = wrapped.Order
		}
	}
	return ack, nil
}

// CancelOrder removes one order. The request body shape is the SDK's
// {orderId} form; a server that expects a list is a contract change and will
// answer with a 400 rather than silently cancelling something else.
func (c *Client) CancelOrder(ctx context.Context, orderID string) error {
	trimmed := strings.TrimSpace(orderID)
	if trimmed == "" {
		return fmt.Errorf("缺少 Predict.fun 订单 ID，无法撤单")
	}
	_, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodPost,
		path:   "/v1/orders/remove",
		body:   map[string]string{"orderId": trimmed},
		authed: true,
	}, nil)
	return err
}

// Positions reads the account's positions. Nothing in this build trades from
// it; it exists so an operator can verify what the exchange believes.
func (c *Client) Positions(ctx context.Context) ([]Position, error) {
	var raw json.RawMessage
	if _, err := c.roundTrip(ctx, apiRequest{
		method: http.MethodGet,
		path:   "/v1/positions",
		authed: true,
	}, &raw); err != nil {
		return nil, err
	}
	var positions []Position
	if err := json.Unmarshal(raw, &positions); err != nil {
		var page struct {
			Items     []Position `json:"items"`
			Positions []Position `json:"positions"`
		}
		if wrappedErr := json.Unmarshal(raw, &page); wrappedErr != nil {
			return nil, fmt.Errorf("解析 Predict.fun 持仓: %w", err)
		}
		if len(page.Positions) > 0 {
			return page.Positions, nil
		}
		if len(page.Items) > 0 {
			return page.Items, nil
		}
	}
	return positions, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// snippet truncates untrusted text for an error message. It is rune-aware so a
// multi-byte error body is not cut mid-character.
func snippet(text string) string {
	trimmed := strings.TrimSpace(text)
	runes := []rune(trimmed)
	if len(runes) <= errorBodyLimit {
		return trimmed
	}
	return string(runes[:errorBodyLimit]) + "…"
}
