package predictfun

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// testKeyHex is a throwaway key used by every test. It is publicly known and
// never funded.
const testKeyHex = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.HexToECDSA(testKeyHex)
	if err != nil {
		t.Fatalf("test key: %v", err)
	}
	return key
}

// testMarketJSON is a market shaped exactly like the probed testnet payload:
// numeric id, 18-decimal on-chain ids, decimal prices.
const testMarketJSON = `{
  "id": 42,
  "title": "Will BTC close above 100k?",
  "question": "Will BTC close above 100k by 2030?",
  "tradingStatus": "OPEN",
  "status": "REGISTERED",
  "isVisible": true,
  "isNegRisk": false,
  "isYieldBearing": false,
  "feeRateBps": 200,
  "makerFeeBps": 0,
  "conditionId": "0xcond42",
  "categorySlug": "crypto",
  "decimalPrecision": 2,
  "stats": {"totalLiquidityUsd": 90.5, "volumeUsd": 12.25},
  "outcomes": [
    {"name": "Yes", "indexSet": 1, "onChainId": "111", "status": "OPEN", "bestBid": 0.48, "bestAsk": 0.5},
    {"name": "No", "indexSet": 2, "onChainId": "222", "status": "OPEN", "bestBid": 0.5, "bestAsk": 0.52}
  ]
}`

const testEndsAt = "2030-01-01T00:00:00Z"

// fakeExchange is a Predict.fun API standing in for the real one. It records the
// signed order bodies so tests can assert what would have hit the wire.
type fakeExchange struct {
	mu sync.Mutex

	marketJSON string
	bookJSON   string
	category   map[string]string

	orderID           string
	orderHash         string
	orderStatus       string
	orderAmount       string
	orderAmountFilled string
	// matchesJSON is the next matches page. matchPages, when present, is keyed
	// by the incoming after cursor so pagination can be exercised.
	matchesJSON   string
	matchesCursor string
	// matchCursors overrides matchesCursor per incoming after cursor. An empty
	// mapped value terminates pagination.
	matchCursors map[string]string
	matchPages   map[string]string
	marketID     int64
	// placed flips once a POST /v1/orders succeeds, so a lookup for an order the
	// exchange never accepted answers 404 like the real API does.
	placed bool
	// statusHooks injects a non-2xx status for the n-th POST /v1/orders.
	statusHooks []int

	orderAttempts int
	orderBodies   []CreateOrderRequest
	lastOrder     *ContractOrder
	authPosts     int
	messageCalls  int
}

func newFakeExchange() *fakeExchange {
	return &fakeExchange{
		marketJSON:        testMarketJSON,
		bookJSON:          `{"marketId": 42, "asks": [[0.5, 100000000000000000000]], "bids": [[0.48, 100000000000000000000]]}`,
		category:          map[string]string{"crypto": testEndsAt},
		orderID:           "ord-1",
		orderHash:         "0x" + strings.Repeat("11", 32),
		orderStatus:       "FILLED",
		orderAmount:       "100000000000000000000",
		orderAmountFilled: "0",
		matchesJSON:       "[]",
		marketID:          42,
	}
}

