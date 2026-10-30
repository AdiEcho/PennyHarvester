package live

import (
	"context"
	"encoding/base64"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clobclient "github.com/Cyvadra/polymarket-clob-client"

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

func TestApplyFeeAddsTakerFeeToCost(t *testing.T) {
	fill := engine.LiveFill{Shares: 100, Cost: 98, Price: 0.98, Status: "matched"}
	if err := applyFee(clobclient.FeeSchedule{Rate: "0.07", Exponent: "1", TakerOnly: true}, &fill); err != nil {
		t.Fatal(err)
	}
	wantFee := 0.07 * 100 * 0.98 * 0.02
	if math.Abs(fill.Fee-wantFee) > 1e-6 {
		t.Fatalf("unexpected fee: got %f want %f", fill.Fee, wantFee)
	}
	if math.Abs(fill.Cost-(98+wantFee)) > 1e-6 {
		t.Fatalf("fee was not added to cost: %+v", fill)
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
	if math.Abs(fill.Cost-(98+fill.Fee)) > 1e-9 {
		t.Fatalf("fee was not included in cost: %+v", fill)
	}
	// The builder code must be inside the signed payload, otherwise the matched
	// trade is not attributed to the builder profile.
	if !strings.Contains(submittedOrder, `"builder":"`+builderCode+`"`) {
		t.Fatalf("signed order does not carry the builder code: %s", submittedOrder)
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
