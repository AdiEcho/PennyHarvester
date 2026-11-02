package predictfun

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"pennyharvester/backend/internal/venue"
)

// recoverWireSigner rebuilds the EIP-712 struct from what was posted and
// recovers the address that signed it. It proves the executor signed the same
// order it sent, including the neg-risk domain choice.
func recoverWireSigner(t *testing.T, signed SignedOrder, negRisk bool) common.Address {
	t.Helper()
	addresses, ok := AddressesFor(ChainIDBnbTestnet)
	if !ok {
		t.Fatal("testnet addresses missing")
	}
	order := Order{
		Salt:          mustBig(t, signed.Salt),
		Maker:         common.HexToAddress(signed.Maker),
		Signer:        common.HexToAddress(signed.Signer),
		Taker:         common.HexToAddress(signed.Taker),
		TokenID:       mustBig(t, signed.TokenID),
		MakerAmount:   mustBig(t, signed.MakerAmount),
		TakerAmount:   mustBig(t, signed.TakerAmount),
		Expiration:    mustBig(t, signed.Expiration),
		Nonce:         mustBig(t, signed.Nonce),
		FeeRateBps:    mustBig(t, signed.FeeRateBps),
		Side:          Side(signed.Side),
		SignatureType: SignatureType(signed.SignatureType),
	}
	hash, err := order.Hash(ExchangeDomain(ChainIDBnbTestnet, addresses, negRisk))
	if err != nil {
		t.Fatalf("hash posted order: %v", err)
	}
	raw, err := hexutil.Decode(signed.Signature)
	if err != nil || len(raw) != 65 {
		t.Fatalf("posted signature is not 65 bytes: %v", err)
	}
	if raw[64] != 27 && raw[64] != 28 {
		t.Fatalf("posted signature v = %d", raw[64])
	}
	raw[64] -= 27
	publicKey, err := crypto.Ecrecover(hash.Bytes(), raw)
	if err != nil {
		t.Fatalf("Ecrecover: %v", err)
	}
	recovered, err := crypto.UnmarshalPubkey(publicKey)
	if err != nil {
		t.Fatalf("UnmarshalPubkey: %v", err)
	}
	return crypto.PubkeyToAddress(*recovered)
}

func dp3MarketJSON() string {
	market := strings.ReplaceAll(testMarketJSON, `"id": 42`, `"id": 43`)
	market = strings.ReplaceAll(market, `"0xcond42"`, `"0xcond43"`)
	market = strings.ReplaceAll(market, `"onChainId": "111"`, `"onChainId": "333"`)
	market = strings.ReplaceAll(market, `"onChainId": "222"`, `"onChainId": "444"`)
	market = strings.ReplaceAll(market, `"decimalPrecision": 2`, `"decimalPrecision": 3`)
	return market
}

