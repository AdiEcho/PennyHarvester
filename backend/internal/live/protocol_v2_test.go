package live

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	pmorder "pennyharvester/backend/internal/polymarket/order"
	"pennyharvester/backend/internal/venue"
)

// protocolTestKey is the public Anvil account #0 key; it holds no funds.
const protocolTestKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

func protocolTestClient(t *testing.T, key string) (*clobclient.Client, common.Address) {
	t.Helper()
	parsed, err := crypto.HexToECDSA(strings.TrimPrefix(key, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.PubkeyToAddress(parsed.PublicKey)
	client, err := clobclient.New(clobclient.Config{
		Host:         "https://clob.polymarket.com",
		PrivateKey:   key,
		MakerAddress: address.Hex(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, address
}

// TestSignOrderProtocolV2UsesExchangeV3 checks the whole adapter path: the
// order the CLOB receives carries the ExchangeV3 digest as its id, amounts
// rounded exactly like the SDK, and a signature that recovers to the maker.
func TestSignOrderProtocolV2UsesExchangeV3(t *testing.T) {
	client, address := protocolTestClient(t, protocolTestKey)
	executor := New(Config{PrivateKey: protocolTestKey, ProtocolV2: true})
	if !executor.SupportsProtocol("v2") {
		t.Fatal("ProtocolV2 config must advertise v2 support")
	}
	if executor.SupportsProtocol("v1") || executor.SupportsProtocol("") {
		t.Fatal("legacy versions use the plain path, not the versioned one")
	}

	signed, err := executor.signOrder(context.Background(), client, venue.ProtocolOrder{
		TokenID:     "12345678901234567890",
		ConditionID: "0xcondition",
		Version:     "v2",
	}, clobclient.UserOrder{
		TokenID: "12345678901234567890", Side: clobclient.SideBuy,
		Price: 0.43, Shares: 6.04, OrderType: clobclient.OrderTypeFAK,
	})
	if err != nil {
		t.Fatal(err)
	}
	if signed.Maker != address.Hex() || signed.Signer != address.Hex() {
		t.Fatalf("maker/signer = %s/%s", signed.Maker, signed.Signer)
	}
	if signed.Side != "BUY" || signed.SignatureType != 0 || signed.Expiration != "0" {
		t.Fatalf("wire fields = %+v", signed)
	}
	// FAK buys floor collateral to cents and shares to 0.0001.
	if signed.MakerAmount != "2590000" || signed.TakerAmount != "6040000" {
		t.Fatalf("amounts = %s/%s", signed.MakerAmount, signed.TakerAmount)
	}
	if signed.Timestamp != big.NewInt(signed.Salt).String() {
		t.Fatalf("timestamp %s does not match salt %d", signed.Timestamp, signed.Salt)
	}
	zeroHash := common.Hash{}.Hex()
	if signed.Builder != zeroHash || signed.Metadata != zeroHash {
		t.Fatalf("builder/metadata = %s/%s", signed.Builder, signed.Metadata)
	}

	tokenID, _ := new(big.Int).SetString(signed.TokenID, 10)
	makerAmount, _ := new(big.Int).SetString(signed.MakerAmount, 10)
	takerAmount, _ := new(big.Int).SetString(signed.TakerAmount, 10)
	domain, err := pmorder.DomainFor("v2", false)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := pmorder.Digest(domain, pmorder.Order{
		Salt: signed.Salt, Maker: common.HexToAddress(signed.Maker), Signer: common.HexToAddress(signed.Signer),
		TokenID: tokenID, MakerAmount: makerAmount, TakerAmount: takerAmount,
		Side: 0, SignatureType: 0, Timestamp: signed.Salt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if signed.OrderID != digest.Hex() {
		t.Fatalf("order id %s is not the ExchangeV3 digest %s", signed.OrderID, digest.Hex())
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(signed.Signature, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	raw[64] -= 27
	pub, err := crypto.SigToPub(digest[:], raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := crypto.PubkeyToAddress(*pub); got != address {
		t.Fatalf("signature recovers %s, want %s", got.Hex(), address.Hex())
	}
}

func TestSignOrderProtocolV2RefusedWhenDisabled(t *testing.T) {
	client, _ := protocolTestClient(t, protocolTestKey)
	executor := New(Config{PrivateKey: protocolTestKey, ProtocolV2: false})
	if executor.SupportsProtocol("v2") {
		t.Fatal("a disabled executor must not advertise v2")
	}
	_, err := executor.signOrder(context.Background(), client, venue.ProtocolOrder{
		TokenID: "12345678901234567890", Version: "v2",
	}, clobclient.UserOrder{
		TokenID: "12345678901234567890", Side: clobclient.SideBuy,
		Price: 0.5, Shares: 2, OrderType: clobclient.OrderTypeFAK,
	})
	if err == nil || !strings.Contains(err.Error(), "未启用") {
		t.Fatalf("disabled V2 signing must fail before producing a signature: %v", err)
	}
}

// TestBuyProtocolV2WireBody runs the full buy pipeline against a fake CLOB and
// inspects the exact JSON the exchange would receive. It proves the piece no
// unit test can: the submitted body carries the position id, no protocol
// version field, the FAK order type, and a signature over the ExchangeV3
// domain that recovers to the maker.
func TestBuyProtocolV2WireBody(t *testing.T) {
	const (
		condition = "0x02e25be23d14d9b72d7997ad0bbd45adc1000b00000000000000000000000000"
		position  = "1304567292104579522530582944178631656107034319502628153202976400084872200192"
	)
	var submitted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/clob-markets/"+condition:
			_, _ = w.Write([]byte(`{"c":"` + condition + `","v":"v2","mts":0.001,"mos":5,"fd":{"r":0.01,"e":1,"to":true}}`))
		case r.URL.Path == "/book":
			_, _ = w.Write([]byte(`{"market":"` + condition + `","asset_id":"` + position + `","tick_size":"0.001","bids":[],"asks":[{"price":"0.5","size":"100"}]}`))
		case r.URL.Path == "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.001}`))
		case r.URL.Path == "/order" && r.Method == http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Errorf("decode order body: %v", err)
			}
			_, _ = w.Write([]byte(`{"success":true,"orderID":"clob-v2","status":"matched","makingAmount":"10","takingAmount":"20"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey: protocolTestKey, CLOBURL: server.URL,
		ProtocolV2: true, BuilderCode: "0x" + strings.Repeat("ab", 32),
		APIKey: "api-key", APISecret: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", APIPassphrase: "pass",
	})
	fill, err := executor.BuyProtocol(context.Background(), venue.ProtocolOrder{
		TokenID: position, ConditionID: condition, Version: "v2",
	}, 10, 0.6, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fill.OrderID != "clob-v2" || fill.Shares != 20 || fill.Cost != 10 {
		t.Fatalf("fill = %+v", fill)
	}
	if submitted == nil {
		t.Fatal("no order body was submitted")
	}
	if _, ok := submitted["version"]; ok {
		t.Fatalf("the wire body must not carry a protocol version: %v", submitted)
	}
	if submitted["orderType"] != "FAK" || submitted["deferExec"] != false || submitted["postOnly"] != false {
		t.Fatalf("wire submission options = %v", submitted)
	}
	wire, ok := submitted["order"].(map[string]any)
	if !ok {
		t.Fatalf("wire body has no order object: %v", submitted)
	}
	if wire["tokenId"] != position || wire["side"] != "BUY" {
		t.Fatalf("wire identity = %v", wire)
	}
	if wire["makerAmount"] != "10000000" || wire["takerAmount"] != "20000000" {
		t.Fatalf("wire amounts = %v/%v", wire["makerAmount"], wire["takerAmount"])
	}
	_, address := protocolTestClient(t, protocolTestKey)
	if wire["maker"] != address.Hex() || wire["signer"] != address.Hex() {
		t.Fatalf("wire maker/signer = %v/%v", wire["maker"], wire["signer"])
	}
	if wire["builder"] != "0x"+strings.Repeat("ab", 32) {
		t.Fatalf("builder = %v", wire["builder"])
	}
	if wire["timestamp"] == "" || wire["expiration"] != "0" || wire["signatureType"] != float64(0) {
		t.Fatalf("wire signature metadata = %v", wire)
	}

	salt, _ := wire["salt"].(float64)
	makerAmount, _ := new(big.Int).SetString(wire["makerAmount"].(string), 10)
	takerAmount, _ := new(big.Int).SetString(wire["takerAmount"].(string), 10)
	tokenID, _ := new(big.Int).SetString(position, 10)
	timestamp, _ := strconv.ParseInt(wire["timestamp"].(string), 10, 64)
	domain, err := pmorder.DomainFor("v2", false)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := pmorder.Digest(domain, pmorder.Order{
		Salt: int64(salt), Maker: common.HexToAddress(address.Hex()), Signer: common.HexToAddress(address.Hex()),
		TokenID: tokenID, MakerAmount: makerAmount, TakerAmount: takerAmount,
		Side: 0, SignatureType: 0, Timestamp: timestamp,
		Builder: common.HexToHash("0x" + strings.Repeat("ab", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(wire["signature"].(string), "0x"))
	if err != nil {
		t.Fatal(err)
	}
	raw[64] -= 27
	pub, err := crypto.SigToPub(digest[:], raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := crypto.PubkeyToAddress(*pub); got != address {
		t.Fatalf("wire signature recovers %s, want %s", got.Hex(), address.Hex())
	}
}

// TestSellProtocolV2WireBody mirrors the buy test for exits: the sell must
// sign side 1 over ExchangeV3, report shares as the making amount, and keep
// the position's version all the way to the wire.
func TestSellProtocolV2WireBody(t *testing.T) {
	const (
		condition = "0x02e25be23d14d9b72d7997ad0bbd45adc1000b00000000000000000000000000"
		position  = "1304567292104579522530582944178631656107034319502628153202976400084872200192"
	)
	var submitted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/clob-markets/"+condition:
			_, _ = w.Write([]byte(`{"c":"` + condition + `","v":"v2","mts":0.001,"mos":5,"fd":{"r":0.01,"e":1,"to":true}}`))
		case r.URL.Path == "/book":
			_, _ = w.Write([]byte(`{"market":"` + condition + `","asset_id":"` + position + `","tick_size":"0.001","asks":[],"bids":[{"price":"0.5","size":"100"}]}`))
		case r.URL.Path == "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.001}`))
		case r.URL.Path == "/order" && r.Method == http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Errorf("decode order body: %v", err)
			}
			_, _ = w.Write([]byte(`{"success":true,"orderID":"clob-v2-sell","status":"matched","makingAmount":"20","takingAmount":"10"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := New(Config{
		PrivateKey: protocolTestKey, CLOBURL: server.URL, ProtocolV2: true,
		APIKey: "api-key", APISecret: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", APIPassphrase: "pass",
	})
	fill, err := executor.SellProtocol(context.Background(), venue.ProtocolOrder{
		TokenID: position, ConditionID: condition, Version: "v2",
	}, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fill.OrderID != "clob-v2-sell" || fill.Shares != 20 || fill.Cost != 10 {
		t.Fatalf("fill = %+v", fill)
	}
	if fill.Fee <= 0 || fill.Fee >= fill.Cost {
		t.Fatalf("sell fee = %v", fill.Fee)
	}
	wire, ok := submitted["order"].(map[string]any)
	if !ok {
		t.Fatalf("wire body has no order object: %v", submitted)
	}
	if wire["side"] != "SELL" || wire["tokenId"] != position {
		t.Fatalf("wire identity = %v", wire)
	}
	if wire["makerAmount"] != "20000000" || wire["takerAmount"] != "10000000" {
		t.Fatalf("wire amounts = %v/%v", wire["makerAmount"], wire["takerAmount"])
	}
	makerAmount, _ := new(big.Int).SetString(wire["makerAmount"].(string), 10)
	takerAmount, _ := new(big.Int).SetString(wire["takerAmount"].(string), 10)
	tokenID, _ := new(big.Int).SetString(position, 10)
	timestamp, _ := strconv.ParseInt(wire["timestamp"].(string), 10, 64)
	salt, _ := wire["salt"].(float64)
	domain, err := pmorder.DomainFor("v2", false)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := pmorder.Digest(domain, pmorder.Order{
		Salt: int64(salt), Maker: common.HexToAddress(wire["maker"].(string)), Signer: common.HexToAddress(wire["signer"].(string)),
		TokenID: tokenID, MakerAmount: makerAmount, TakerAmount: takerAmount,
		Side: 1, SignatureType: 0, Timestamp: timestamp,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(wire["signature"].(string), "0x"))
	if err != nil {
		t.Fatal(err)
	}
	raw[64] -= 27
	pub, err := crypto.SigToPub(digest[:], raw)
	if err != nil {
		t.Fatal(err)
	}
	_, address := protocolTestClient(t, protocolTestKey)
	if got := crypto.PubkeyToAddress(*pub); got != address {
		t.Fatalf("sell signature recovers %s, want %s", got.Hex(), address.Hex())
	}
}
