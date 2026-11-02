package venue

import "pennyharvester/backend/internal/market"

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
		OrderTypes:     []string{"FAK", "FOK"},
		MaxSlippageBPS: 100,
		// No cancel path exists in this build's Polymarket executor (it only
		// places FAK buys and FOK sells), so advertising cancel would offer a
		// button that cannot work.
		SupportsCancel:     false,
		SupportsSettlement: true,
	}
}
