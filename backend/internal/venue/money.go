package venue

import (
	"math"
	"math/big"
	"strconv"
)

// CollateralDecimals is the exactness boundary of this codebase.
//
// Polymarket's collateral has 6 decimals and this build has always carried
// amounts as float64, which is safe there. Predict.fun signs 18-decimal
// integers; a float64 holds ~15-16 significant digits, so converting a
// float64 straight into wei silently rounds the last three digits. Every
// amount that is signed is therefore scaled from its decimal text form
// (Scale) and never from `amount * 1e18`.
const (
	USDCDecimals = 6
	USDTDecimals = 18
)

// Scale converts a decimal amount into the venue's integer base units, rounding
// toward zero. Rounding down is deliberate: a buy maker amount must never
// exceed what the caller approved, and an extra wei on an approval is harmless
// whereas an extra wei on a spend is not.
func Scale(amount float64, decimals int) *big.Int {
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return new(big.Int)
	}
	// big.Rat is built from the shortest decimal that round-trips, not from the
	// float's exact binary value. SetFloat64 would give 0.1 as
	// 0.1000000000000000055511151231257827 and scale to 100000000000000005 wei;
	// parsing "0.1" gives exactly one tenth.
	text := strconv.FormatFloat(amount, 'f', -1, 64)
	rat, ok := new(big.Rat).SetString(text)
	if !ok {
		return new(big.Int)
	}
	scale := new(big.Rat).SetInt(pow10(decimals))
	rat.Mul(rat, scale)
	return new(big.Int).Quo(rat.Num(), rat.Denom())
}

// Unscale converts integer base units back to a decimal amount. It goes through
// big.Rat so 1e18 wei is exactly 1.0 and not 0.9999999999999999.
func Unscale(raw *big.Int, decimals int) float64 {
	if raw == nil || raw.Sign() == 0 {
		return 0
	}
	rat := new(big.Rat).SetInt(raw)
	rat.Quo(rat, new(big.Rat).SetInt(pow10(decimals)))
	value, _ := rat.Float64()
	return value
}

func pow10(n int) *big.Int {
	if n <= 0 {
		return big.NewInt(1)
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// Quantize rounds a price down to the venue's tick. Exchanges reject a price
// that is not a multiple of the tick, and rounding up could cross above the
// caller's limit, so the direction is always down.
func Quantize(price, tick float64) float64 {
	if tick <= 0 || price <= 0 {
		return price
	}
	return math.Floor(price/tick+1e-9) * tick
}

// PriceDecimalsForTick returns how many decimal places a tick needs. A 0.01
// tick yields 2, a 0.001 tick yields 3.
func PriceDecimalsForTick(tick float64) int {
	switch {
	case tick <= 0:
		return 3
	case tick >= 0.1:
		return 1
	case tick >= 0.01:
		return 2
	default:
		return 3
	}
}
