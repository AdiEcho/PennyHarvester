package venue

import (
	"context"

	"pennyharvester/backend/internal/market"
)

// RedeemRequest asks a venue to burn settled outcome shares back into
// collateral. Shares are human units of the venue's collateral token, exactly
// what a position row shows; the executor converts them to base units, because
// a float64 that has already been rounded is not what the chain should receive.
type RedeemRequest struct {
	// MarketID is the venue's own market id, when the caller has it. A venue can
	// use it to load redemption metadata after a restart without rescanning the
	// open-market list (a resolved market is no longer in that list).
	MarketID string `json:"market_id,omitempty"`
	// ConditionID is the market's condition id, used by every CTF-shaped venue.
	ConditionID string `json:"condition_id"`
	// TokenID identifies which outcome shares are burned.
	TokenID string `json:"token_id"`
	// Shares is how many outcome shares to redeem.
	Shares float64 `json:"shares"`
}

// RedeemResult reports one submitted redemption. TxHash is the only durable
// handle: the engine records it so a second click cannot spend gas twice.
type RedeemResult struct {
	Venue market.Venue `json:"venue"`
	// TxHash is the on-chain transaction hash (0x-prefixed hex).
	TxHash string `json:"tx_hash"`
	// Method names the contract path, for the UI and for support: "ctf" for the
	// plain Conditional Tokens redeemPositions, "neg-risk" for the adapter's.
	Method string `json:"method"`
}

// Redeemer burns resolved shares for collateral.
//
// It is deliberately separate from Executor: trading and claiming are different
// capabilities, and a venue can be able to trade without being able to claim
// (for example when the redemption transaction needs gas the operator has not
// funded). Keeping them apart lets the UI say which one is missing instead of
// failing at the last step of a settlement the operator already trusts.
type Redeemer interface {
	Redeem(ctx context.Context, request RedeemRequest) (RedeemResult, error)
}
