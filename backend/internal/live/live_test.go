package live

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/ethereum/go-ethereum/crypto"

	"pennyharvester/backend/internal/config"
	"pennyharvester/backend/internal/engine"
)

func TestExecutorConfigurationValidation(t *testing.T) {
	if New(Config{}).Configured() {
		t.Fatal("an executor without a private key must not be configured")
	}
	if !strings.Contains(New(Config{}).ReadyError(), "POLYMARKET_PRIVATE_KEY") {
		t.Fatal("missing private key should be reported")
	}

	key := "0x" + strings.Repeat("1", 64)
	proxy := New(Config{PrivateKey: key, SignatureType: 1})
	if !proxy.Configured() {
		t.Fatal("private key should mark the executor configured")
	}
	if !strings.Contains(proxy.ReadyError(), "POLYMARKET_FUNDER") {
		t.Fatalf("proxy signature type without funder should fail: %s", proxy.ReadyError())
	}

	invalidType := New(Config{PrivateKey: key, SignatureType: 4})
	if !strings.Contains(invalidType.ReadyError(), "SIGNATURE_TYPE") {
		t.Fatalf("invalid signature type should fail: %s", invalidType.ReadyError())
	}

	valid := New(Config{PrivateKey: key, SignatureType: 0})
	if err := valid.ReadyError(); err != "" {
		t.Fatalf("valid EOA config should be ready: %s", err)
	}
	if valid.Signer() == "" {
		t.Fatal("signer address should be derived locally")
	}
}

func TestExecutorRejectsEOAFunderAndPartialCredentials(t *testing.T) {
	key := "0x" + strings.Repeat("1", 64)
	eoaWithFunder := New(Config{PrivateKey: key, SignatureType: 0, Funder: "0x0000000000000000000000000000000000000002"})
	if !strings.Contains(eoaWithFunder.ReadyError(), "POLYMARKET_FUNDER") {
		t.Fatalf("EOA with funder should fail: %s", eoaWithFunder.ReadyError())
	}
	partialCredentials := New(Config{PrivateKey: key, SignatureType: 0, APIKey: "only-key"})
	if !strings.Contains(partialCredentials.ReadyError(), "必须同时设置") {
		t.Fatalf("partial L2 credentials should fail: %s", partialCredentials.ReadyError())
	}
	badFunder := New(Config{PrivateKey: key, SignatureType: 1, Funder: "not-an-address"})
	if !strings.Contains(badFunder.ReadyError(), "有效地址") {
		t.Fatalf("invalid funder should fail: %s", badFunder.ReadyError())
	}
}

func TestProxyMakerUsesFunder(t *testing.T) {
	key := "0x" + strings.Repeat("1", 64)
	funder := "0x0000000000000000000000000000000000000002"
	proxy := New(Config{PrivateKey: key, SignatureType: 1, Funder: funder})
	if err := proxy.ReadyError(); err != "" {
		t.Fatalf("valid proxy config should be ready: %s", err)
	}
	if !strings.EqualFold(proxy.Maker(), funder) {
		t.Fatalf("maker should be the funder, got %s", proxy.Maker())
	}
	if proxy.Signer() == "" || strings.EqualFold(proxy.Signer(), funder) {
		t.Fatalf("signer should be the EOA, got %s", proxy.Signer())
	}
}

func TestHardcodedBuilderCodeIsValid(t *testing.T) {
	// The builder code is compiled in, so a typo would only surface when the
	// first live order is signed; fail the build's tests instead.
	executor := New(Config{PrivateKey: "0x" + strings.Repeat("1", 64), SignatureType: 0, BuilderCode: config.BuilderCode})
	if err := executor.ReadyError(); err != "" {
		t.Fatalf("hardcoded builder code must be valid: %s", err)
	}
}

func TestExecutorRejectsInvalidBuilderCode(t *testing.T) {
	key := "0x" + strings.Repeat("1", 64)
	for _, code := range []string{"0x1234", "not-a-code", "0x" + strings.Repeat("z", 64)} {
		invalid := New(Config{PrivateKey: key, SignatureType: 0, BuilderCode: code})
		if !strings.Contains(invalid.ReadyError(), "开发者码") {
			t.Fatalf("builder code %q should be rejected: %s", code, invalid.ReadyError())
		}
	}
	// An empty code is valid: orders are simply left unattributed.
	empty := New(Config{PrivateKey: key, SignatureType: 0})
	if err := empty.ReadyError(); err != "" {
		t.Fatalf("an empty builder code must be allowed: %s", err)
	}
}

func TestApplyFeeReportsTakerFeeSeparately(t *testing.T) {
	fill := engine.LiveFill{Shares: 100, Cost: 98, Price: 0.98, Status: "matched"}
	if err := applyFee(clobclient.FeeSchedule{Rate: "0.07", Exponent: "1", TakerOnly: true}, &fill); err != nil {
		t.Fatal(err)
	}
	wantFee := 0.07 * 100 * 0.98 * 0.02
	if math.Abs(fill.Fee-wantFee) > 1e-6 {
		t.Fatalf("unexpected fee: got %f want %f", fill.Fee, wantFee)
	}
	// Cost stays the gross notional: the engine folds the fee in exactly once,
	// so an executor that also folded it here would double-charge the ledger.
	if math.Abs(fill.Cost-98) > 1e-6 {
		t.Fatalf("cost must stay the gross notional: %+v", fill)
	}
	// Price must stay the execution price: the fee is carried separately so a
	// live entry price is comparable to a paper entry price.
	if fill.Price != 0.98 {
		t.Fatalf("entry price must not absorb the fee: %+v", fill)
	}
}

func TestApplyFeeFailsOnImpossibleRate(t *testing.T) {
	fill := engine.LiveFill{Shares: 100, Cost: 98, Price: 0.98}
	if err := applyFee(clobclient.FeeSchedule{Rate: "2.5", Exponent: "1", TakerOnly: true}, &fill); err == nil {
		t.Fatal("an out-of-range fee rate must not be booked as a valid fee")
	}
	if fill.Fee != 0 || fill.Cost != 98 {
		t.Fatalf("a rejected fee must leave the fill untouched: %+v", fill)
	}
}

