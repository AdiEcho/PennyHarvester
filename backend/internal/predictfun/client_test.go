package predictfun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMarketsReadsEnvelopeAndCursor(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/markets" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		if key := r.Header.Get("x-api-key"); key != "test-api-key" {
			t.Errorf("x-api-key = %q", key)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("public read sent Authorization: %q", auth)
		}
		writeJSON(w, `{"success":true,"cursor":"next-page","data":[`+testMarketJSON+`]}`)
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-api-key", Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	markets, cursor, err := client.Markets(context.Background(), MarketQuery{Status: statusOpen})
	if err != nil {
		t.Fatalf("Markets: %v", err)
	}
	if cursor != "next-page" {
		t.Fatalf("cursor = %q", cursor)
	}
	if len(markets) != 1 {
		t.Fatalf("markets = %d", len(markets))
	}
	market := markets[0]
	if market.ID.String() != "42" || market.ConditionID != "0xcond42" {
		t.Fatalf("market = %+v", market)
	}
	if market.FeeRateBps.String() != "200" || market.DecimalPrecision.String() != "2" {
		t.Fatalf("fee/precision = %s/%s", market.FeeRateBps, market.DecimalPrecision)
	}
	if len(market.Outcomes) != 2 || market.Outcomes[0].OnChainID != "111" {
		t.Fatalf("outcomes = %+v", market.Outcomes)
	}
	if market.Outcomes[1].BestAsk.Price.String() != "0.52" {
		t.Fatalf("outcome ask = %s", market.Outcomes[1].BestAsk.Price)
	}
	if !strings.Contains(gotQuery, "first=100") || !strings.Contains(gotQuery, "status=OPEN") {
		t.Fatalf("query = %q", gotQuery)
	}
}

func TestAPIErrorCarriesStatusAndTruncatedBody(t *testing.T) {
	body := strings.Repeat("x", 500)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, body)
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Market(context.Background(), "42")
	if err == nil {
		t.Fatal("expected an error")
	}
	message := err.Error()
	if !strings.Contains(message, "HTTP 500") {
		t.Fatalf("error without status: %s", message)
	}
	if strings.Contains(message, strings.Repeat("x", 201)) {
		t.Fatalf("error body was not truncated: %d chars", len(message))
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("error type = %T", err)
	}
}

func TestSuccessFalseIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"success":false,"message":"invalid api key"}`)
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "bad", Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _, err = client.Markets(context.Background(), MarketQuery{})
	if err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 200") {
		t.Fatalf("success:false error should still name the status: %v", err)
	}
}

func TestAuthRefreshRetriesOnceWithSamePayload(t *testing.T) {
	var (
		mu          sync.Mutex
		orderCalls  int
		authPosts   int
		attemptBody [][]byte
		attemptAuth []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/auth/message":
			writeJSON(w, `{"success":true,"data":{"message":"login challenge"}}`)
		case r.URL.Path == "/v1/auth":
			mu.Lock()
			authPosts++
			mu.Unlock()
			writeJSON(w, `{"success":true,"data":{"token":"test-token"}}`)
		case r.URL.Path == "/v1/orders":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			orderCalls++
			calls := orderCalls
			attemptBody = append(attemptBody, body)
			attemptAuth = append(attemptAuth, r.Header.Get("Authorization"))
			mu.Unlock()
			if calls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"success":false,"message":"token expired"}`)
				return
			}
			writeJSON(w, `{"success":true,"data":{"id":"ord-9","status":"MATCHED"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "key", PrivateKey: testKeyHex, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ack, err := client.PlaceOrder(context.Background(), SignedOrderRequest{
		Order: SignedOrder{TokenID: "111", Side: uint8(SideBuy), Signature: "0xsig"},
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if ack.IDString() != "ord-9" {
		t.Fatalf("ack = %+v", ack)
	}
	mu.Lock()
	defer mu.Unlock()
	if orderCalls != 2 {
		t.Fatalf("order calls = %d, want 2", orderCalls)
	}
	if authPosts != 2 {
		t.Fatalf("auth posts = %d, want 2 (initial + refresh)", authPosts)
	}
	if string(attemptBody[0]) != string(attemptBody[1]) {
		t.Fatalf("retry payload changed:\n%s\n%s", attemptBody[0], attemptBody[1])
	}
	if attemptAuth[0] != "Bearer test-token" || attemptAuth[1] != "Bearer test-token" {
		t.Fatalf("authorization headers = %v", attemptAuth)
	}
}

func TestRateLimitBacksOffOnce(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		attempt := calls
		mu.Unlock()
		if attempt == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"success":false,"message":"slow down"}`)
			return
		}
		writeJSON(w, `{"success":true,"cursor":"","data":[]}`)
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.backoff = func(context.Context, time.Duration) error { return nil }
	if _, _, err := client.Markets(context.Background(), MarketQuery{}); err != nil {
		t.Fatalf("Markets after backoff: %v", err)
	}
	mu.Lock()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	calls = 0
	mu.Unlock()

	// A second consecutive 429 must fail instead of looping.
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"success":false,"message":"slow down"}`)
	}))
	defer limited.Close()
	client2, err := NewClient(Config{BaseURL: limited.URL, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client2.backoff = func(context.Context, time.Duration) error { return nil }
	_, _, err = client2.Markets(context.Background(), MarketQuery{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("rate-limited calls = %d, want 2 (one retry)", calls)
	}
}

func TestPlaceOrderDoesNotRetryServerError(t *testing.T) {
	exchange := newFakeExchange()
	exchange.statusHooks = []int{http.StatusInternalServerError}
	client := newTestClient(t, exchange.handler())
	_, err := client.PlaceOrder(context.Background(), SignedOrderRequest{
		Order: SignedOrder{TokenID: "111", Side: uint8(SideBuy), Signature: "0xsig"},
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("error = %v", err)
	}
	if attempts := exchange.attempts(); attempts != 1 {
		t.Fatalf("a 5xx POST was retried: %d attempts", attempts)
	}
}

func TestCancelOrderPostsOrderID(t *testing.T) {
	var body map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/message":
			writeJSON(w, `{"success":true,"data":{"message":"login"}}`)
		case "/v1/auth":
			writeJSON(w, `{"success":true,"data":{"token":"test-token"}}`)
		case "/v1/orders/remove":
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("cancel body: %v", err)
			}
			writeJSON(w, `{"success":true,"data":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "key", PrivateKey: testKeyHex, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.CancelOrder(context.Background(), "ord-1"); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if body["orderId"] != "ord-1" {
		t.Fatalf("cancel body = %v", body)
	}
}

func TestOrderNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"success":false,"message":"order not found"}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "key", PrivateKey: testKeyHex, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.backoff = func(context.Context, time.Duration) error { return nil }
	_, err = client.Order(context.Background(), "missing")
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false", err)
	}
}

func TestOrderbookParsesArraysAndInvertsForNo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/markets/42/orderbook" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, `{"success":true,"data":{
			"marketId": 42,
			"updateTimestampMs": 1700000000000,
			"asks": [[0.333, 5000000000000000000], [0.32, 1000000000000000000]],
			"bids": [[0.31, 2000000000000000000], [0.30, 4000000000000000000]]
		}}`)
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	book, err := client.Orderbook(context.Background(), "42")
	if err != nil {
		t.Fatalf("Orderbook: %v", err)
	}
	if book.MarketID.String() != "42" || len(book.Asks) != 2 || len(book.Bids) != 2 {
		t.Fatalf("book = %+v", book)
	}

	yes := BookForOutcome(book, OutcomeYes, 0.01)
	if price, ok := yes.BestAsk(); !ok || price != 0.32 {
		t.Fatalf("Yes best ask = %v (%v)", price, ok)
	}
	if price, ok := yes.BestBid(); !ok || price != 0.31 {
		t.Fatalf("Yes best bid = %v (%v)", price, ok)
	}

	// No is 1-p with the depth mirrored: Yes bids become No asks.
	no := BookForOutcome(book, OutcomeNo, 0.01)
	if price, ok := no.BestAsk(); !ok || price != 0.69 {
		t.Fatalf("No best ask = %v (%v), want 0.69 (1 - best Yes bid)", price, ok)
	}
	if no.Asks[0].Size != 2 {
		t.Fatalf("No best ask size = %v, want the Yes bid size 2", no.Asks[0].Size)
	}
	if price, ok := no.BestBid(); !ok || price != 0.68 {
		t.Fatalf("No best bid = %v (%v), want 0.68 (1 - best Yes ask 0.32)", price, ok)
	}
	if no.Bids[0].Size != 1 {
		t.Fatalf("No best bid size = %v, want the Yes ask size 1", no.Bids[0].Size)
	}
	if len(no.Asks) != 2 || no.Asks[1].Price != 0.70 {
		t.Fatalf("No asks are not sorted ascending: %+v", no.Asks)
	}
	if len(no.Bids) != 2 || no.Bids[1].Price != 0.66 || no.Bids[1].Size != 5 {
		t.Fatalf("No bids are not sorted descending: %+v", no.Bids)
	}
}

func TestBookLevelAcceptsObjectForm(t *testing.T) {
	var level Level
	if err := json.Unmarshal([]byte(`{"price":"0.4","size":"12"}`), &level); err != nil {
		t.Fatalf("object level: %v", err)
	}
	if level.Price.String() != "0.4" || level.Size.String() != "12" {
		t.Fatalf("level = %+v", level)
	}
}
