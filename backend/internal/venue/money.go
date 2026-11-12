package venue

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
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
// toward zero.
//
// The float64 API is kept for callers that already carry amounts as float64, but
// float64 is not an exact money type: it holds about 15-16 significant decimal
// digits, and a 999999999999999999 wei balance reads back as 1.0 via Unscale.
// An amount that must survive an exact round trip (a full balance, a
// user-entered 18-decimal value) must stay a decimal string or *big.Int and use
// ScaleDecimal/UnscaleDecimal instead.
//
// The conversion parses the shortest decimal that round-trips, so binary noise
// such as 0.1's 0.10000000000000000555... is not scaled into the result. It
// still floors at the base unit: a positive amount smaller than one base unit
// becomes zero. Non-positive, NaN and infinite amounts return zero because a
// caller that failed to validate has no spendable amount. Rounding down is
// deliberate: a buy maker amount must never exceed what the caller approved.
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
	scaled := scaleRat(rat, decimals)
	return new(big.Int).Quo(scaled.Num(), scaled.Denom())
}

// ScaleDecimal converts an exact decimal string into base units without ever
// going through float64. It is the safe path for user-entered 18-decimal
// amounts and full balances that must not lose a wei.
//
// Negative amounts are preserved. The text may use the decimal forms big.Rat
// accepts ("1.5", "1e-18"); it must also be exactly representable at the
// requested precision. A value with more fractional digits than decimals
// returns an error rather than silently rounding the amount down, because that
// would change the amount the user entered.
func ScaleDecimal(text string, decimals int) (*big.Int, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("venue: empty decimal amount")
	}
	rat, ok := new(big.Rat).SetString(trimmed)
	if !ok {
		return nil, fmt.Errorf("venue: invalid decimal amount %q", text)
	}
	scaled := scaleRat(rat, decimals)
	if !scaled.IsInt() {
		return nil, fmt.Errorf("venue: decimal amount %q has more than %d decimal places", text, decimals)
	}
	return new(big.Int).Set(scaled.Num()), nil
}

// scaleRat multiplies a decimal amount by 10^decimals. The caller decides how
// to handle a non-integer result; keeping the rational avoids any float step.
func scaleRat(rat *big.Rat, decimals int) *big.Rat {
	return new(big.Rat).Mul(rat, new(big.Rat).SetInt(pow10(decimals)))
}

// Unscale converts integer base units back to a decimal amount. It goes through
// big.Rat so 1e18 wei is exactly 1.0 and not 0.9999999999999999.
//
// float64 cannot represent every 18-decimal integer: Unscale(999999999999999999,
// 18) is 1.0, not 0.999999999999999999. Use UnscaleDecimal or UnscaleRat when
// the raw value must be preserved. Values outside float64's range return ±Inf.
func Unscale(raw *big.Int, decimals int) float64 {
	if raw == nil || raw.Sign() == 0 {
		return 0
	}
	value, _ := UnscaleRat(raw, decimals).Float64()
	return value
}

// UnscaleRat converts base units to an exact rational number. It is the
// lossless arithmetic counterpart of Unscale.
func UnscaleRat(raw *big.Int, decimals int) *big.Rat {
	if raw == nil {
		return new(big.Rat)
	}
	return new(big.Rat).SetFrac(new(big.Int).Set(raw), pow10(decimals))
}

// UnscaleDecimal renders base units as an exact decimal string, trimming
// trailing fractional zeros. A nil value is zero. Negative values keep their
// sign. Unlike Unscale, this can round-trip an arbitrary 18-decimal integer
// through ScaleDecimal without losing a wei.
func UnscaleDecimal(raw *big.Int, decimals int) string {
	if raw == nil || raw.Sign() == 0 {
		return "0"
	}
	sign := ""
	if raw.Sign() < 0 {
		sign = "-"
	}
	digits := new(big.Int).Abs(raw).String()
	if decimals <= 0 {
		return sign + digits
	}
	if len(digits) <= decimals {
		digits = strings.Repeat("0", decimals-len(digits)+1) + digits
	}
	cut := len(digits) - decimals
	whole := digits[:cut]
	fraction := strings.TrimRight(digits[cut:], "0")
	if fraction == "" {
		return sign + whole
	}
	return sign + whole + "." + fraction
}

func pow10(n int) *big.Int {
	if n <= 0 {
		return big.NewInt(1)
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// maxPriceTickDecimals caps PriceDecimalsForTick at the collateral's own
// precision. A finer price tick cannot be signed as an 18-decimal amount, so
// reporting more places would only expose float noise.
const maxPriceTickDecimals = USDTDecimals

// Quantize rounds a price down to the venue's tick. Exchanges reject a price
// that is not a multiple of the tick, and rounding up could cross above the
// caller's limit, so the direction is always down.
func Quantize(price, tick float64) float64 {
	if tick <= 0 || price <= 0 {
		return price
	}
	return math.Floor(price/tick+1e-9) * tick
}

// PriceDecimalsForTick returns how many decimal places a tick needs, derived
// from the tick's own decimal form rather than from magnitude thresholds. A
// 0.5 tick yields 1, 0.025 yields 3, 0.001 yields 3 and 0.0001 yields 4.
//
// Zero, negative, NaN and infinite ticks yield 0. A float tick that is not a
// short decimal (for example 1.0/3.0) reports the digits of its shortest
// round-tripping decimal text; the result is capped at maxPriceTickDecimals
// (18) so a subnormal or sub-wei tick cannot request an unbounded scale.
func PriceDecimalsForTick(tick float64) int {
	if tick <= 0 || math.IsNaN(tick) || math.IsInf(tick, 0) {
		return 0
	}
	// FormatFloat('f', -1) yields the shortest decimal that round-trips and
	// never an exponent, which is exactly the text a tick is written as.
	text := strconv.FormatFloat(tick, 'f', -1, 64)
	dot := strings.IndexByte(text, '.')
	if dot < 0 {
		return 0
	}
	fraction := strings.TrimRight(text[dot+1:], "0")
	if len(fraction) > maxPriceTickDecimals {
		return maxPriceTickDecimals
	}
	return len(fraction)
}
