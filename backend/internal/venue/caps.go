package venue

import "pennyharvester/backend/internal/market"

// PolymarketMinSellShares is the smallest sell the Polymarket CLOB accepts:
// every sell this build submits is a limit order, and limit orders are floored
// at five shares. A buy is sized in USDC and only needs one dollar, so a small
// buy creates a position that can be held to settlement but never sold, which
// is why the stop-loss refuses to run against one.
const PolymarketMinSellShares = 5

// PolymarketCapabilities is the fact sheet for the original venue. The numbers
// mirror what this build has always done with Polymarket: 6-decimal USDC, a
// one-dollar minimum order, FAK market buys / FOK market sells, and settlement
// from Gamma reference prices.
func PolymarketCapabilities() Capabilities {
	return Capabilities{
		Venue:          market.VenuePolymarket,
		Label:          "Polymarket",
		Currency:       market.CurrencyUSDC,
		QuoteDecimals:  USDCDecimals,
		MinNotional:    1,
		MinSellShares:  PolymarketMinSellShares,
		OrderTypes:     []string{"FAK", "FOK"},
		MaxSlippageBPS: 100,
		// No cancel path exists in this build's Polymarket executor (it only
		// places FAK buys and FOK sells), so advertising cancel would offer a
		// button that cannot work.
		SupportsCancel:     false,
		SupportsSettlement: true,
	}
}
