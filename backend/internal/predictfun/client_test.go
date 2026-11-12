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

func TestMarketsSendsAfterNotCursor(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		writeJSON(w, `{"success":true,"data":[]}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Testnet: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Markets(context.Background(), MarketQuery{Cursor: "page-2"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "after=page-2") || strings.Contains(gotQuery, "cursor=") {
		t.Fatalf("pagination query = %q, want after=", gotQuery)
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
			writeJSON(w, `{"success":true,"data":{"code":"OK","orderId":"ord-9","orderHash":"0x99"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "key", PrivateKey: testKeyHex, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ack, err := client.PlaceOrder(context.Background(), testCreateOrderRequest())
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if ack.OrderID != "ord-9" {
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
	_, err := client.PlaceOrder(context.Background(), testCreateOrderRequest())
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("error = %v", err)
	}
	if attempts := exchange.attempts(); attempts != 1 {
		t.Fatalf("a 5xx POST was retried: %d attempts", attempts)
	}
}

func TestCancelOrderPostsNestedIDsAndHonoursOutcomes(t *testing.T) {
	var body map[string]any
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
			writeJSON(w, `{"success":true,"removed":["ord-1"],"noop":["ord-2"],"rejected":["locked-3"]}`)
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
		t.Fatalf("CancelOrder removed: %v", err)
	}
	if err := client.CancelOrder(context.Background(), "ord-2"); err != nil {
		t.Fatalf("CancelOrder noop: %v", err)
	}
	err = client.CancelOrder(context.Background(), "locked-3")
	var rejected *CancelRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("CancelOrder rejected error = %v, want CancelRejectedError", err)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("cancel body = %v, want a data object", body)
	}
	ids, ok := data["ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "locked-3" {
		t.Fatalf("cancel ids = %v, want [locked-3]", data["ids"])
	}
	if _, exists := body["orderId"]; exists {
		t.Fatalf("old orderId field was sent: %v", body)
	}
}

func TestCancelOrdersReportsMixedResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/message":
			writeJSON(w, `{"success":true,"data":{"message":"login"}}`)
		case "/v1/auth":
			writeJSON(w, `{"success":true,"data":{"token":"test-token"}}`)
		default:
			writeJSON(w, `{"success":true,"removed":["a"],"noop":["b"],"rejected":["c"]}`)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "key", PrivateKey: testKeyHex, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	response, err := client.CancelOrders(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("CancelOrders: %v", err)
	}
	if response.Removed == nil || response.Noop == nil || response.Rejected == nil {
		t.Fatalf("response = %+v, want all three outcome lists", response)
	}
	if !containsString(*response.Removed, "a") || !containsString(*response.Noop, "b") || !containsString(*response.Rejected, "c") {
		t.Fatalf("response outcomes = %+v", response)
	}
	if _, err := client.CancelOrders(context.Background(), nil); err == nil {
		t.Fatal("an empty ids request must be rejected locally")
	}
}

func TestCancelOrderRequiresTheRequestedIDInTheResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"success":true,"removed":[],"noop":[],"rejected":[]}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.CancelOrder(context.Background(), "missing"); err == nil {
		t.Fatal("an outcome that omits the requested id must not be treated as success")
	}
}

func TestPlaceOrderPostsTheOfficialNestedContract(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/message":
			writeJSON(w, `{"success":true,"data":{"message":"login"}}`)
		case "/v1/auth":
			writeJSON(w, `{"success":true,"data":{"token":"test-token"}}`)
		default:
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			writeJSON(w, `{"success":true,"data":{"code":"OK","orderId":"ord-1","orderHash":"0x1"}}`)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "key", PrivateKey: testKeyHex, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.PlaceOrder(context.Background(), testCreateOrderRequest()); err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("body = %v, want data object", body)
	}
	if _, ok := data["order"].(map[string]any); !ok {
		t.Fatalf("data.order = %v, want the ContractOrder object", data["order"])
	}
	if data["pricePerShare"] != "500000000000000000" {
		t.Fatalf("data.pricePerShare = %v", data["pricePerShare"])
	}
	if data["strategy"] != "LIMIT" {
		t.Fatalf("data.strategy = %v", data["strategy"])
	}
	if data["slippageBps"] != "200" {
		t.Fatalf("data.slippageBps = %v, want a string", data["slippageBps"])
	}
	for _, old := range []string{"salt", "maker", "side", "pricePerShare", "slippageBps"} {
		if _, ok := body[old]; ok {
			t.Fatalf("old top-level %q field was sent: %v", old, body)
		}
	}
}

