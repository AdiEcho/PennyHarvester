package live

import (
	"context"
	"encoding/base64"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clobclient "github.com/Cyvadra/polymarket-clob-client"

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

func TestApplyFeeAddsTakerFeeToCost(t *testing.T) {
	fill := engine.LiveFill{Shares: 100, Cost: 98, Price: 0.98, Status: "matched"}
	applyFee(clobclient.FeeSchedule{Rate: "0.07", Exponent: "1", TakerOnly: true}, &fill)
	wantFee := 0.07 * 100 * 0.98 * 0.02
	if math.Abs(fill.Fee-wantFee) > 1e-6 {
		t.Fatalf("unexpected fee: got %f want %f", fill.Fee, wantFee)
	}
	if math.Abs(fill.Cost-(98+wantFee)) > 1e-6 {
		t.Fatalf("fee was not added to cost: %+v", fill)
	}
	if math.Abs(fill.Price-(fill.Cost/fill.Shares)) > 1e-12 {
		t.Fatalf("all-in price not recomputed: %+v", fill)
	}
}

func TestLookupOrderAddsFee(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/data/order/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"order-1","status":"MATCHED","size_matched":"100","price":"0.98","avg_price":"0.98"}`))
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
	if math.Abs(fill.Cost-(98+fill.Fee)) > 1e-9 {
		t.Fatalf("reconciled cost must include fee: %+v", fill)
	}
	fee, err := executor.EstimateBuyFee(context.Background(), "condition-1", 98, 0.98)
	if err != nil || fee <= 0 {
		t.Fatalf("unexpected estimated fee: %v %v", fee, err)
	}
}

func TestBuyPersistsSignedOrderAndAddsFee(t *testing.T) {
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
	if math.Abs(fill.Cost-(98+fill.Fee)) > 1e-9 {
		t.Fatalf("fee was not included in cost: %+v", fill)
	}
}