func TestEstimateBuyFeeRejectsInvalidInput(t *testing.T) {
	executor := New(Config{})
	for _, price := range []float64{0, 1, 1.2} {
		if _, err := executor.EstimateBuyFee(context.Background(), "condition-1", 100, price); err == nil {
			t.Fatalf("price %f must not produce a zero fee", price)
		}
	}
	if _, err := executor.EstimateBuyFee(context.Background(), "", 100, 0.98); err == nil {
		t.Fatal("a missing condition_id must not produce a zero fee")
	}
}

func TestQuoteReadsBookAndFeeWithoutCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/book":
			_, _ = w.Write([]byte(`{"asset_id":"token-1","market":"condition-1","bids":[],"asks":[{"price":"0.99","size":"100"},{"price":"0.98","size":"100"}],"min_order_size":"5","tick_size":"0.01","neg_risk":false}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// A quote is public market data: it must not require a wallet key.
	executor := New(Config{CLOBURL: server.URL})
	if executor.Configured() {
		t.Fatal("test executor must not be configured")
	}
	quote, err := executor.Quote(context.Background(), "token-1", "condition-1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if quote.BestAsk != 0.98 || quote.LimitPrice != 0.99 {
		t.Fatalf("unexpected book prices: %+v", quote)
	}
	if !quote.FullyFilled || quote.Shares != 101.0101 {
		t.Fatalf("unexpected full quote: %+v", quote)
	}
	// 100 shares fill at 0.98 and only the remainder at 0.99, so the average
	// execution price sits just above the best ask.
	if math.Abs(quote.FillPrice-0.9801) > 1e-6 || math.Abs(quote.Notional-98.999999) > 1e-6 {
		t.Fatalf("every level must be priced at its own ask: %+v", quote)
	}
	if quote.Fee <= 0 || quote.Fee >= quote.Notional*maxFeeRate {
		t.Fatalf("unexpected fee: %+v", quote)
	}

	partial, err := executor.Quote(context.Background(), "token-1", "condition-1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if partial.FullyFilled || math.Abs(partial.AvailableNotional-197) > 1e-9 {
		t.Fatalf("a thin book must quote the available depth: %+v", partial)
	}
}

func TestQuoteAllowsDollarAmountBelowMinShares(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/book":
			// min_order_size is the five-share floor for resting limit orders;
			// a $2 marketable BUY at 0.98 only buys 2.04 shares and the web UI
			// fills it, so the quote must not reject it.
			_, _ = w.Write([]byte(`{"asset_id":"token-1","market":"condition-1","bids":[],"asks":[{"price":"0.98","size":"1000"}],"min_order_size":"5","tick_size":"0.01","neg_risk":false}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := New(Config{CLOBURL: server.URL})
	quote, err := executor.Quote(context.Background(), "token-1", "condition-1", 2)
	if err != nil {
		t.Fatalf("a $2 marketable buy must quote: %v", err)
	}
	// The signed maker side is floored to cents. At a 0.98 limit that gives
	// 1.99/2.0408 = 0.9751, below the ask, so the signed limit moves one tick up
	// and the shares are resized to keep the spend within budget.
	if math.Abs(quote.Shares-2.0202) > 1e-9 || quote.LimitPrice != 0.99 {
		t.Fatalf("shares must be sized from the dollars and the signed tick: %+v", quote)
	}
	if !quote.FullyFilled || quote.Fee <= 0 {
		t.Fatalf("unexpected quote: %+v", quote)
	}
}

func TestQuoteRejectsBelowMinNotional(t *testing.T) {
	// The exchange floors a FAK buy's USDC side to cents and applies its $1
	// minimum to that signed amount, so a $1.00 budget at a 0.983 ask signs as
	// $0.99 and must be refused here instead of by the exchange.
	cases := []struct {
		ask    string
		amount float64
	}{{"0.98", 0.5}, {"0.983", 1}}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/book":
				_, _ = w.Write([]byte(`{"asset_id":"token-1","market":"condition-1","bids":[],"asks":[{"price":"` + tc.ask + `","size":"1000"}],"min_order_size":"5","tick_size":"0.001","neg_risk":false}`))
			case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
				_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
			default:
				http.NotFound(w, r)
			}
		}))
		executor := New(Config{CLOBURL: server.URL})
		_, err := executor.Quote(context.Background(), "token-1", "condition-1", tc.amount)
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "最小下单额") {
			t.Fatalf("ask %s amount %v must be rejected on the dollar floor: %v", tc.ask, tc.amount, err)
		}
	}
}
func TestLookupOrderReportsFeeWithoutBookingSide(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.98"}`))
		case r.URL.Path == "/data/trades":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"trade-1","taker_order_id":"order-1","price":"0.98","size":"100","status":"CONFIRMED"}],"next_cursor":""}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       server.URL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
	})
	if err := executor.ReadyError(); err != "" {
		t.Fatal(err)
	}
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || fill.Shares != 100 || fill.Fee <= 0 {
		t.Fatalf("unexpected reconciled fill: %+v found=%v", fill, found)
	}
	// Reconciliation reports the gross notional and the fee separately: the
	// engine adds the fee for a buy and subtracts it for a sell.
	if math.Abs(fill.Cost-98) > 1e-9 || fill.Fee <= 0 {
		t.Fatalf("reconciled fill must expose gross cost and fee separately: %+v", fill)
	}
	fee, err := executor.EstimateBuyFee(context.Background(), "condition-1", 98, 0.98)
	if err != nil || fee <= 0 {
		t.Fatalf("unexpected estimated fee: %v %v", fee, err)
	}
}