func TestPlaceOrderRejectsMissingRequiredContractFields(t *testing.T) {
	client, err := NewClient(Config{BaseURL: "http://127.0.0.1:1", Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	missingOrder := testCreateOrderRequest()
	missingOrder.Data.Order = nil
	missingPrice := testCreateOrderRequest()
	missingPrice.Data.PricePerShare = ""
	missingStrategy := testCreateOrderRequest()
	missingStrategy.Data.Strategy = ""
	numericSlippage := testCreateOrderRequest()
	numericSlippage.Data.SlippageBps = "abc"
	for name, request := range map[string]CreateOrderRequest{
		"order": missingOrder, "price": missingPrice, "strategy": missingStrategy, "slippage": numericSlippage,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := client.PlaceOrder(context.Background(), request); err == nil {
				t.Fatal("invalid create request was accepted locally")
			}
		})
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

func TestStrictFixtureRejectsOldCreateAndCancelShapes(t *testing.T) {
	exchange := newFakeExchange()
	client := newTestClient(t, exchange.handler())

	oldCreate := map[string]any{
		"data": map[string]any{
			"salt": "1", "maker": "0x1111111111111111111111111111111111111111",
			"signer":  "0x1111111111111111111111111111111111111111",
			"taker":   "0x0000000000000000000000000000000000000000",
			"tokenId": "111", "makerAmount": "5000000000000000000",
			"takerAmount": "10000000000000000000", "expiration": 1893456000,
			"nonce": "0", "feeRateBps": "200", "side": 0, "signatureType": 0, "signature": "0xsig",
		},
		"pricePerShare": "500000000000000000", "slippageBps": 200, "isMinAmountOut": true,
	}
	var createResponse CreateOrderResponse
	if _, err := client.roundTrip(context.Background(), apiRequest{
		method: http.MethodPost, path: "/v1/orders", body: oldCreate, authed: true, whole: true,
	}, &createResponse); err == nil || !isAPIStatus(err, http.StatusBadRequest) {
		t.Fatalf("old create shape error = %v, want HTTP 400 from the strict fixture", err)
	}

	oldCancel := map[string]any{"orderId": "ord-1"}
	var cancelResponse RemoveOrdersResponse
	if _, err := client.roundTrip(context.Background(), apiRequest{
		method: http.MethodPost, path: "/v1/orders/remove", body: oldCancel, authed: true, whole: true,
	}, &cancelResponse); err == nil || !isAPIStatus(err, http.StatusBadRequest) {
		t.Fatalf("old cancel shape error = %v, want HTTP 400 from the strict fixture", err)
	}
}

func isAPIStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

func TestOrderReadsNestedOrderWhenOuterIDIsPresent(t *testing.T) {
	exchange := newFakeExchange()
	exchange.placed = true
	exchange.orderStatus = "OPEN"
	exchange.orderAmountFilled = "40000000000000000000"
	exchange.lastOrder = &ContractOrder{
		Salt: "7", Maker: "0x2222222222222222222222222222222222222222",
		Signer:  "0x2222222222222222222222222222222222222222",
		Taker:   "0x0000000000000000000000000000000000000000",
		TokenID: "222", MakerAmount: "30000000000000000000",
		TakerAmount: "100000000000000000000", Expiration: 1893456000,
		Nonce: "3", FeeRateBps: "150", Side: uint8(SideSell), SignatureType: 0,
		Signature: "0xsig",
	}
	client := newTestClient(t, exchange.handler())
	order, err := client.Order(context.Background(), exchange.orderHash)
	if err != nil {
		t.Fatalf("Order: %v", err)
	}
	if order.ID != exchange.orderID || order.Status != "OPEN" || order.AmountFilled.String() != exchange.orderAmountFilled {
		t.Fatalf("outer order fields = %+v", order)
	}
	if order.Order == nil {
		t.Fatal("nested order was dropped because the outer id was present")
	}
	if order.Order.Side != uint8(SideSell) || order.Order.MakerAmount != "30000000000000000000" || order.Order.TakerAmount != "100000000000000000000" {
		t.Fatalf("nested order = %+v", order.Order)
	}
	if order.Order.Hash != exchange.orderHash {
		t.Fatalf("inner hash = %q, want %q", order.Order.Hash, exchange.orderHash)
	}
}

func TestAllMatchesForOrderFollowsCursorAndStopsOnCycle(t *testing.T) {
	pageOne := `[{"transactionHash":"0xtx1","taker":{"hash":"0xa","amount":"10","price":"0.4","fee":{"amount":"0","type":"SHARES"}}}]`
	pageTwo := `[{"transactionHash":"0xtx2","taker":{"hash":"0xa","amount":"20","price":"0.6","fee":{"amount":"0","type":"SHARES"}}}]`

	exchange := newFakeExchange()
	exchange.matchPages = map[string]string{"": pageOne, "p2": pageTwo}
	exchange.matchCursors = map[string]string{"": "p2", "p2": ""}
	client := newTestClient(t, exchange.handler())
	matches, complete, err := client.AllMatchesForOrder(context.Background(), exchange.orderHash)
	if err != nil {
		t.Fatalf("AllMatchesForOrder: %v", err)
	}
	if !complete || len(matches) != 2 {
		t.Fatalf("matches/complete = %d/%v, want 2/true", len(matches), complete)
	}

	exchange.matchesCursor = ""
	exchange.matchCursors = map[string]string{"": "p2", "p2": "p2"}
	matches, complete, err = client.AllMatchesForOrder(context.Background(), exchange.orderHash)
	if err != nil {
		t.Fatalf("AllMatchesForOrder cycle: %v", err)
	}
	if complete || len(matches) != 2 {
		t.Fatalf("repeated cursor matches/complete = %d/%v, want 2/false", len(matches), complete)
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

func TestApplyDuringAuthenticationDropsTheOldToken(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/message":
			writeJSON(w, `{"success":true,"data":{"message":"login challenge"}}`)
		case "/v1/auth":
			close(started)
			<-release
			writeJSON(w, `{"success":true,"data":{"token":"old-wallet-token"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, APIKey: "key", PrivateKey: testKeyHex, Testnet: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Authenticate(context.Background()) }()
	<-started
	if err := client.SetBaseURL(server.URL + "/v2"); err != nil {
		t.Fatalf("switch configuration: %v", err)
	}
	close(release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "配置已变更") {
		t.Fatalf("Authenticate error = %v, want a generation mismatch", err)
	}
	client.mu.Lock()
	token := client.token
	client.mu.Unlock()
	if token != "" {
		t.Fatalf("token from the old configuration was cached: %q", token)
	}
}