func (f *fakeExchange) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/markets", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, `{"success":true,"cursor":"","data":[`+f.marketJSON+`]}`)
	})
	mux.HandleFunc("/v1/markets/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/orderbook") {
			writeJSON(w, `{"success":true,"data":`+f.bookJSON+`}`)
			return
		}
		writeJSON(w, `{"success":true,"data":`+f.marketJSON+`}`)
	})
	mux.HandleFunc("/v1/categories/", func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/v1/categories/")
		f.mu.Lock()
		endsAt, ok := f.category[slug]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, `{"success":true,"data":{"slug":"`+slug+`","endsAt":"`+endsAt+`","markets":[{"id":42}],"tags":[]}}`)
	})
	mux.HandleFunc("/v1/auth/message", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.messageCalls++
		f.mu.Unlock()
		writeJSON(w, `{"success":true,"data":{"message":"predict.fun login: 42"}}`)
	})
	mux.HandleFunc("/v1/auth", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authPosts++
		f.mu.Unlock()
		writeJSON(w, `{"success":true,"data":{"token":"test-token"}}`)
	})
	mux.HandleFunc("/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			io.WriteString(w, `{"success":false,"message":"content type must be application/json"}`)
			return
		}
		var body CreateOrderRequest
		if !decodeStrictJSON(raw, &body) || body.Data.Order == nil || body.Data.PricePerShare == "" || body.Data.Strategy == "" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"success":false,"message":"data.order required object; data.pricePerShare required string; data.strategy required MARKET|LIMIT"}`)
			return
		}
		f.mu.Lock()
		f.orderAttempts++
		attempt := f.orderAttempts
		var injected int
		if attempt <= len(f.statusHooks) {
			injected = f.statusHooks[attempt-1]
		}
		f.orderBodies = append(f.orderBodies, body)
		if body.Data.Order != nil {
			copied := *body.Data.Order
			f.lastOrder = &copied
		}
		if injected != 0 {
			f.mu.Unlock()
			w.WriteHeader(injected)
			io.WriteString(w, `{"success":false,"message":"injected"}`)
			return
		}
		id, hash := f.orderID, f.orderHash
		f.placed = true
		f.mu.Unlock()
		writeJSON(w, fmt.Sprintf(`{"success":true,"data":{"code":"OK","orderId":%q,"orderHash":%q}}`, id, hash))
	})
	mux.HandleFunc("/v1/orders/matches", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		cursor := r.URL.Query().Get("after")
		body, ok := f.matchPages[cursor]
		if !ok {
			body = f.matchesJSON
		}
		next := f.matchesCursor
		if perPage, ok := f.matchCursors[cursor]; ok {
			next = perPage
		}
		f.mu.Unlock()
		writeJSON(w, fmt.Sprintf(`{"success":true,"cursor":%q,"data":%s}`, next, body))
	})
	mux.HandleFunc("/v1/orders/remove", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body RemoveOrdersRequest
		if !decodeStrictJSON(raw, &body) || len(body.Data.IDs) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"success":false,"message":"data.ids required non-empty string array"}`)
			return
		}
		removed := []string{}
		rejected := []string{}
		for _, id := range body.Data.IDs {
			if strings.HasPrefix(id, "locked") {
				rejected = append(rejected, id)
			} else {
				removed = append(removed, id)
			}
		}
		payload, _ := json.Marshal(RemoveOrdersResponse{
			Success:  true,
			Removed:  &removed,
			Noop:     &[]string{},
			Rejected: &rejected,
		})
		writeJSON(w, string(payload))
	})
	mux.HandleFunc("/v1/orders/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		placed := f.placed
		status, id, amount, filled := f.orderStatus, f.orderID, f.orderAmount, f.orderAmountFilled
		marketID := f.marketID
		inner := ContractOrder{
			Salt: "1", Maker: "0x1111111111111111111111111111111111111111",
			Signer:  "0x1111111111111111111111111111111111111111",
			Taker:   "0x0000000000000000000000000000000000000000",
			TokenID: "111", MakerAmount: "5000000000000000000",
			TakerAmount: "10000000000000000000", Expiration: 1893456000,
			Nonce: "0", FeeRateBps: "200", Side: 0, SignatureType: 0,
			Signature: "0xsig",
		}
		if f.lastOrder != nil {
			inner = *f.lastOrder
		}
		inner.Hash = strings.TrimPrefix(r.URL.Path, "/v1/orders/")
		f.mu.Unlock()
		if !placed {
			http.NotFound(w, r)
			return
		}
		order := OrderData{
			Order: &inner, ID: id, MarketID: marketID, Currency: "USDT",
			Amount: Number(amount), AmountFilled: Number(filled),
			Strategy: StrategyLimit, Status: status,
		}
		payload, _ := json.Marshal(OrderResponse{Success: true, Data: &order})
		writeJSON(w, string(payload))
	})
	return mux
}

func (f *fakeExchange) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orderAttempts
}

func (f *fakeExchange) bodies() []CreateOrderRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]CreateOrderRequest, len(f.orderBodies))
	copy(out, f.orderBodies)
	return out
}

func (f *fakeExchange) authCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authPosts
}

// decodeStrictJSON mirrors a strict exchange: unknown fields (including the old
// top-level order DTO and orderId cancel shape) are rejected instead of ignored.
func decodeStrictJSON(raw []byte, out any) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out) == nil
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
}

// testCreateOrderRequest is a structurally valid POST /v1/orders payload. Its
// signature is not cryptographically checked by these fixtures; the executor
// tests recover the signer from a real ECDSA signature separately.
func testCreateOrderRequest() CreateOrderRequest {
	minOut := true
	return CreateOrderRequest{Data: CreateOrderData{
		Order: &ContractOrder{
			Salt:          "1",
			Maker:         "0x1111111111111111111111111111111111111111",
			Signer:        "0x1111111111111111111111111111111111111111",
			Taker:         "0x0000000000000000000000000000000000000000",
			TokenID:       "111",
			MakerAmount:   "5000000000000000000",
			TakerAmount:   "10000000000000000000",
			Expiration:    1893456000,
			Nonce:         "0",
			FeeRateBps:    "200",
			Side:          uint8(SideBuy),
			SignatureType: uint8(SignatureTypeEOA),
			Signature:     "0xsig",
		},
		PricePerShare:  "500000000000000000",
		Strategy:       StrategyLimit,
		SlippageBps:    "200",
		IsMinAmountOut: &minOut,
	}}
}

// newTestClient builds a client against a fake handler and disables the real
// backoff timer so a rate-limit test does not spend a second sleeping.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(Config{
		BaseURL:    server.URL,
		APIKey:     "test-api-key",
		PrivateKey: testKeyHex,
		Testnet:    true,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.backoff = func(context.Context, time.Duration) error { return nil }
	return client
}

// newTestExecutor wires a client, provider and executor against one fake
// exchange, loads the market metadata and counts order signatures.
func newTestExecutor(t *testing.T, exchange *fakeExchange, marketID string) (*Executor, *int) {
	t.Helper()
	client := newTestClient(t, exchange.handler())
	provider := NewProvider(client)
	provider.now = func() time.Time { return time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC) }
	executor := NewExecutor(client, provider)
	executor.now = provider.now
	if _, err := provider.Get(context.Background(), marketID); err != nil {
		t.Fatalf("load market metadata: %v", err)
	}
	signatures := 0
	sign := executor.signOrder
	executor.signOrder = func(order Order, domain OrderDomain) (string, common.Hash, error) {
		signatures++
		return sign(order, domain)
	}
	return executor, &signatures
}
