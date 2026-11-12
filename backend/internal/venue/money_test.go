package venue

import (
	"math"
	"math/big"
	"testing"
)

func mustBig(t *testing.T, text string) *big.Int {
	t.Helper()
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		t.Fatalf("invalid big.Int fixture %q", text)
	}
	return value
}

func TestScaleExactFinancialInputs(t *testing.T) {
	// A runtime sum, not a compile-time constant expression, so the value is
	// the binary float64 0.30000000000000004 the caller would actually hold.
	noisySum := 0.1
	noisySum += 0.2

	cases := []struct {
		name     string
		amount   float64
		decimals int
		want     string
	}{
		{"one", 1, 18, "1000000000000000000"},
		{"tenth", 0.1, 18, "100000000000000000"},
		{"half", 0.5, 18, "500000000000000000"},
		{"one and a half", 1.5, 18, "1500000000000000000"},
		{"five hundredths", 0.05, 18, "50000000000000000"},
		{"seven cents", 0.07, 18, "70000000000000000"},
		{"one basis point", 0.0001, 18, "100000000000000"},
		{"one wei", 1e-18, 18, "1"},
		{"one tick", 0.025, 18, "25000000000000000"},
		{"usdc", 12.345678, 6, "12345678"},
		{"usdc floors the seventh digit", 12.3456789, 6, "12345678"},
		{"shortest decimal of a noisy sum", noisySum, 18, "300000000000000040"},
		{"next float above one", math.Nextafter(1, 2), 18, "1000000000000000200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Scale(tc.amount, tc.decimals).String(); got != tc.want {
				t.Fatalf("Scale(%v, %d) = %s, want %s", tc.amount, tc.decimals, got, tc.want)
			}
		})
	}
}

func TestScaleRejectsNonFiniteAndNonPositiveAmounts(t *testing.T) {
	amounts := []struct {
		name   string
		amount float64
	}{
		{"zero", 0},
		{"negative zero", math.Copysign(0, -1)},
		{"negative", -3},
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
	}
	for _, tc := range amounts {
		t.Run(tc.name, func(t *testing.T) {
			if got := Scale(tc.amount, 18); got.Sign() != 0 {
				t.Fatalf("Scale(%v, 18) = %s, want 0", tc.amount, got)
			}
		})
	}
	// A positive value below one base unit cannot be represented and floors to
	// zero. Exactness at that boundary belongs to ScaleDecimal, which reports
	// lost digits instead of silently dropping them.
	if got := Scale(4.9e-19, 18); got.Sign() != 0 {
		t.Fatalf("sub-wei Scale = %s, want 0", got)
	}
}

func TestScaleDecimalIsExact(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		decimals int
		want     string
	}{
		{"tenth", "0.1", 18, "100000000000000000"},
		{"one and a half", "1.5", 18, "1500000000000000000"},
		{"usdc", "12.345678", 6, "12345678"},
		{"one wei", "0.000000000000000001", 18, "1"},
		{"all eighteen decimals", "0.123456789012345678", 18, "123456789012345678"},
		{"negative all eighteen decimals", "-1.234567890123456789", 18, "-1234567890123456789"},
		{"negative usdc", "-12.345678", 6, "-12345678"},
		{"exponent form", "1.5e-3", 18, "1500000000000000"},
		{"whole base units", "999999999999999999", 0, "999999999999999999"},
		{"zero", "0", 18, "0"},
		{"surrounding spaces", " 2.5 ", 18, "2500000000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScaleDecimal(tc.text, tc.decimals)
			if err != nil {
				t.Fatalf("ScaleDecimal(%q, %d): %v", tc.text, tc.decimals, err)
			}
			if got.String() != tc.want {
				t.Fatalf("ScaleDecimal(%q, %d) = %s, want %s", tc.text, tc.decimals, got, tc.want)
			}
		})
	}
}

func TestScaleDecimalRefusesToDropDigits(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		decimals int
	}{
		{"seventh decimal at six", "0.1234567", 6},
		{"nineteenth decimal at eighteen", "1.0000000000000000001", 18},
		{"sub-wei amount", "0.0000000000000000005", 18},
		{"fraction form", "1/3", 18},
		{"empty", "", 18},
		{"not a number", "abc", 18},
		{"two decimal points", "1.2.3", 18},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ScaleDecimal(tc.text, tc.decimals); err == nil {
				t.Fatalf("ScaleDecimal(%q, %d) = %s, want an error", tc.text, tc.decimals, got)
			}
		})
	}
}

func TestEighteenDecimalRoundTripStaysExact(t *testing.T) {
	raw := mustBig(t, "999999999999999999")

	// float64 cannot distinguish this balance from one unit, which is exactly
	// why the precise helpers exist.
	if got := Unscale(raw, USDTDecimals); got != 1 {
		t.Fatalf("Unscale(%s, 18) = %v, want the documented float64 limit 1.0", raw, got)
	}

	text := UnscaleDecimal(raw, USDTDecimals)
	if text != "0.999999999999999999" {
		t.Fatalf("UnscaleDecimal = %q, want 0.999999999999999999", text)
	}
	got, err := ScaleDecimal(text, USDTDecimals)
	if err != nil {
		t.Fatalf("ScaleDecimal(%q): %v", text, err)
	}
	if got.Cmp(raw) != 0 {
		t.Fatalf("round trip = %s, want %s", got, raw)
	}
}