// TestLookupOrderUsesRealFillPrices covers the reason /data/trades is read: the
// order response only carries the FAK limit price, so an order that filled at
// better prices must be booked at what its matches actually paid.
func TestLookupOrderUsesRealFillPrices(t *testing.T) {
	var tradeQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			// The exchange reports the tick-ceiled limit price, not the average.
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.99"}`))
		case r.URL.Path == "/data/trades":
			tradeQuery = r.URL.RawQuery
			if r.Header.Get("POLY_API_KEY") == "" || r.Header.Get("POLY_SIGNATURE") == "" || r.Header.Get("POLY_ADDRESS") == "" {
				t.Errorf("trades read must be L2 authenticated: %v", r.Header)
			}
			_, _ = w.Write([]byte(`{"data":[
				{"id":"trade-1","taker_order_id":"order-1","price":"0.98","size":"60","status":"CONFIRMED"},
				{"id":"trade-2","taker_order_id":"order-1","price":"0.985","size":"40","status":"CONFIRMED"},
				{"id":"trade-3","taker_order_id":"other-order","price":"0.5","size":"900","status":"CONFIRMED"}
			],"next_cursor":""}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       server.URL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
	})
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if err != nil || !found {
		t.Fatalf("lookup failed: %v found=%v", err, found)
	}
	if !strings.Contains(tradeQuery, "market=condition-1") {
		t.Fatalf("trades must be scoped to the market: %q", tradeQuery)
	}
	// 60 × 0.98 + 40 × 0.985 = 98.2, so the average is 0.982 rather than the
	// 0.99 limit price the order response reports.
	if math.Abs(fill.Price-0.982) > 1e-9 || math.Abs(fill.Cost-98.2) > 1e-9 {
		t.Fatalf("reconciliation must use the real fills: %+v", fill)
	}
}

// TestCollateralReadsAllowanceMap covers the response contract change: a wallet
// holds one approval per exchange, so GET /balance-allowance answers with an
// `allowances` map and no scalar `allowance`. Reading the scalar alone reports
// zero and silently disables the pre-trade allowance guard.
func TestCollateralReadsAllowanceMap(t *testing.T) {
	var sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/balance-allowance" {
			http.NotFound(w, r)
			return
		}
		sawAuth = r.Header.Get("POLY_API_KEY") == "test-key" &&
			r.Header.Get("POLY_SIGNATURE") != "" &&
			r.Header.Get("POLY_ADDRESS") != "" &&
			r.Header.Get("POLY_PASSPHRASE") == "test-passphrase"
		if got := r.URL.Query().Get("asset_type"); got != "COLLATERAL" {
			t.Errorf("asset_type = %q, want COLLATERAL", got)
		}
		if got := r.URL.Query().Get("signature_type"); got != "1" {
			t.Errorf("signature_type = %q, want the wallet mode", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"balance":"12345678",
			"allowances":{"0x1111111111111111111111111111111111111111":"1000000","0x2222222222222222222222222222222222222222":"9000000"}
		}`))
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 1,
		Funder:        "0x0000000000000000000000000000000000000002",
		CLOBURL:       server.URL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
	})
	snapshot, err := executor.Collateral(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sawAuth {
		t.Fatal("the balance read must carry L2 authentication headers")
	}
	if math.Abs(snapshot.Amount-12.345678) > 1e-9 {
		t.Fatalf("balance = %v, want 12.345678", snapshot.Amount)
	}
	if math.Abs(snapshot.Allowance-9) > 1e-9 {
		t.Fatalf("allowance = %v, want the largest approval (9)", snapshot.Allowance)
	}
}

