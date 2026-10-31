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