func TestUnscaleDecimalFormatting(t *testing.T) {
	cases := []struct {
		name     string
		raw      *big.Int
		decimals int
		want     string
	}{
		{"nil", nil, 18, "0"},
		{"zero", big.NewInt(0), 18, "0"},
		{"one wei", big.NewInt(1), 18, "0.000000000000000001"},
		{"negative one wei", big.NewInt(-1), 18, "-0.000000000000000001"},
		{"fifteen tenths", mustBig(t, "1500000000000000000"), 18, "1.5"},
		{"negative fifteen tenths", mustBig(t, "-1500000000000000000"), 18, "-1.5"},
		{"usdc with trailing zeros", mustBig(t, "1234500"), 6, "1.2345"},
		{"whole value", mustBig(t, "100"), 2, "1"},
		{"two decimals", mustBig(t, "123"), 2, "1.23"},
		{"no decimals", big.NewInt(42), 0, "42"},
		{"all eighteen decimals", mustBig(t, "1234567890123456789"), 18, "1.234567890123456789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnscaleDecimal(tc.raw, tc.decimals); got != tc.want {
				t.Fatalf("UnscaleDecimal(%v, %d) = %q, want %q", tc.raw, tc.decimals, got, tc.want)
			}
		})
	}
}

func TestUnscaleHandlesNilNegativeAndOverflow(t *testing.T) {
	if got := Unscale(nil, 18); got != 0 {
		t.Fatalf("Unscale(nil) = %v, want 0", got)
	}
	if got := Unscale(big.NewInt(-1500000000000000000), 18); got != -1.5 {
		t.Fatalf("Unscale(negative) = %v, want -1.5", got)
	}
	if got := Unscale(big.NewInt(1), 18); got != 1e-18 {
		t.Fatalf("Unscale(1 wei) = %v, want 1e-18", got)
	}

	huge := new(big.Int).Exp(big.NewInt(10), big.NewInt(400), nil)
	if got := Unscale(huge, 0); !math.IsInf(got, 1) {
		t.Fatalf("Unscale(10^400) = %v, want +Inf", got)
	}
	if got := Unscale(new(big.Int).Neg(huge), 0); !math.IsInf(got, -1) {
		t.Fatalf("Unscale(-10^400) = %v, want -Inf", got)
	}
}

func TestUnscaleRatIsExact(t *testing.T) {
	raw := mustBig(t, "999999999999999999")
	rat := UnscaleRat(raw, USDTDecimals)
	if got := rat.RatString(); got != "999999999999999999/1000000000000000000" {
		t.Fatalf("UnscaleRat = %s, want the exact fraction", got)
	}
	if got := UnscaleRat(nil, 18).RatString(); got != "0" {
		t.Fatalf("UnscaleRat(nil) = %s, want 0", got)
	}
}

func TestPriceDecimalsForTickUsesDecimalForm(t *testing.T) {
	noisySum := 0.1
	noisySum += 0.2

	cases := []struct {
		name string
		tick float64
		want int
	}{
		{"half", 0.5, 1},
		{"tenth", 0.1, 1},
		{"five hundredths", 0.05, 2},
		{"twenty five thousandths", 0.025, 3},
		{"hundredth", 0.01, 2},
		{"thousandth", 0.001, 3},
		{"ten thousandth", 0.0001, 4},
		{"hundred thousandth", 0.00001, 5},
		{"one wei", 1e-18, 18},
		{"finer than a wei is capped", 1e-19, 18},
		{"smallest subnormal is capped", math.SmallestNonzeroFloat64, 18},
		{"whole tick", 1, 0},
		{"zero", 0, 0},
		{"negative", -0.01, 0},
		{"NaN", math.NaN(), 0},
		{"positive infinity", math.Inf(1), 0},
		{"negative infinity", math.Inf(-1), 0},
		{"one third", 1.0 / 3.0, 16},
		{"noisy sum", noisySum, 17},
		{"next float above a tenth of a cent", math.Nextafter(0.01, 1), 18},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PriceDecimalsForTick(tc.tick); got != tc.want {
				t.Fatalf("PriceDecimalsForTick(%v) = %d, want %d", tc.tick, got, tc.want)
			}
		})
	}
}

func TestPriceDecimalsForTickTextIsBounded(t *testing.T) {
	// Regression guard for the old magnitude switch: 0.0001 must not report 3,
	// and 0.025 must not report 2.
	for _, tc := range []struct {
		tick      float64
		forbidden int
	}{
		{0.0001, 3},
		{0.025, 2},
	} {
		if got := PriceDecimalsForTick(tc.tick); got == tc.forbidden {
			t.Fatalf("PriceDecimalsForTick(%v) = %d, the old incorrect answer", tc.tick, got)
		}
	}
	if got := PriceDecimalsForTick(1e-300); got != maxPriceTickDecimals {
		t.Fatalf("subnormal tick decimals = %d, want cap %d", got, maxPriceTickDecimals)
	}
}