// TestCollateralFallsBackWhenL2ReadFails keeps a rejected direct read from
// breaking the balance: the client path still answers with what it can decode.
func TestCollateralFallsBackWhenL2ReadFails(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/balance-allowance" {
			requests++
			w.Header().Set("Content-Type", "application/json")
			if requests == 1 {
				// The direct L2 read is the first caller; reject it so the client
				// fallback has to answer.
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid signature"}`))
				return
			}
			_, _ = w.Write([]byte(`{"balance":"5000000","allowance":"5000000"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       server.URL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
	})
	snapshot, err := executor.Collateral(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(snapshot.Amount-5) > 1e-9 || math.Abs(snapshot.Allowance-5) > 1e-9 {
		t.Fatalf("fallback lost the balance: %+v", snapshot)
	}
}

func TestBuyPersistsSignedOrderAndReportsFee(t *testing.T) {
	const builderCode = "0x07718b719cce8cdfb015b268d90732d15f7071e9fa845bd16ec058b0ab2226eb"
	var submittedOrder string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/book":
			_, _ = w.Write([]byte(`{"asset_id":"token-1","market":"condition-1","bids":[],"asks":[{"price":"0.98","size":"1000"}],"min_order_size":"5","tick_size":"0.01","neg_risk":false}`))
		case r.URL.Path == "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		case r.URL.Path == "/neg-risk":
			_, _ = w.Write([]byte(`{"neg_risk":false}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		case r.URL.Path == "/order" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			submittedOrder = string(body)
			_, _ = w.Write([]byte(`{"success":true,"orderID":"order-1","status":"matched","takingAmount":"100","makingAmount":"98"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       server.URL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
		BuilderCode:   builderCode,
	})
	if err := executor.ReadyError(); err != "" {
		t.Fatal(err)
	}
	var signedOrderID string
	fill, err := executor.Buy(context.Background(), "token-1", "condition-1", 100, 0.99, func(orderID string) error {
		signedOrderID = orderID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if signedOrderID == "" {
		t.Fatal("onSigned must receive the signed digest before submission")
	}
	if fill.Shares != 100 || fill.Fee <= 0 {
		t.Fatalf("unexpected fill: %+v", fill)
	}
	// The executor reports the gross cost; the engine adds the fee when it books
	// the position (see TestLiveBuyBooksFeeOnce).
	if math.Abs(fill.Cost-98) > 1e-9 {
		t.Fatalf("cost must stay the gross notional: %+v", fill)
	}
	// The builder code must be inside the signed payload, otherwise the matched
	// trade is not attributed to the builder profile.
	if !strings.Contains(submittedOrder, `"builder":"`+builderCode+`"`) {
		t.Fatalf("signed order does not carry the builder code: %s", submittedOrder)
	}
}

func TestBuySizesBelowMinOrderSizeForMarketableBuy(t *testing.T) {
	var submittedOrder string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/book":
			// A five-share limit-order floor must not block a $2 marketable buy.
			_, _ = w.Write([]byte(`{"asset_id":"token-1","market":"condition-1","bids":[],"asks":[{"price":"0.98","size":"1000"}],"min_order_size":"5","tick_size":"0.01","neg_risk":false}`))
		case r.URL.Path == "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		case r.URL.Path == "/neg-risk":
			_, _ = w.Write([]byte(`{"neg_risk":false}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		case r.URL.Path == "/order" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			submittedOrder = string(body)
			_, _ = w.Write([]byte(`{"success":true,"orderID":"order-2","status":"matched","takingAmount":"2.0202","makingAmount":"1.979796"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       server.URL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
	})
	fill, err := executor.Buy(context.Background(), "token-1", "condition-1", 2, 0.99, nil)
	if err != nil {
		t.Fatalf("a $2 marketable buy must be submitted: %v", err)
	}
	if fill.Shares != 2.0202 || math.Abs(fill.Cost-1.979796) > 1e-9 {
		t.Fatalf("unexpected fill: %+v", fill)
	}
	// 2.0202 shares at the one-tick-raised 0.99 limit: the signed order must
	// carry that share count, not a padded five-share one.
	if !strings.Contains(submittedOrder, `"takerAmount":"2020200"`) {
		t.Fatalf("signed order does not carry the dollar-sized shares: %s", submittedOrder)
	}
	// The exchange compares the signed integer maker/taker ratio. It must still
	// cross the original 0.98 ask, not the 0.9751 ratio the old plan signed.
	var signed struct {
		Order struct {
			MakerAmount string `json:"makerAmount"`
			TakerAmount string `json:"takerAmount"`
		} `json:"order"`
	}
	if err := json.Unmarshal([]byte(submittedOrder), &signed); err != nil {
		t.Fatalf("decode signed order: %v", err)
	}
	maker, err := strconv.ParseFloat(signed.Order.MakerAmount, 64)
	if err != nil {
		t.Fatalf("decode maker amount: %v", err)
	}
	taker, err := strconv.ParseFloat(signed.Order.TakerAmount, 64)
	if err != nil {
		t.Fatalf("decode taker amount: %v", err)
	}
	if ratio := maker / taker; ratio < 0.98 {
		t.Fatalf("signed maker/taker ratio %v does not cross the 0.98 ask", ratio)
	}
}

func TestCeilTickDoesNotAddAFloatNoiseTick(t *testing.T) {
	cases := []struct {
		price, tick, want float64
	}{
		{0.07, 0.01, 0.07},
		{0.14, 0.01, 0.14},
		{0.28, 0.01, 0.28},
		{0.071, 0.01, 0.08},
		{0.9801, 0.001, 0.981},
	}
	for _, tc := range cases {
		if got := ceilTick(tc.price, tc.tick); math.Abs(got-tc.want) > 1e-12 {
			t.Fatalf("ceilTick(%v,%v)=%v want %v", tc.price, tc.tick, got, tc.want)
		}
	}
}

func TestSellPersistsSignedOrderAndReportsFee(t *testing.T) {
	var submittedOrder string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/book":
			_, _ = w.Write([]byte(`{"asset_id":"token-1","market":"condition-1","bids":[{"price":"0.75","size":"100"}],"asks":[],"min_order_size":"5","tick_size":"0.01","neg_risk":false}`))
		case r.URL.Path == "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		case r.URL.Path == "/neg-risk":
			_, _ = w.Write([]byte(`{"neg_risk":false}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		case r.URL.Path == "/order" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			submittedOrder = string(body)
			// A sell reverses the response fields: makingAmount is shares,
			// takingAmount is the pUSD proceeds.
			_, _ = w.Write([]byte(`{"success":true,"orderID":"sell-1","status":"matched","makingAmount":"100","takingAmount":"75"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       server.URL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
	})
	if err := executor.ReadyError(); err != "" {
		t.Fatal(err)
	}
	var signedOrderID string
	fill, err := executor.Sell(context.Background(), "token-1", "condition-1", 100, func(orderID string) error {
		signedOrderID = orderID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if signedOrderID == "" || fill.OrderID != "sell-1" {
		t.Fatalf("unexpected order identity: signed=%q fill=%+v", signedOrderID, fill)
	}
	if fill.Shares != 100 || fill.Price != 0.75 || fill.Fee <= 0 {
		t.Fatalf("unexpected sell fill: %+v", fill)
	}
	wantFee := 0.07 * 100 * 0.75 * 0.25
	if math.Abs(fill.Fee-wantFee) > 1e-9 || math.Abs(fill.Cost-75) > 1e-9 {
		t.Fatalf("sell must report gross proceeds and the fee separately: %+v wantFee=%f", fill, wantFee)
	}
	// The engine subtracts Fee exactly once. Pre-deducting it here would make the
	// wallet's proceeds %v instead of %v.
	if net := fill.Cost - fill.Fee; math.Abs(net-(75-wantFee)) > 1e-9 {
		t.Fatalf("engine net proceeds = %v, want %v", net, 75-wantFee)
	}
	if !strings.Contains(submittedOrder, `"side":"SELL"`) {
		t.Fatalf("submitted order is not a sell: %s", submittedOrder)
	}
}

func TestSellUsesTheFreshTickAfterAMarketTickChange(t *testing.T) {
	var orders []string
	var tickFetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/book":
			_, _ = w.Write([]byte(`{"asset_id":"token-1","market":"condition-1","bids":[{"price":"0.745","size":"100"}],"asks":[],"min_order_size":"5","tick_size":"0.001","neg_risk":false}`))
		case r.URL.Path == "/tick-size":
			tickFetches++
			if tickFetches == 1 {
				_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
				return
			}
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.001}`))
		case r.URL.Path == "/neg-risk":
			_, _ = w.Write([]byte(`{"neg_risk":false}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		case r.URL.Path == "/order" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			orders = append(orders, string(body))
			_, _ = w.Write([]byte(`{"success":true,"orderID":"sell-1","status":"matched","makingAmount":"100","takingAmount":"74.5"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       server.URL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
	})
	for i := 0; i < 2; i++ {
		if _, err := executor.Sell(context.Background(), "token-1", "condition-1", 100, nil); err != nil {
			t.Fatalf("sell %d: %v", i, err)
		}
	}
	if len(orders) != 2 {
		t.Fatalf("orders = %d", len(orders))
	}
	if !strings.Contains(orders[0], `"takerAmount":"74000000"`) {
		t.Fatalf("first sell should use the reported 0.01 tick: %s", orders[0])
	}
	if !strings.Contains(orders[1], `"takerAmount":"74500000"`) {
		t.Fatalf("second sell must use the refreshed 0.001 tick: %s", orders[1])
	}
}

func intPointer(value int) *int { return &value }

func TestUpdateSettingsMergesOverridesAndRejectsInvalid(t *testing.T) {
	baseKey := "0x" + strings.Repeat("1", 64)
	funder := "0x0000000000000000000000000000000000000002"
	executor := New(Config{
		PrivateKey:    baseKey,
		SignatureType: 0,
		CLOBURL:       "https://default-clob.example",
	})
	if err := executor.ReadyError(); err != "" {
		t.Fatalf("base config should be ready: %s", err)
	}

	if err := executor.UpdateSettings(engine.LiveSettings{}, "https://override-clob.example"); err != nil {
		t.Fatal(err)
	}
	if got := executor.EffectiveCLOBURL(); got != "https://override-clob.example" {
		t.Fatalf("effective CLOB URL = %q", got)
	}
	if !executor.Environment().PrivateKeySet {
		t.Fatal("the base private key should be reported as environment-provided")
	}

	// An invalid override must not replace the working configuration.
	if err := executor.UpdateSettings(engine.LiveSettings{
		Funder: funder, SignatureType: intPointer(0),
	}, ""); err == nil {
		t.Fatal("EOA with a funder must be rejected")
	}
	if got := executor.EffectiveCLOBURL(); got != "https://override-clob.example" {
		t.Fatalf("a rejected update changed the CLOB URL: %q", got)
	}
	if err := executor.UpdateSettings(engine.LiveSettings{PrivateKey: "not-a-key"}, ""); err == nil {
		t.Fatal("an invalid private key must be rejected")
	}

	// A proxy signature type with a funder is valid and changes the maker.
	if err := executor.UpdateSettings(engine.LiveSettings{
		Funder: funder, SignatureType: intPointer(1),
	}, ""); err != nil {
		t.Fatal(err)
	}
	if err := executor.ReadyError(); err != "" {
		t.Fatalf("proxy config should be ready: %s", err)
	}
	if !strings.EqualFold(executor.Maker(), funder) {
		t.Fatalf("maker = %q, want funder %q", executor.Maker(), funder)
	}

	// Explicit L2 credentials are accepted as a complete set.
	if err := executor.UpdateSettings(engine.LiveSettings{
		APIKey: "key", APISecret: "secret", APIPassphrase: "passphrase",
	}, ""); err != nil {
		t.Fatal(err)
	}
	if err := executor.ReadyError(); err != "" {
		t.Fatalf("explicit L2 credentials should be ready: %s", err)
	}
}

func TestProbeDerivesCredentialsWithoutTouchingExecutor(t *testing.T) {
	var derived bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/derive-api-key":
			derived = true
			if r.Header.Get("POLY_SIGNATURE") == "" {
				http.Error(w, `{"error":"missing L1 signature"}`, http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"apiKey":"11111111-2222-3333-4444-555555555555","secret":"c2VjcmV0","passphrase":"pass"}`))
		case "/balance-allowance":
			_, _ = w.Write([]byte(`{"balance":"12500000","allowance":"9000000"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       "https://default-clob.example",
	})
	if err := executor.ReadyError(); err != "" {
		t.Fatalf("base config should be ready: %s", err)
	}

	probe, err := executor.Probe(context.Background(), engine.LiveSettings{}, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !derived {
		t.Fatal("the probe must derive L2 credentials from the private key")
	}
	if probe.Source != "derived" {
		t.Fatalf("source = %q, want derived", probe.Source)
	}
	if probe.APIKey != "11111111…" {
		t.Fatalf("api key must be masked, got %q", probe.APIKey)
	}
	if probe.Cash != 12.5 || probe.Allowance != 9 {
		t.Fatalf("unexpected collateral snapshot: %+v", probe)
	}
	if probe.Signer == "" || !strings.EqualFold(probe.Signer, probe.Maker) {
		t.Fatalf("EOA maker must equal the signer: %+v", probe)
	}
	// Probing must not repoint the running executor at the candidate host.
	if executor.EffectiveCLOBURL() != "https://default-clob.example" {
		t.Fatalf("probe changed the effective CLOB URL: %q", executor.EffectiveCLOBURL())
	}
}

func TestProbeReportsRejectedCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/balance-allowance" {
			http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       "https://default-clob.example",
	})
	_, err := executor.Probe(context.Background(), engine.LiveSettings{
		APIKey:        "bad-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("bad-secret")),
		APIPassphrase: "bad-passphrase",
	}, server.URL)
	if err == nil {
		t.Fatal("an unauthenticated balance read must fail the probe")
	}
	if !strings.Contains(err.Error(), "USDC") && !strings.Contains(err.Error(), "401") {
		t.Fatalf("unexpected probe error: %v", err)
	}
}

func TestFunderMustNotBeTheSigner(t *testing.T) {
	key := "0x" + strings.Repeat("1", 64)
	privateKey, err := crypto.HexToECDSA(strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	signer := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	executor := New(Config{PrivateKey: key, SignatureType: 3, Funder: signer})
	if !strings.Contains(executor.ReadyError(), "不能等于") {
		t.Fatalf("using the signer as funder must be rejected: %s", executor.ReadyError())
	}
}

func TestProbeRelayerChecksKeyOwnerAndSigner(t *testing.T) {
	const relayerKey = "01967c03-b8c8-7000-8f68-8b8eaec6fd3d"
	privateKey, err := crypto.HexToECDSA(strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	signer := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/relayer/api/keys" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("RELAYER_API_KEY") != relayerKey || !strings.EqualFold(r.Header.Get("RELAYER_API_KEY_ADDRESS"), signer) {
			http.Error(w, `{"error":"invalid authorization"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"apiKey":"` + relayerKey + `","address":"` + signer + `"}]`))
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 3,
		Funder:        "0x0000000000000000000000000000000000000003",
		CLOBURL:       "https://clob.example.com",
		RelayerURL:    server.URL,
	})
	probe, err := executor.ProbeRelayer(context.Background(), engine.LiveSettings{}, "", relayerKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	if probe.KeyCount != 1 || !strings.EqualFold(probe.Address, signer) || probe.APIKey != "01967c03…" {
		t.Fatalf("unexpected relayer probe: %+v", probe)
	}

	// The key is bound to the signer, so any other address must be rejected
	// before a request is even made.
	if _, err := executor.ProbeRelayer(context.Background(), engine.LiveSettings{}, "", relayerKey, "0x0000000000000000000000000000000000000009"); err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("a mismatched signer address must fail: %v", err)
	}

	// A syntactically valid but unknown key must not be reported as working.
	otherKey := "01967c03-b8c8-7000-8f68-8b8eaec6fd99"
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"apiKey":"` + relayerKey + `","address":"` + signer + `"}]`))
	})
	if _, err := executor.ProbeRelayer(context.Background(), engine.LiveSettings{}, "", otherKey, signer); err == nil || !strings.Contains(err.Error(), "不包含") {
		t.Fatalf("an unknown key must be reported: %v", err)
	}
}

// TestL2SignatureMatchesCLOBScheme pins the HMAC construction: a wrong variant
// would still compile and only surface as a 401 from the exchange.
func TestL2SignatureMatchesCLOBScheme(t *testing.T) {
	signature, err := l2Signature(base64.URLEncoding.EncodeToString([]byte("test-secret")), 1700000000, http.MethodGet, "/balance-allowance", nil)
	if err != nil {
		t.Fatal(err)
	}
	// base64url(HMAC-SHA256("test-secret", "1700000000GET/balance-allowance")),
	// cross-checked against an independent implementation.
	if signature != "7sNNKh8J45Qt3mybPDV2Wal05yNQuc0jn4exnFlxjm8=" {
		t.Fatalf("signature = %q", signature)
	}
	if _, err := l2Signature("not-base64!", 1, http.MethodGet, "/x", nil); err == nil {
		t.Fatal("a malformed secret must fail loudly")
	}
}

func TestCollateralUnitsAcceptsNumberAndStringForm(t *testing.T) {
	// The CLOB spec shows quoted fixed-point values, but the response has also
	// carried bare JSON numbers; both must decode.
	for _, raw := range []string{
		`{"balance":"12345678","allowances":{"0x1":"9000000"}}`,
		`{"balance":12345678,"allowances":{"0x1":9000000}}`,
	} {
		var response struct {
			Balance    json.Number            `json:"balance"`
			Allowances map[string]json.Number `json:"allowances"`
		}
		if err := json.Unmarshal([]byte(raw), &response); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		balance, err := collateralUnits(response.Balance)
		if err != nil || math.Abs(balance-12.345678) > 1e-9 {
			t.Fatalf("%s: balance = %v, err = %v", raw, balance, err)
		}
		allowance, err := maxAllowance(response.Allowances)
		if err != nil || math.Abs(allowance-9) > 1e-9 {
			t.Fatalf("%s: allowance = %v, err = %v", raw, allowance, err)
		}
	}
}

// TestTradesForOrderSkipsUnrelatedAndFailedFills keeps a reconciliation from
// booking another order's fills or a failed match, and keeps a confirmed+failed
// mix from being treated as a complete fill set.
func TestTradesForOrderSkipsUnrelatedAndFailedFills(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/data/trades" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"trade-1","taker_order_id":"order-1","price":"0.98","size":"10","status":"CONFIRMED"},
			{"id":"trade-2","taker_order_id":"order-1","price":"0.98","size":"10","status":"FAILED"},
			{"id":"trade-3","taker_order_id":"order-2","price":"0.98","size":"10","status":"CONFIRMED"}
		],"next_cursor":""}`))
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	result, err := executor.tradesForOrder(context.Background(), "order-1", "condition-1", 10)
	if err == nil {
		t.Fatal("a mixed confirmed+failed fill set must not reconcile as complete")
	}
	if !strings.Contains(err.Error(), "失败 1 笔") {
		t.Fatalf("the failed leg must be surfaced: %v", err)
	}
	if result.Complete || result.ConfirmedShares != 10 || math.Abs(result.ConfirmedNotional-9.8) > 1e-9 {
		t.Fatalf("failed and unrelated fills must not contribute: %+v", result)
	}
	if result.FailedLegs != 1 || result.PendingLegs != 0 || result.ConfirmedLegs != 1 {
		t.Fatalf("unexpected leg counts: %+v", result)
	}
}

