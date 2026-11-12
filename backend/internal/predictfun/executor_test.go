package predictfun

import (
	"context"
	"fmt"
	"math"
	"math/big"
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
		Expiration:    big.NewInt(signed.Expiration),
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
	if posted.Data.Order.MakerAmount != "5000000000000000000" {
		t.Fatalf("makerAmount = %s, want 5e18", posted.Data.Order.MakerAmount)
	}
	limit := 0.51
	shares := math.Floor(5/limit/shareStep) * shareStep
	if posted.Data.Order.TakerAmount != venue.Scale(shares, venue.USDTDecimals).String() {
		t.Fatalf("takerAmount = %s, want %s", posted.Data.Order.TakerAmount, venue.Scale(shares, venue.USDTDecimals))
	}
	if posted.Data.PricePerShare != "510000000000000000" {
		t.Fatalf("pricePerShare = %s, want 510000000000000000", posted.Data.PricePerShare)
	}
	if posted.Data.Strategy != StrategyMarket || posted.Data.SlippageBps != "200" {
		t.Fatalf("strategy/slippage = %q/%q, want MARKET/200", posted.Data.Strategy, posted.Data.SlippageBps)
	}
	if posted.Data.Order.Side != uint8(SideBuy) || posted.Data.Order.TokenID != "111" || posted.Data.Order.FeeRateBps != "200" {
		t.Fatalf("posted order = %+v", posted.Data.Order)
	}
	if posted.Data.Order.Taker != (common.Address{}).Hex() {
		t.Fatalf("taker = %s, want the zero address for a public order", posted.Data.Order.Taker)
	}
	if got := recoverWireSigner(t, *posted.Data.Order, false); got != wantSigner {
		t.Fatalf("posted order signed by %s, want %s", got, wantSigner)
	}

	// A successful create is only an acceptance. This fixture does not expose a
	// terminal order or settled matches, so the executor must not invent a fill
	// from the signed request size or limit price.
	if fill.Status != "unknown" || fill.Shares != 0 || fill.Cost != 0 {
		t.Fatalf("fill = %+v, want an unknown zero-share result", fill)
	}
	// The ledger row is created before the POST, so onSigned receives the local
	// order hash, which is also the reconciliation identity returned here.
	if len(signedIDs) != 1 || !strings.HasPrefix(signedIDs[0], "0x") || len(signedIDs[0]) != 66 {
		t.Fatalf("onSigned calls = %v, want one local order hash", signedIDs)
	}
	if fill.OrderID != signedIDs[0] {
		t.Fatalf("fill order id = %q, want local hash %q", fill.OrderID, signedIDs[0])
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
	if *bodies[0].Data.Order != *bodies[1].Data.Order || bodies[0].Data.PricePerShare != bodies[1].Data.PricePerShare {
		t.Fatalf("retry re-signed the order:\n%+v\n%+v", bodies[0], bodies[1])
	}
	// A resting order is not a rejection: the engine would otherwise write
	// "rejected" while the order stays live at the exchange. "unknown" keeps it
	// reconcilable.
	if fill.Status != "unknown" || fill.Shares != 0 {
		t.Fatalf("resting order fill = %+v, want status unknown with no booked shares", fill)
	}
	if !strings.HasPrefix(fill.OrderID, "0x") || len(fill.OrderID) != 66 {
		t.Fatalf("fill order id = %s, want the local order hash", fill.OrderID)
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
	if len(bodies) != 1 || bodies[0].Data.PricePerShare != "509000000000000000" {
		t.Fatalf("pricePerShare = %+v, want the clamped 509000000000000000", bodies)
	}
	shares := math.Floor(5/0.509/shareStep) * shareStep
	if bodies[0].Data.Order.TakerAmount != venue.Scale(shares, venue.USDTDecimals).String() {
		t.Fatalf("takerAmount = %s, want %s", bodies[0].Data.Order.TakerAmount, venue.Scale(shares, venue.USDTDecimals))
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
	if bodies[0].Data.PricePerShare != "530000000000000000" {
		t.Fatalf("No pricePerShare = %s, want 530000000000000000", bodies[0].Data.PricePerShare)
	}
	shares := math.Floor(5/0.53/shareStep) * shareStep
	if bodies[0].Data.Order.TakerAmount != venue.Scale(shares, venue.USDTDecimals).String() {
		t.Fatalf("No takerAmount = %s, want %s", bodies[0].Data.Order.TakerAmount, venue.Scale(shares, venue.USDTDecimals))
	}
	if bodies[0].Data.Order.TokenID != "222" {
		t.Fatalf("token = %s, want the No onChainId", bodies[0].Data.Order.TokenID)
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
	if posted.Data.PricePerShare != "470000000000000000" {
		t.Fatalf("pricePerShare = %s", posted.Data.PricePerShare)
	}
	if posted.Data.Order.Side != uint8(SideSell) {
		t.Fatalf("side = %d, want SELL", posted.Data.Order.Side)
	}
	if posted.Data.Order.MakerAmount != "10000000000000000000" {
		t.Fatalf("makerAmount = %s, want 10 shares", posted.Data.Order.MakerAmount)
	}
	if posted.Data.Order.TakerAmount != "4700000000000000000" {
		t.Fatalf("takerAmount = %s, want 4.7 USDT", posted.Data.Order.TakerAmount)
	}
	if got := recoverWireSigner(t, *posted.Data.Order, false); got != wantSigner {
		t.Fatalf("sell signed by %s, want %s", got, wantSigner)
	}
	// As with a buy, acceptance carries no execution evidence. The sell only
	// becomes bookable after a terminal order and attributable matches exist.
	if fill.Status != "unknown" || fill.Shares != 0 {
		t.Fatalf("fill = %+v, want unknown with no booked shares", fill)
	}
}

func testFillOrder(hash string, side Side, status, filled string) OrderData {
	return OrderData{
		Order:        &ContractOrder{Hash: hash, Side: uint8(side)},
		ID:           "exchange-id",
		Status:       status,
		Amount:       Number(venue.Scale(100, venue.USDTDecimals).String()),
		AmountFilled: Number(filled),
	}
}

func testMatch(hash string, taker bool, amount, price float64, feeType string, feeAmount float64) MatchData {
	leg := OrderFillData{
		QuoteType: "Ask",
		Amount:    Number(venue.Scale(amount, venue.USDTDecimals).String()),
		Price:     Number(fmt.Sprintf("%.6f", price)),
		Hash:      hash,
		Fee:       &Fee{Amount: Number(venue.Scale(feeAmount, venue.USDTDecimals).String()), Type: feeType},
	}
	match := MatchData{TransactionHash: "0xtx", Taker: OrderFillData{Hash: "0xother", Fee: &Fee{Amount: Number("0"), Type: feeType}}}
	if taker {
		match.Taker = leg
	} else {
		match.Makers = []OrderFillData{leg, {Hash: "0xother", Fee: &Fee{Amount: Number("0"), Type: feeType}}}
	}
	return match
}

func TestFillFromOrderUsesAttributableMatchesForVWAP(t *testing.T) {
	executor := &Executor{}
	hash := "0xabc"
	order := testFillOrder(hash, SideBuy, "FILLED", venue.Scale(100, venue.USDTDecimals).String())
	matches := []MatchData{
		testMatch(hash, false, 40, 0.40, "SHARES", 0),
		testMatch(hash, true, 60, 0.60, "SHARES", 0),
	}
	fill := executor.fillFromOrder(order, matches, hash)
	if fill.Status != "filled" {
		t.Fatalf("status = %q, want filled", fill.Status)
	}
	if math.Abs(fill.Shares-100) > 1e-9 {
		t.Fatalf("shares = %v, want 100 (maker leg must not be double counted)", fill.Shares)
	}
	if math.Abs(fill.Cost-52) > 1e-9 {
		t.Fatalf("cost = %v, want 40*0.4 + 60*0.6 = 52", fill.Cost)
	}
	if math.Abs(fill.Price-0.52) > 1e-9 {
		t.Fatalf("price = %v, want two-leg VWAP 0.52", fill.Price)
	}
}

func TestFillFromOrderConvertsBuyFeeAssets(t *testing.T) {
	executor := &Executor{}
	hash := "0xabc"
	tests := []struct {
		name      string
		feeType   string
		feeAmount float64
	}{
		{name: "shares", feeType: "SHARES", feeAmount: 2},
		{name: "collateral", feeType: "COLLATERAL", feeAmount: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			order := testFillOrder(hash, SideBuy, "FILLED", venue.Scale(100, venue.USDTDecimals).String())
			matches := []MatchData{testMatch(hash, true, 100, 0.5, tc.feeType, tc.feeAmount)}
			fill := executor.fillFromOrder(order, matches, hash)
			if fill.Status != "filled" {
				t.Fatalf("status = %q", fill.Status)
			}
			if math.Abs(fill.Shares-98) > 1e-9 {
				t.Fatalf("net shares = %v, want 98 after a 2-share-equivalent fee", fill.Shares)
			}
			if math.Abs(fill.Cost-50) > 1e-9 || fill.Fee != 0 {
				t.Fatalf("cost/fee = %v/%v, want gross 50 and Fee=0", fill.Cost, fill.Fee)
			}
		})
	}
}

func TestFillFromOrderSellIsNestedAndNetOfCollateralFee(t *testing.T) {
	executor := &Executor{}
	hash := "0xsell"
	order := testFillOrder(hash, SideSell, "FILLED", venue.Scale(100, venue.USDTDecimals).String())
	matches := []MatchData{testMatch(hash, false, 100, 0.5, "COLLATERAL", 1)}
	fill := executor.fillFromOrder(order, matches, hash)
	if fill.Status != "filled" || math.Abs(fill.Shares-100) > 1e-9 {
		t.Fatalf("fill = %+v, want 100 gross shares filled", fill)
	}
	if math.Abs(fill.Cost-49) > 1e-9 || fill.Fee != 0 {
		t.Fatalf("cost/fee = %v/%v, want net 49 and Fee=0", fill.Cost, fill.Fee)
	}
	if math.Abs(fill.Price-0.5) > 1e-9 {
		t.Fatalf("price = %v, want execution price 0.5", fill.Price)
	}
}

func TestFillFromOrderDoesNotDoubleChargeFee(t *testing.T) {
	executor := &Executor{}
	hash := "0xabc"
	buy := executor.fillFromOrder(
		testFillOrder(hash, SideBuy, "FILLED", venue.Scale(100, venue.USDTDecimals).String()),
		[]MatchData{testMatch(hash, true, 100, 0.5, "SHARES", 2)}, hash,
	)
	if booked := buy.Cost + buy.Fee; math.Abs(booked-50) > 1e-9 {
		t.Fatalf("engine buy cost = %v, want 50", booked)
	}
	sell := executor.fillFromOrder(
		testFillOrder(hash, SideSell, "FILLED", venue.Scale(98, venue.USDTDecimals).String()),
		[]MatchData{testMatch(hash, true, 98, 0.5, "COLLATERAL", 1)}, hash,
	)
	if proceeds := sell.Cost - sell.Fee; math.Abs(proceeds-48) > 1e-9 {
		t.Fatalf("engine sell proceeds = %v, want 48", proceeds)
	}
}

func TestFillFromOrderExplicitZeroFeeIsKnown(t *testing.T) {
	executor := &Executor{}
	hash := "0xzero"
	fill := executor.fillFromOrder(
		testFillOrder(hash, SideBuy, "FILLED", venue.Scale(100, venue.USDTDecimals).String()),
		[]MatchData{testMatch(hash, true, 100, 0.5, "SHARES", 0)}, hash,
	)
	if fill.Status != "filled" || math.Abs(fill.Shares-100) > 1e-9 || math.Abs(fill.Cost-50) > 1e-9 {
		t.Fatalf("explicit zero fee was estimated: %+v", fill)
	}
	if fill.Fee != 0 {
		t.Fatalf("fee = %v, want zero", fill.Fee)
	}
}

func TestFillFromOrderMissingEvidenceStaysUnknown(t *testing.T) {
	executor := &Executor{}
	hash := "0xabc"
	filled := venue.Scale(100, venue.USDTDecimals).String()
	cases := []struct {
		name    string
		order   OrderData
		matches []MatchData
	}{
		{
			name:    "missing amountFilled",
			order:   testFillOrder(hash, SideBuy, "FILLED", ""),
			matches: []MatchData{testMatch(hash, true, 100, 0.5, "SHARES", 0)},
		},
		{
			name:    "incomplete match set",
			order:   testFillOrder(hash, SideBuy, "FILLED", filled),
			matches: []MatchData{testMatch(hash, true, 40, 0.5, "SHARES", 0)},
		},
		{
			name:    "only another maker leg",
			order:   testFillOrder(hash, SideBuy, "FILLED", filled),
			matches: []MatchData{testMatch("0xother", true, 100, 0.5, "SHARES", 0)},
		},
		{
			name:    "missing fee object",
			order:   testFillOrder(hash, SideBuy, "FILLED", filled),
			matches: []MatchData{{TransactionHash: "0xtx", Taker: OrderFillData{Hash: hash, Amount: Number(filled), Price: Number("0.5")}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fill := executor.fillFromOrder(tc.order, tc.matches, hash)
			if fill.Status != "unknown" || fill.Shares != 0 || fill.Cost != 0 {
				t.Fatalf("fill = %+v, want unknown with no booked amounts", fill)
			}
		})
	}
}

func TestFillFromOrderPartialFilledIsNotPadded(t *testing.T) {
	executor := &Executor{}
	hash := "0xpartial"
	order := testFillOrder(hash, SideBuy, "FILLED", venue.Scale(40, venue.USDTDecimals).String())
	fill := executor.fillFromOrder(order, []MatchData{testMatch(hash, true, 40, 0.5, "SHARES", 0)}, hash)
	if fill.Status != "filled" || math.Abs(fill.Shares-40) > 1e-9 {
		t.Fatalf("partial MARKET fill = %+v, want exactly 40 shares", fill)
	}
}

func TestFillFromOrderOpenAmountFilledGrowthDoesNotFinalize(t *testing.T) {
	executor := &Executor{}
	hash := "0xopen"
	first := testFillOrder(hash, SideBuy, "OPEN", venue.Scale(40, venue.USDTDecimals).String())
	if fill := executor.fillFromOrder(first, []MatchData{testMatch(hash, true, 40, 0.5, "SHARES", 0)}, hash); fill.Status != "unknown" || fill.Shares != 0 {
		t.Fatalf("first OPEN increment was finalized: %+v", fill)
	}
	second := testFillOrder(hash, SideBuy, "FILLED", venue.Scale(100, venue.USDTDecimals).String())
	matches := []MatchData{
		testMatch(hash, true, 40, 0.5, "SHARES", 0),
		testMatch(hash, false, 60, 0.6, "SHARES", 0),
	}
	fill := executor.fillFromOrder(second, matches, hash)
	if fill.Status != "filled" || math.Abs(fill.Shares-100) > 1e-9 || math.Abs(fill.Cost-56) > 1e-9 {
		t.Fatalf("terminal cumulative fill = %+v, want 100 shares at 0.56", fill)
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

func TestApprovalFromKeepsRedemptionReadinessExplicit(t *testing.T) {
	executor := &Executor{}
	state := ApprovalState{
		CollateralBalance: big.NewInt(0),
		ExchangeAllowance: big.NewInt(1),
		NegRiskAllowance:  big.NewInt(1),
		ExchangeApproved:  true,
		NegRiskApproved:   true,
	}
	approval := executor.approvalFrom(state)
	if !approval.Ready || approval.RedemptionReady == nil || *approval.RedemptionReady {
		t.Fatalf("trading readiness leaked into redemption readiness: %+v", approval)
	}
	state.AdapterApproved = true
	approval = executor.approvalFrom(state)
	if approval.RedemptionReady == nil || !*approval.RedemptionReady {
		t.Fatalf("redemption readiness = %+v", approval.RedemptionReady)
	}
}
