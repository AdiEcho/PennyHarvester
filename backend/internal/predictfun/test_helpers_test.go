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

	orderID     string
	orderStatus string
	// placed flips once a POST /v1/orders succeeds, so a lookup for an order the
	// exchange never accepted answers 404 like the real API does.
	placed bool
	// statusHooks injects a non-2xx status for the n-th POST /v1/orders.
	statusHooks []int

	orderAttempts int
	orderBodies   []SignedOrderRequest
	authPosts     int
	messageCalls  int
}

func newFakeExchange() *fakeExchange {
	return &fakeExchange{
		marketJSON:  testMarketJSON,
		bookJSON:    `{"marketId": 42, "asks": [[0.5, 100000000000000000000]], "bids": [[0.48, 100000000000000000000]]}`,
		category:    map[string]string{"crypto": testEndsAt},
		orderID:     "ord-1",
		orderStatus: "MATCHED",
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
		f.mu.Lock()
		f.orderAttempts++
		attempt := f.orderAttempts
		var injected int
		if attempt <= len(f.statusHooks) {
			injected = f.statusHooks[attempt-1]
		}
		var body SignedOrderRequest
		if err := json.Unmarshal(raw, &body); err == nil {
			f.orderBodies = append(f.orderBodies, body)
		}
		if injected != 0 {
			f.mu.Unlock()
			w.WriteHeader(injected)
			io.WriteString(w, `{"success":false,"message":"injected"}`)
			return
		}
		status, id := f.orderStatus, f.orderID
		f.placed = true
		f.mu.Unlock()
		writeJSON(w, fmt.Sprintf(`{"success":true,"data":{"id":%q,"status":%q}}`, id, status))
	})
	mux.HandleFunc("/v1/orders/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		status, id, placed := f.orderStatus, f.orderID, f.placed
		f.mu.Unlock()
		if !placed {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, fmt.Sprintf(`{"success":true,"data":{"id":%q,"status":%q,"pricePerShare":"0.5"}}`, id, status))
	})
	return mux
}

func (f *fakeExchange) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orderAttempts
}

func (f *fakeExchange) bodies() []SignedOrderRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SignedOrderRequest, len(f.orderBodies))
	copy(out, f.orderBodies)
	return out
}

func (f *fakeExchange) authCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authPosts
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
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