func TestBuyScalesAmountsAndSignsOnce(t *testing.T) {
	exchange := newFakeExchange()
	executor, signatures := newTestExecutor(t, exchange, "42")
	key := testKey(t)
	wantSigner := crypto.PubkeyToAddress(key.PublicKey)

	signedIDs := make([]string, 0, 1)
	fill, err := executor.Buy(context.Background(), "111", "0xcond42", 5, 0.55, func(orderID string) error {
		signedIDs = append(signedIDs, orderID)
		return nil
	})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if *signatures != 1 {
		t.Fatalf("signatures = %d, want 1", *signatures)
	}
	bodies := exchange.bodies()
	if len(bodies) != 1 {
		t.Fatalf("order posts = %d, want 1", len(bodies))
	}
	posted := bodies[0]
	if posted.Order.MakerAmount != "5000000000000000000" {
		t.Fatalf("makerAmount = %s, want 5e18", posted.Order.MakerAmount)
	}
	limit := 0.51
	shares := math.Floor(5/limit/shareStep) * shareStep
	if posted.Order.TakerAmount != venue.Scale(shares, venue.USDTDecimals).String() {
		t.Fatalf("takerAmount = %s, want %s", posted.Order.TakerAmount, venue.Scale(shares, venue.USDTDecimals))
	}
	if posted.PricePerShare != "0.51" {
		t.Fatalf("pricePerShare = %s, want 0.51", posted.PricePerShare)
	}
	if posted.Order.Side != uint8(SideBuy) || posted.Order.TokenID != "111" || posted.Order.FeeRateBps != "200" {
		t.Fatalf("posted order = %+v", posted.Order)
	}
	if posted.Order.Taker != (common.Address{}).Hex() {
		t.Fatalf("taker = %s, want the zero address for a public order", posted.Order.Taker)
	}
	if got := recoverWireSigner(t, posted.Order, false); got != wantSigner {
		t.Fatalf("posted order signed by %s, want %s", got, wantSigner)
	}

	if fill.OrderID != "ord-1" || fill.Status != "filled" {
		t.Fatalf("fill = %+v", fill)
	}
	if math.Abs(fill.Shares-shares) > 1e-9 || fill.Price != 0.51 {
		t.Fatalf("fill shares/price = %v/%v", fill.Shares, fill.Price)
	}
	wantFee := 0.02 * math.Min(0.51, 0.49) * shares
	if math.Abs(fill.Fee-wantFee) > 1e-9 {
		t.Fatalf("fee = %v, want %v", fill.Fee, wantFee)
	}
	// Cost is the gross notional; the fee travels separately so the engine can
	// add it exactly once instead of charging it twice.
	if math.Abs(fill.Cost-shares*0.51) > 1e-9 {
		t.Fatalf("cost = %v, want gross notional %v", fill.Cost, shares*0.51)
	}
	// The ledger row is created before the POST, so onSigned receives the local
	// order hash rather than the exchange id that arrives with the response.
	if len(signedIDs) != 1 || !strings.HasPrefix(signedIDs[0], "0x") || len(signedIDs[0]) != 66 {
		t.Fatalf("onSigned calls = %v, want one local order hash", signedIDs)
	}
	if signedIDs[0] == fill.OrderID {
		t.Fatalf("onSigned must run before the exchange id is known: %v", signedIDs)
	}
}

func TestBuyReusesSignatureAcrossAuthRetry(t *testing.T) {
	exchange := newFakeExchange()
	exchange.statusHooks = []int{401}
	exchange.orderStatus = "OPEN"
	executor, signatures := newTestExecutor(t, exchange, "42")

	fill, err := executor.Buy(context.Background(), "111", "0xcond42", 5, 0.55, nil)
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if *signatures != 1 {
		t.Fatalf("signatures = %d, want exactly 1 across a retry", *signatures)
	}
	if attempts := exchange.attempts(); attempts != 2 {
		t.Fatalf("order posts = %d, want 2 (401 then retry)", attempts)
	}
	if auth := exchange.authCount(); auth != 2 {
		t.Fatalf("auth posts = %d, want 2 (initial token + refresh)", auth)
	}
	bodies := exchange.bodies()
	if len(bodies) != 2 {
		t.Fatalf("captured bodies = %d", len(bodies))
	}
	if bodies[0].Order != bodies[1].Order || bodies[0].PricePerShare != bodies[1].PricePerShare {
		t.Fatalf("retry re-signed the order:\n%+v\n%+v", bodies[0], bodies[1])
	}
	// A resting order is not a rejection: the engine would otherwise write
	// "rejected" while the order stays live at the exchange. "unknown" keeps it
	// reconcilable.
	if fill.Status != "unknown" || fill.Shares != 0 {
		t.Fatalf("resting order fill = %+v, want status unknown with no booked shares", fill)
	}
	if fill.OrderID != "ord-1" {
		t.Fatalf("fill order id = %s", fill.OrderID)
	}
}

func TestBuyRejectsUnknownTickWithoutSigning(t *testing.T) {
	exchange := newFakeExchange()
	exchange.marketJSON = strings.ReplaceAll(testMarketJSON, `"decimalPrecision": 2,`, "")
	executor, signatures := newTestExecutor(t, exchange, "42")

	_, err := executor.Buy(context.Background(), "111", "0xcond42", 5, 0.55, nil)
	if err == nil || !strings.Contains(err.Error(), "decimalPrecision") {
		t.Fatalf("error = %v", err)
	}
	if *signatures != 0 || exchange.attempts() != 0 {
		t.Fatalf("an order without a tick was signed or sent")
	}
}

