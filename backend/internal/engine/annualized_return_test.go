package engine

import (
	"math"
	"testing"
)

func TestAnnualizedReturn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		totalReturn float64
		days        float64
		want        float64
	}{
		{name: "zero_window", totalReturn: 0.1, days: 0, want: 0},
		{name: "zero_window_with_loss", totalReturn: -1, days: 0, want: 0},
		{name: "negative_window", totalReturn: 0.1, days: -5, want: 0},
		{name: "negative_window_with_loss", totalReturn: -1, days: -5, want: 0},
		{name: "zero_return", totalReturn: 0, days: 5.0 / 1440, want: 0},
		{name: "five_days", totalReturn: 0.1, days: 5, want: 0.1 * 365 / 5},
		{name: "fractional_days", totalReturn: 0.39, days: 3.4, want: 0.39 * 365 / 3.4},
		{name: "half_day", totalReturn: 0.01, days: 0.5, want: 0.01 * 365 / 0.5},
		{name: "five_minutes", totalReturn: 0.01, days: 5.0 / 1440, want: 0.01 * 365 / (5.0 / 1440)},
		{name: "thirty_seconds", totalReturn: 0.001, days: 30.0 / 86400, want: 0.001 * 365 / (30.0 / 86400)},
		{name: "thirty_days", totalReturn: 0.1, days: 30, want: 0.1 * 365 / 30},
		{name: "just_under_year", totalReturn: 0.1, days: 364.5, want: 0.1 * 365 / 364.5},
		{name: "one_year", totalReturn: 0.1, days: 365, want: 0.1},
		{name: "four_hundred_days", totalReturn: 0.1, days: 400, want: math.Pow(1.1, 365.0/400) - 1},
		{name: "small_short_loss", totalReturn: -0.001, days: 5, want: -0.001 * 365 / 5},
		{name: "small_minute_loss", totalReturn: -0.0000001, days: 5.0 / 1440, want: -0.0000001 * 365 / (5.0 / 1440)},
		{name: "short_loss_floor", totalReturn: -0.02, days: 5, want: -1},
		{name: "minute_loss_floor", totalReturn: -0.001, days: 5.0 / 1440, want: -1},
		{name: "short_wipeout", totalReturn: -1, days: 5, want: -1},
		{name: "short_loss_beyond_wipeout", totalReturn: -1.1, days: 5, want: -1},
		{name: "year_loss", totalReturn: -0.5, days: 365, want: -0.5},
		{name: "long_loss", totalReturn: -0.5, days: 400, want: math.Pow(0.5, 365.0/400) - 1},
		{name: "long_wipeout", totalReturn: -1, days: 400, want: -1},
		{name: "long_loss_beyond_wipeout", totalReturn: -1.1, days: 400, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := annualizedReturn(tc.totalReturn, tc.days)
			if math.IsNaN(got) || math.IsInf(got, 0) || math.Abs(got-tc.want) > 1e-12*math.Max(1, math.Abs(tc.want)) {
				t.Fatalf("annualizedReturn(%v, %v) = %v, want %v", tc.totalReturn, tc.days, got, tc.want)
			}
			if got < -1 {
				t.Fatalf("annualized return must not fall below -100%%: %v", got)
			}
		})
	}
}
