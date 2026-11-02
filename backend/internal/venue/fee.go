package venue

import "pennyharvester/backend/internal/market"

// FeeFunc computes a fill's taker fee in collateral units from the share count
// and the execution price. Paper trading uses it so a simulated fill in one
// venue is charged the way that venue actually charges.
type FeeFunc func(shares, price float64) float64

// FeeModelFor returns the fee model for a venue at the given taker rate.
//
// Polymarket charges `shares × rate × p × (1-p)`; this build's paper model
// charges `notional × rate`, which is the same thing with (1-p) folded out and
// is what every stored Polymarket paper result was produced with, so it is kept
// unchanged to avoid rewriting history.
//
// Predict.fun charges `rate × min(p, 1-p) × shares`. At the penny prices this
// strategy hunts for it is very close to the Polymarket model, but it is not
// the same function: on a 50¢ market Predict.fun takes twice as much, which is
// exactly the case where a shared model would misprice a trade.
func FeeModelFor(v market.Venue, rateBPS float64) FeeFunc {
	rate := rateBPS / 10000
	switch v {
	case market.VenuePredictFun:
		return func(shares, price float64) float64 {
			return rate * minFloat(price, 1-price) * shares
		}
	default:
		return func(shares, price float64) float64 {
			return rate * shares * price
		}
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