func newLookupTestExecutor(serverURL string) *Executor {
	return New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       serverURL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
	})
}

// TestNormalizeTradeStatus pins the finality mapping: only CONFIRMED is a
// successful terminal fill, every FAILED* spelling is a failed leg, and every
// other status (including MATCHED/MINED/RETRYING and unknown values) is
// non-final.
func TestNormalizeTradeStatus(t *testing.T) {
	cases := []struct {
		raw  string
		want tradeLegStatus
	}{
		{"CONFIRMED", tradeLegConfirmed},
		{"confirmed", tradeLegConfirmed},
		{" TRADE_STATUS_CONFIRMED ", tradeLegConfirmed},
		{"MATCHED", tradeLegPending},
		{"MINED", tradeLegPending},
		{"RETRYING", tradeLegPending},
		{"TRADE_STATUS_MATCHED", tradeLegPending},
		{"TRADE_STATUS_RETRYING", tradeLegPending},
		{"UNKNOWN", tradeLegPending},
		{"", tradeLegPending},
		{"FAILED", tradeLegFailed},
		{"failed", tradeLegFailed},
		{"TRADE_STATUS_FAILED", tradeLegFailed},
		{"TRADE_STATUS_FAILED_INSUFFICIENT_BALANCE", tradeLegFailed},
	}
	for _, tc := range cases {
		if got := normalizeTradeStatus(tc.raw); got != tc.want {
			t.Fatalf("normalizeTradeStatus(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestLookupOrderRequiresConfirmedTradesForFinality walks one order through the
// documented lifecycle. MATCHED and RETRYING are non-final; FAILED is a failed
// leg; only CONFIRMED may be booked, and even then the confirmed shares must
// equal size_matched.
func TestLookupOrderRequiresConfirmedTradesForFinality(t *testing.T) {
	status := "TRADE_STATUS_MATCHED"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.98"}`))
		case r.URL.Path == "/data/trades":
			fmt.Fprintf(w, `{"data":[{"id":"trade-1","taker_order_id":"order-1","price":"0.98","size":"100","status":%q}],"next_cursor":""}`, status)
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	if err := executor.ReadyError(); err != "" {
		t.Fatal(err)
	}

	steps := []struct {
		status  string
		wantErr string
	}{
		{"TRADE_STATUS_MATCHED", "未终态 1 笔"},
		{"RETRYING", "未终态 1 笔"},
		{"TRADE_STATUS_FAILED", "失败 1 笔"},
		{"CONFIRMED", ""},
	}
	for _, step := range steps {
		status = step.status
		fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
		if !found {
			t.Fatalf("%s: order must be found", step.status)
		}
		if step.wantErr != "" {
			if err == nil {
				t.Fatalf("%s: a non-final or failed leg must not be booked: %+v", step.status, fill)
			}
			if !strings.Contains(err.Error(), step.wantErr) {
				t.Fatalf("%s: error must surface the leg state %q: %v", step.status, step.wantErr, err)
			}
			if fill.Status != "unknown" || fill.Shares != 0 || fill.Cost != 0 {
				t.Fatalf("%s: incomplete evidence must not carry a fill: %+v", step.status, fill)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: confirmed fill must reconcile: %v", step.status, err)
		}
		if fill.Status != "CONFIRMED" || math.Abs(fill.Shares-100) > 1e-9 || math.Abs(fill.Cost-98) > 1e-9 || math.Abs(fill.Price-0.98) > 1e-9 {
			t.Fatalf("%s: unexpected final fill: %+v", step.status, fill)
		}
	}
}

// TestLookupOrderAllFailedWithSizeMatchedDoesNotFill covers the dangerous
// fallback: a positive size_matched with an all-FAILED fill set is not a fill,
// and the limit-price estimate must not be returned as final.
func TestLookupOrderAllFailedWithSizeMatchedDoesNotFill(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.99"}`))
		case r.URL.Path == "/data/trades":
			_, _ = w.Write([]byte(`{"data":[{"id":"trade-1","taker_order_id":"order-1","price":"0.80","size":"100","status":"TRADE_STATUS_FAILED"}],"next_cursor":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if !found {
		t.Fatal("the order exists even though every match failed")
	}
	if err == nil || !strings.Contains(err.Error(), "失败 1 笔") {
		t.Fatalf("all-failed legs must surface as incomplete: %v", err)
	}
	if fill.Status != "unknown" || fill.Shares != 0 || fill.Cost != 0 || fill.Price != 0 {
		t.Fatalf("a failed leg and size_matched must not produce a fill: %+v", fill)
	}
}

// TestLookupOrderFeesEachConfirmedTrade pins the nonlinear fee contract: two
// fills at 0.20 and 0.80 pay 1.12, not the 1.75 a VWAP schedule would charge.
func TestLookupOrderFeesEachConfirmedTrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			_, _ = w.Write([]byte(`{"id":"order-1","status":"CONFIRMED","size_matched":"100","price":"0.80"}`))
		case r.URL.Path == "/data/trades":
			_, _ = w.Write([]byte(`{"data":[{"id":"trade-1","taker_order_id":"order-1","price":"0.20","size":"50","status":"CONFIRMED"},{"id":"trade-2","taker_order_id":"order-1","price":"0.80","size":"50","status":"CONFIRMED"}],"next_cursor":""}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	if math.Abs(fill.Fee-1.12) > 1e-9 {
		t.Fatalf("fee must be summed per trade: got %v want 1.12 (VWAP would be 1.75)", fill.Fee)
	}
}

// TestLookupOrderFollowsTradesCursorToExhaustion pins the pagination bug: an
// order whose 60-share fill is on page 1 and 40-share fill is on page 2 totals
// 100, not 60, and the second page must be requested.
func TestLookupOrderFollowsTradesCursorToExhaustion(t *testing.T) {
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.99"}`))
		case r.URL.Path == "/data/trades":
			cursor := r.URL.Query().Get("next_cursor")
			cursors = append(cursors, cursor)
			switch cursor {
			case "":
				_, _ = w.Write([]byte(`{"data":[{"id":"trade-1","taker_order_id":"order-1","price":"0.98","size":"60","status":"CONFIRMED"}],"next_cursor":"cursor-2"}`))
			case "cursor-2":
				_, _ = w.Write([]byte(`{"data":[{"id":"trade-2","taker_order_id":"order-1","price":"0.985","size":"40","status":"CONFIRMED"}],"next_cursor":"LTE="}`))
			default:
				t.Errorf("unexpected trades cursor %q", cursor)
				http.Error(w, "bad cursor", http.StatusBadRequest)
			}
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if err != nil || !found {
		t.Fatalf("lookup failed: %v found=%v", err, found)
	}
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "cursor-2" {
		t.Fatalf("both trade pages must be read in order: %v", cursors)
	}
	if math.Abs(fill.Shares-100) > 1e-9 || math.Abs(fill.Cost-98.2) > 1e-9 || math.Abs(fill.Price-0.982) > 1e-9 {
		t.Fatalf("page 2 must contribute to the fill: %+v", fill)
	}
}

// TestLookupOrderTradePageCapIsIncomplete makes the hard page cap observable:
// even after the confirmed quantity happens to equal size_matched, an unread
// cursor means the fill set is not proven and must not be booked.
func TestLookupOrderTradePageCapIsIncomplete(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.98"}`))
		case r.URL.Path == "/data/trades":
			calls++
			fmt.Fprintf(w, `{"data":[{"id":"trade-%d","taker_order_id":"order-1","price":"0.98","size":"50","status":"CONFIRMED"}],"next_cursor":"cursor-%d"}`, calls, calls)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if !found {
		t.Fatal("the order must still be reported as found")
	}
	if err == nil || !strings.Contains(err.Error(), "页上限") {
		t.Fatalf("exceeding the page cap must be incomplete: %v", err)
	}
	if fill.Status != "unknown" || fill.Shares != 0 || fill.Cost != 0 {
		t.Fatalf("a capped trade walk must not produce a fill: %+v", fill)
	}
	if calls != maxTradePages {
		t.Fatalf("trades calls = %d, want the hard cap %d", calls, maxTradePages)
	}
}

// TestLookupOrderDeduplicatesTradeIDs counts a repeated leg once, so a
// duplicated page row cannot double the booked shares.
func TestLookupOrderDeduplicatesTradeIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.98"}`))
		case r.URL.Path == "/data/trades":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"trade-1","taker_order_id":"order-1","price":"0.98","size":"100","status":"CONFIRMED"},
				{"id":"trade-1","taker_order_id":"order-1","price":"0.98","size":"100","status":"CONFIRMED"}
			],"next_cursor":""}`))
		case strings.HasPrefix(r.URL.Path, "/clob-markets/"):
			_, _ = w.Write([]byte(`{"fd":{"r":"0.07","e":"1","to":true}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if err != nil || !found {
		t.Fatalf("lookup failed: %v found=%v", err, found)
	}
	if math.Abs(fill.Shares-100) > 1e-9 || math.Abs(fill.Cost-98) > 1e-9 {
		t.Fatalf("duplicate trade IDs must be counted once: %+v", fill)
	}
}

// TestLookupOrderTrades404DoesNotEstimate makes sure an unavailable trade
// history never silently falls back to size_matched × limit price.
func TestLookupOrderTrades404DoesNotEstimate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.99"}`))
		case r.URL.Path == "/data/trades":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if !found {
		t.Fatal("the order is known; only its fills are missing")
	}
	if err == nil {
		t.Fatal("a 404 from /data/trades must not become a final fill")
	}
	if fill.Status != "unknown" || fill.Shares != 0 || fill.Cost != 0 {
		t.Fatalf("no estimate may be booked without trade evidence: %+v", fill)
	}
}

// TestLookupOrderTradesTransportErrorDoesNotEstimate is the timeout/transport
// variant: the connection is closed before a response, and the order must stay
// unknown without the limit-price estimate.
func TestLookupOrderTradesTransportErrorDoesNotEstimate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.99"}`))
		case r.URL.Path == "/data/trades":
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("test server does not support connection hijacking")
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, _, hijackErr := hijacker.Hijack()
			if hijackErr != nil {
				t.Errorf("hijack: %v", hijackErr)
				return
			}
			_ = conn.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if !found {
		t.Fatal("the order is known; only its fills failed")
	}
	if err == nil {
		t.Fatal("a transport failure must not become a final fill")
	}
	if fill.Status != "unknown" || fill.Shares != 0 || fill.Cost != 0 {
		t.Fatalf("no estimate may be booked without trade evidence: %+v", fill)
	}
}

// TestLookupOrderConfirmedQuantityMustMatchSizeMatched rejects a complete but
// short trade history: 60 confirmed shares against size_matched=100 is not a
// 60-share fill, because 40 shares could still be processing or missing.
func TestLookupOrderConfirmedQuantityMustMatchSizeMatched(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.98"}`))
		case r.URL.Path == "/data/trades":
			_, _ = w.Write([]byte(`{"data":[{"id":"trade-1","taker_order_id":"order-1","price":"0.98","size":"60","status":"CONFIRMED"}],"next_cursor":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	fill, found, err := executor.LookupOrder(context.Background(), "order-1", "condition-1")
	if !found {
		t.Fatal("the order must be reported as found")
	}
	if err == nil || !strings.Contains(err.Error(), "已确认 60/100 份") {
		t.Fatalf("a short confirmed sum must be incomplete: %v", err)
	}
	if fill.Status != "unknown" || fill.Shares != 0 || fill.Cost != 0 {
		t.Fatalf("a mismatched confirmed sum must not be booked: %+v", fill)
	}
}

// TestLookupAmbiguousSubmitDoesNotBookMatchedEstimate covers the timeout path
// in Buy/Sell: a successful order lookup with size_matched but only FAILED
// trade legs must come back unknown, not as a limit-price estimate.
func TestLookupAmbiguousSubmitDoesNotBookMatchedEstimate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.99"}`))
		case r.URL.Path == "/data/trades":
			_, _ = w.Write([]byte(`{"data":[{"id":"trade-1","taker_order_id":"order-1","price":"0.80","size":"100","status":"TRADE_STATUS_FAILED"}],"next_cursor":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := newLookupTestExecutor(server.URL)
	fill, err := executor.lookupAmbiguousSubmit(context.Background(), "order-1", "condition-1", errors.New("submit timeout"))
	if err == nil || !strings.Contains(err.Error(), "submit timeout") {
		t.Fatalf("an ambiguous submit must stay unresolved with its cause: %v", err)
	}
	if fill.Status != "unknown" || fill.Shares != 0 || fill.Cost != 0 {
		t.Fatalf("the submit path must not book size_matched × limit price: %+v", fill)
	}
}

func TestGuardedHTTPClientDoesNotForwardAuthHeaders(t *testing.T) {
	leaked := ""
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("POLY_API_KEY")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	req, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("POLY_API_KEY", "secret-key")
	response, err := guardedHTTPClient(5*time.Second, nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("redirect must be returned, not followed: status %d", response.StatusCode)
	}
	if leaked != "" {
		t.Fatalf("credential leaked to redirect target: %q", leaked)
	}
}

func TestLateCollateralReadFromOldEndpointDoesNotRefillTheCache(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/balance-allowance" {
			http.NotFound(w, r)
			return
		}
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"balance":"100000000","allowances":{"0xe111180000d2663c0091e4f400237545b87b996b":"100000000"}}`))
	}))
	defer oldServer.Close()
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/balance-allowance" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"balance":"50000000","allowances":{"0xe111180000d2663c0091e4f400237545b87b996b":"50000000"}}`))
	}))
	defer newServer.Close()

	executor := New(Config{
		PrivateKey:    "0x" + strings.Repeat("1", 64),
		SignatureType: 0,
		CLOBURL:       oldServer.URL,
		APIKey:        "test-key",
		APISecret:     base64.URLEncoding.EncodeToString([]byte("test-secret")),
		APIPassphrase: "test-passphrase",
	})
	oldRead := make(chan engine.CashSnapshot, 1)
	go func() {
		snapshot, _ := executor.Collateral(context.Background())
		oldRead <- snapshot
	}()
	<-started
	if err := executor.UpdateSettings(engine.LiveSettings{}, newServer.URL); err != nil {
		t.Fatal(err)
	}
	fresh, err := executor.Collateral(context.Background())
	if err != nil || fresh.Amount != 50 {
		t.Fatalf("fresh collateral = %+v, err=%v", fresh, err)
	}
	close(release)
	<-oldRead
	cached, err := executor.Collateral(context.Background())
	if err != nil || cached.Amount != 50 {
		t.Fatalf("old endpoint refilled the cache: %+v, err=%v", cached, err)
	}
}