func TestBuySubmitFailureReturnsReconcilableUnknown(t *testing.T) {
	exchange := newFakeExchange()
	exchange.statusHooks = []int{500}
	executor, signatures := newTestExecutor(t, exchange, "42")

	var signedIDs []string
	fill, err := executor.Buy(context.Background(), "111", "0xcond42", 5, 0.55, func(id string) error {
		signedIDs = append(signedIDs, id)
		return nil
	})
	if err == nil {
		t.Fatal("expected the submit failure to be reported")
	}
	// The order must already be recorded when the POST fails: otherwise an
	// accepted-but-unknown order can never be reconciled and the market gets
	// bought twice on the next scan.
	if len(signedIDs) != 1 || signedIDs[0] != fill.OrderID {
		t.Fatalf("onSigned = %v, want the reconciliable id %q", signedIDs, fill.OrderID)
	}
	if *signatures != 1 {
		t.Fatalf("signatures = %d, want 1", *signatures)
	}
	if fill.Status != "unknown" {
		t.Fatalf("fill status = %q, want unknown", fill.Status)
	}
	if !strings.HasPrefix(fill.OrderID, "0x") || len(fill.OrderID) != 66 {
		t.Fatalf("fill order id = %q, want the 32-byte order hash", fill.OrderID)
	}
}

// A definitive 4xx refusal is not "unknown": nothing can fill later, and the
// engine books a rejected order so the account's stop-loss is not frozen for the
// reconciliation window.
func TestBuyTreatsExchangeRejectionAsRejected(t *testing.T) {
	exchange := newFakeExchange()
	exchange.statusHooks = []int{400}
	executor, _ := newTestExecutor(t, exchange, "42")

	var signedIDs []string
	fill, err := executor.Buy(context.Background(), "111", "0xcond42", 5, 0.55, func(id string) error {
		signedIDs = append(signedIDs, id)
		return nil
	})
	if err == nil {
		t.Fatal("expected the rejection to be reported")
	}
	if fill.Status != "rejected" {
		t.Fatalf("fill status = %q, want rejected", fill.Status)
	}
	if len(signedIDs) != 1 || signedIDs[0] != fill.OrderID {
		t.Fatalf("onSigned = %v, want %q", signedIDs, fill.OrderID)
	}
}

// Rate limiting is transient: the order may still be accepted, so it has to stay
// reconcilable rather than being declared rejected.
func TestBuyKeepsRateLimitReconcilable(t *testing.T) {
	exchange := newFakeExchange()
	// One retry is attempted for a rate limit, so the second attempt is limited
	// too: the point is the final classification, not the retry itself.
	exchange.statusHooks = []int{429, 429}
	executor, _ := newTestExecutor(t, exchange, "42")

	fill, err := executor.Buy(context.Background(), "111", "0xcond42", 5, 0.55, nil)
	if err == nil {
		t.Fatal("expected the failure to be reported")
	}
	if fill.Status != "unknown" {
		t.Fatalf("fill status = %q, want unknown", fill.Status)
	}
}

func TestBuyRejectsBelowMinNotional(t *testing.T) {
	exchange := newFakeExchange()
	executor, signatures := newTestExecutor(t, exchange, "42")

	_, err := executor.Buy(context.Background(), "111", "0xcond42", 0.5, 0.55, nil)
	if err == nil || !strings.Contains(err.Error(), "最小金额") {
		t.Fatalf("error = %v", err)
	}
	if exchange.attempts() != 0 {
		t.Fatalf("an order below the minimum reached the API")
	}
	if *signatures != 0 {
		t.Fatalf("signatures = %d, want 0", *signatures)
	}
}

