package order

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cyvadra/polymarket-clob-client"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// vectorKey is the well-known Anvil account #0 key. It is public test
// material and must never hold funds.
const vectorKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

// TestVectors were produced independently with eth-account 0.13.4 by ABI
// encoding the published EIP-712 types; they are not derived from this
// package's code.
func tokenID() *big.Int {
	value, _ := new(big.Int).SetString("12345678901234567890", 10)
	return value
}

func vectorOrder(t *testing.T, signatureType uint8) Order {
	t.Helper()
	key, err := crypto.HexToECDSA(strings.TrimPrefix(vectorKey, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.PubkeyToAddress(key.PublicKey)
	return Order{
		Salt:          1700000000000,
		Maker:         address,
		Signer:        address,
		TokenID:       tokenID(),
		MakerAmount:   big.NewInt(2500000),
		TakerAmount:   big.NewInt(5000000),
		Side:          0,
		SignatureType: signatureType,
		Timestamp:     1700000000123,
	}
}

func TestExchangeV3DomainSeparator(t *testing.T) {
	domain, err := DomainFor("v2", false)
	if err != nil {
		t.Fatal(err)
	}
	want := common.HexToHash("0x466c63910185bbd55e8679264200c4e0abdcbb0c6264eb3d41d13326022e095b")
	if got := domain.Separator(); got != want {
		t.Fatalf("ExchangeV3 domain separator = %s, want %s", got.Hex(), want.Hex())
	}
}

func TestSignVectorV3(t *testing.T) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(vectorKey, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	domain, err := DomainFor("v2", false)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign(key, domain, vectorOrder(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := common.HexToHash("0x481286fd4d6433386e6caa2ced2b93120b25d04e32948b60ae6a7812f66a73c4")
	if signed.Digest != wantDigest {
		t.Fatalf("digest = %s, want %s", signed.Digest.Hex(), wantDigest.Hex())
	}
	if signed.OrderID() != wantDigest.Hex() {
		t.Fatalf("order ID = %s, want digest", signed.OrderID())
	}
	wantSignature := "0xe33d43114b2a5475a2455755f3c991acfd18f0eaa5461e80b2f4fc4b606c54c55b296d1828feca8c2e17c19a62697491d5959aed44268ea25694b4b20119c5a51b"
	if signed.Signature != wantSignature {
		t.Fatalf("signature = %s, want %s", signed.Signature, wantSignature)
	}

	raw, err := hex.DecodeString(signed.Signature[2:])
	if err != nil {
		t.Fatal(err)
	}
	raw[64] -= 27
	pub, err := crypto.SigToPub(signed.Digest[:], raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := crypto.PubkeyToAddress(*pub); got != signed.Order.Maker {
		t.Fatalf("recovered %s, want %s", got.Hex(), signed.Order.Maker.Hex())
	}
}

// TestSignMatchesCLOBSDK cross-checks the same order against the SDK's V2
// signer. The SDK cannot build a V3 order, but the struct hash and signature
// algorithm are shared; only the domain values differ, and the V3 domain is
// pinned by TestExchangeV3DomainSeparator.
func TestSignMatchesCLOBSDK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tick-size":
			_, _ = w.Write([]byte(`{"minimum_tick_size":0.01}`))
		case "/neg-risk":
			_, _ = w.Write([]byte(`{"neg_risk":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	key, err := crypto.HexToECDSA(strings.TrimPrefix(vectorKey, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.PubkeyToAddress(key.PublicKey)
	saltTime := time.UnixMilli(1700000000123)
	client, err := clobclient.New(clobclient.Config{
		Host:         server.URL,
		PrivateKey:   vectorKey,
		MakerAddress: address.Hex(),
		Now:          func() time.Time { return saltTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := client.CreateOrder(context.Background(), clobclient.UserOrder{
		TokenID:   "12345678901234567890",
		Side:      clobclient.SideBuy,
		Price:     0.5,
		Shares:    5,
		OrderType: clobclient.OrderTypeFAK,
	})
	if err != nil {
		t.Fatal(err)
	}

	domain, err := DomainFor("v1", false)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := Sign(key, domain, Order{
		Salt:          1700000000123,
		Maker:         address,
		Signer:        address,
		TokenID:       tokenID(),
		MakerAmount:   big.NewInt(2500000),
		TakerAmount:   big.NewInt(5000000),
		Side:          0,
		SignatureType: 0,
		Timestamp:     1700000000123,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reference.OrderID != mine.Digest.Hex() {
		t.Fatalf("SDK digest %s, local digest %s", reference.OrderID, mine.Digest.Hex())
	}
	if reference.Signature != mine.Signature {
		t.Fatalf("SDK signature %s, local signature %s", reference.Signature, mine.Signature)
	}
	if reference.MakerAmount != "2500000" || reference.TakerAmount != "5000000" {
		t.Fatalf("SDK amounts %s/%s", reference.MakerAmount, reference.TakerAmount)
	}
}

func TestAmounts(t *testing.T) {
	cases := []struct {
		name          string
		side          string
		price, shares float64
		orderType     string
		maker, taker  string
	}{
		{"market buy floors collateral to cents", "BUY", .43, 6.04, "FOK", "2590000", "6040000"},
		{"limit buy avoids float artifact", "BUY", .29, 3.45, "GTC", "1000500", "3450000"},
		{"sell rounds shares then collateral", "SELL", .5, 10, "FOK", "10000000", "5000000"},
		{"large sell does not overflow int64", "SELL", .5, 10_000_000_000_000, "GTC", "10000000000000000000", "5000000000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			maker, taker, err := Amounts(tc.side, tc.price, tc.shares, tc.orderType)
			if err != nil {
				t.Fatal(err)
			}
			if maker.String() != tc.maker || taker.String() != tc.taker {
				t.Fatalf("maker/taker = %s/%s, want %s/%s", maker, taker, tc.maker, tc.taker)
			}
		})
	}
	if _, _, err := Amounts("BUY", .004, .1, "FOK"); err == nil {
		t.Fatal("expected zero-rounded amounts to fail")
	}
	if _, _, err := Amounts("HOLD", .5, 1, "FOK"); err == nil {
		t.Fatal("expected invalid side to fail")
	}
}

func TestPoly1271Envelope(t *testing.T) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(vectorKey, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	order := vectorOrder(t, 3)
	domain, err := DomainFor("v2", false)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign(key, domain, order)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(signed.Signature[2:])
	if err != nil {
		t.Fatal(err)
	}
	// 65-byte inner signature, 32-byte domain separator, 32-byte contents
	// hash, the contents type string and its two-byte length.
	if want := 65 + 32 + 32 + len(OrderTypeString) + 2; len(raw) != want {
		t.Fatalf("envelope length = %d, want %d", len(raw), want)
	}
	if got := hex.EncodeToString(raw[65:97]); got != hex.EncodeToString(domain.Separator().Bytes()) {
		t.Fatalf("envelope domain = %s, want %s", got, domain.Separator().Hex())
	}
	contentsHash, err := order.StructHash()
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(raw[97:129]); got != hex.EncodeToString(contentsHash.Bytes()) {
		t.Fatalf("envelope contents = %s, want %s", got, contentsHash.Hex())
	}
	if got := string(raw[129 : 129+len(OrderTypeString)]); got != OrderTypeString {
		t.Fatalf("envelope type = %q", got)
	}
	if got, want := hex.EncodeToString(raw[129+len(OrderTypeString):]), fmt.Sprintf("%04x", len(OrderTypeString)); got != want {
		t.Fatalf("envelope length suffix = %s, want %s", got, want)
	}
	inner := append([]byte(nil), raw[:65]...)
	inner[64] -= 27
	outerHash, err := poly1271StructHash(domain.Separator(), order, contentsHash)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := crypto.SigToPub(outerHash[:], inner)
	if err != nil {
		t.Fatal(err)
	}
	if got := crypto.PubkeyToAddress(*pub); got != order.Maker {
		t.Fatalf("inner signature recovered %s, want %s", got.Hex(), order.Maker.Hex())
	}
	if _, err := Sign(key, domain, Order{
		Maker: order.Maker, Signer: common.HexToAddress("0x0000000000000000000000000000000000000001"),
		TokenID: order.TokenID, MakerAmount: order.MakerAmount, TakerAmount: order.TakerAmount,
		SignatureType: 3,
	}); err == nil {
		t.Fatal("expected POLY_1271 with a different signer to fail")
	}
}

func TestDomainFor(t *testing.T) {
	v3, err := DomainFor("v2", true)
	if err != nil || v3.VerifyingContract != ExchangeV3 || v3.Version != "3" {
		t.Fatalf("v2 domain = %+v, %v", v3, err)
	}
	plain, err := DomainFor("", false)
	if err != nil || plain.VerifyingContract != CTFExchangeV2 || plain.Version != "2" {
		t.Fatalf("legacy domain = %+v, %v", plain, err)
	}
	negRisk, err := DomainFor("v1", true)
	if err != nil || negRisk.VerifyingContract != NegRiskExchangeV2 {
		t.Fatalf("neg-risk domain = %+v, %v", negRisk, err)
	}
	if _, err := DomainFor("v9", false); err == nil {
		t.Fatal("expected unknown version to fail")
	}
}