func TestBuyRejectsBookAboveMaxPrice(t *testing.T) {
	exchange := newFakeExchange()
	exchange.bookJSON = `{"marketId": 42, "asks": [[0.6, 100000000000000000000]], "bids": [[0.55, 100000000000000000000]]}`
	executor, signatures := newTestExecutor(t, exchange, "42")

	_, err := executor.Buy(context.Background(), "111", "0xcond42", 5, 0.5, nil)
	if err == nil || !strings.Contains(err.Error(), "盘口价格已超过限价") {
		t.Fatalf("error = %v", err)
	}
	if exchange.attempts() != 0 || *signatures != 0 {
		t.Fatalf("an order above the limit price was signed or sent")
	}
}

func TestBuyClampsLimitToMaxPrice(t *testing.T) {
	exchange := newFakeExchange()
	exchange.marketJSON = dp3MarketJSON()
	executor, signatures := newTestExecutor(t, exchange, "43")

	// ask 0.5 + 200 bps = 0.51, but maxPrice is 0.509 with a 0.001 tick.
	if _, err := executor.Buy(context.Background(), "333", "0xcond43", 5, 0.509, nil); err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if *signatures != 1 {
		t.Fatalf("signatures = %d", *signatures)
	}
	bodies := exchange.bodies()
	if len(bodies) != 1 || bodies[0].PricePerShare != "0.509" {
		t.Fatalf("pricePerShare = %+v, want the clamped 0.509", bodies)
	}
	shares := math.Floor(5/0.509/shareStep) * shareStep
	if bodies[0].Order.TakerAmount != venue.Scale(shares, venue.USDTDecimals).String() {
		t.Fatalf("takerAmount = %s, want %s", bodies[0].Order.TakerAmount, venue.Scale(shares, venue.USDTDecimals))
	}
}

func TestBuyNoOutcomeUsesInvertedYesBid(t *testing.T) {
	exchange := newFakeExchange()
	executor, _ := newTestExecutor(t, exchange, "42")

	if _, err := executor.Buy(context.Background(), "222", "0xcond42", 5, 0.99, nil); err != nil {
		t.Fatalf("Buy: %v", err)
	}
	bodies := exchange.bodies()
	if len(bodies) != 1 {
		t.Fatalf("order posts = %d", len(bodies))
	}
	// The only published side is Yes; the No ask is 1-(best Yes bid 0.48)=0.52,
	// and the 200 bps cushion lands on 0.53.
	if bodies[0].PricePerShare != "0.53" {
		t.Fatalf("No pricePerShare = %s, want 0.53", bodies[0].PricePerShare)
	}
	shares := math.Floor(5/0.53/shareStep) * shareStep
	if bodies[0].Order.TakerAmount != venue.Scale(shares, venue.USDTDecimals).String() {
		t.Fatalf("No takerAmount = %s, want %s", bodies[0].Order.TakerAmount, venue.Scale(shares, venue.USDTDecimals))
	}
	if bodies[0].Order.TokenID != "222" {
		t.Fatalf("token = %s, want the No onChainId", bodies[0].Order.TokenID)
	}
}

func TestSellAmountsAndSignature(t *testing.T) {
	exchange := newFakeExchange()
	executor, signatures := newTestExecutor(t, exchange, "42")
	key := testKey(t)
	wantSigner := crypto.PubkeyToAddress(key.PublicKey)

	fill, err := executor.Sell(context.Background(), "111", "0xcond42", 10, nil)
	if err != nil {
		t.Fatalf("Sell: %v", err)
	}
	if *signatures != 1 {
		t.Fatalf("signatures = %d", *signatures)
	}
	bodies := exchange.bodies()
	if len(bodies) != 1 {
		t.Fatalf("order posts = %d", len(bodies))
	}
	posted := bodies[0]
	// best bid 0.48 minus 200 bps = 0.4704, quantized down to 0.47.
	if posted.PricePerShare != "0.47" {
		t.Fatalf("pricePerShare = %s", posted.PricePerShare)
	}
	if posted.Order.Side != uint8(SideSell) {
		t.Fatalf("side = %d, want SELL", posted.Order.Side)
	}
	if posted.Order.MakerAmount != "10000000000000000000" {
		t.Fatalf("makerAmount = %s, want 10 shares", posted.Order.MakerAmount)
	}
	if posted.Order.TakerAmount != "4700000000000000000" {
		t.Fatalf("takerAmount = %s, want 4.7 USDT", posted.Order.TakerAmount)
	}
	if got := recoverWireSigner(t, posted.Order, false); got != wantSigner {
		t.Fatalf("sell signed by %s, want %s", got, wantSigner)
	}
	wantFee := 0.02 * math.Min(0.47, 0.53) * 10
	if math.Abs(fill.Fee-wantFee) > 1e-9 {
		t.Fatalf("fee = %v, want %v", fill.Fee, wantFee)
	}
	if math.Abs(fill.Cost-4.7) > 1e-9 {
		t.Fatalf("cost = %v, want gross proceeds 4.7", fill.Cost)
	}
	if fill.Status != "filled" || math.Abs(fill.Shares-10) > 1e-9 {
		t.Fatalf("fill = %+v", fill)
	}
}

func TestEstimateBuyFeeUsesMinPriceTerm(t *testing.T) {
	exchange := newFakeExchange()
	executor, _ := newTestExecutor(t, exchange, "42")

	fee, err := executor.EstimateBuyFee(context.Background(), "0xcond42", 5, 0.5)
	if err != nil {
		t.Fatalf("EstimateBuyFee: %v", err)
	}
	// 200 bps × min(0.5,0.5) × 10 shares.
	if math.Abs(fee-0.1) > 1e-9 {
		t.Fatalf("fee = %v, want 0.1", fee)
	}
	if _, err := executor.EstimateBuyFee(context.Background(), "", 5, 0.5); err == nil {
		t.Fatal("a missing condition id must fail")
	}
}

func TestQuoteWalksBookDepth(t *testing.T) {
	exchange := newFakeExchange()
	executor, _ := newTestExecutor(t, exchange, "42")

	quote, err := executor.Quote(context.Background(), "111", "0xcond42", 5)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	// Quote mirrors the FAK quote semantics: the limit is the worst touched
	// level tick-ceiled (0.50), not the slippage-cushioned price the order will
	// eventually sign.
	limit := 0.50
	shares := 10.0
	if quote.LimitPrice != limit || math.Abs(quote.Shares-shares) > 1e-9 {
		t.Fatalf("quote limit/shares = %v/%v", quote.LimitPrice, quote.Shares)
	}
	if math.Abs(quote.Notional-shares*0.5) > 1e-9 || quote.FillPrice != 0.5 {
		t.Fatalf("quote notional/fill price = %v/%v", quote.Notional, quote.FillPrice)
	}
	if quote.AvailableNotional != 50 || !quote.FullyFilled {
		t.Fatalf("quote depth = %v fully=%v", quote.AvailableNotional, quote.FullyFilled)
	}
	if math.Abs(quote.Fee-0.01*shares) > 1e-9 {
		t.Fatalf("quote fee = %v", quote.Fee)
	}

	// A budget larger than the visible depth quotes what exists instead of
	// pretending the rest fills.
	large, err := executor.Quote(context.Background(), "111", "0xcond42", 500)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if large.FullyFilled {
		t.Fatal("a quote beyond the book depth reported fully filled")
	}
	if large.AvailableNotional != 50 {
		t.Fatalf("available = %v", large.AvailableNotional)
	}
}

func TestExecutorRejectsSmartWalletMaker(t *testing.T) {
	client, err := NewClient(Config{
		BaseURL:    "http://127.0.0.1:1",
		APIKey:     "key",
		PrivateKey: testKeyHex,
		Maker:      "0x1111111111111111111111111111111111111111",
		Testnet:    true,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	executor := NewExecutor(client, NewProvider(client))
	if message := executor.ReadyError(); !strings.Contains(message, "EOA") {
		t.Fatalf("ReadyError = %q", message)
	}
	if _, err := executor.Buy(context.Background(), "111", "0xcond42", 5, 0.5, nil); err == nil {
		t.Fatal("expected the EOA-only guard to reject the order")
	}
}
