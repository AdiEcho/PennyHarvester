package engine

import (
	"testing"
	"time"

	"pennyharvester/backend/internal/market"
)

func TestEquityWindowSeriesAnchorsAndDownsamples(t *testing.T) {
	since := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	now := since.Add(time.Hour)
	boundary := &EquityPoint{Time: since.Add(-time.Hour), Value: 90}
	points := make([]EquityPoint, 1000)
	for i := range points {
		points[i] = EquityPoint{Time: since.Add(time.Duration(i) * 3600 * time.Millisecond / time.Duration(len(points))), Value: float64(100 + i)}
	}

	series := equityWindowSeries(boundary, points, since, now, 10)
	if len(series) > 11 {
		t.Fatalf("series has %d points, want at most maxPoints+1", len(series))
	}
	if !series[0].Time.Equal(since) || series[0].Value != 90 {
		t.Fatalf("first point = %+v, want the boundary anchored at the window start", series[0])
	}
	if series[len(series)-1].Value != points[len(points)-1].Value {
		t.Fatalf("last point = %v, want the newest value %v", series[len(series)-1].Value, points[len(points)-1].Value)
	}
	for i := 1; i < len(series); i++ {
		if series[i].Time.Before(series[i-1].Time) {
			t.Fatalf("series time went backwards at %d: %+v", i, series[i-1:i+1])
		}
	}
	if series[len(series)-1].Time.After(now) {
		t.Fatalf("last point %v is after the window end %v", series[len(series)-1].Time, now)
	}
}

func TestEquityWindowSeriesKeepsShortSeriesAndClampsBounds(t *testing.T) {
	since := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	now := since.Add(time.Hour)
	points := []EquityPoint{
		{Time: since.Add(-time.Minute), Value: 10},
		{Time: since.Add(30 * time.Minute), Value: 11},
		{Time: now.Add(time.Minute), Value: 12},
	}

	series := equityWindowSeries(nil, points, since, now, 300)
	if len(series) != 3 {
		t.Fatalf("short series length = %d, want 3", len(series))
	}
	if !series[0].Time.Equal(since) || !series[2].Time.Equal(now) {
		t.Fatalf("bounds were not clamped: %+v", series)
	}
}

func TestRealizedEquitySeriesUsesClosedPositions(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(value time.Time) *time.Time { return &value }
	positions := []Position{
		// Closed before the window: its PnL is already part of the opening equity.
		{RealizedPNL: 5, ClosedAt: at(start.Add(-time.Hour))},
		{RealizedPNL: 2, ClosedAt: at(start.Add(time.Hour))},
		{RealizedPNL: -3, ClosedAt: at(start.Add(2 * time.Hour))},
		{Status: "open"},
	}

	series := realizedEquitySeries(positions, 100, start, start.Add(3*time.Hour), time.Hour, market.VenuePolymarket, market.CurrencyUSDC)
	want := []float64{105, 107, 104, 104}
	if len(series) != len(want) {
		t.Fatalf("series length = %d, want %d: %+v", len(series), len(want), series)
	}
	for i, value := range want {
		if series[i].Value != value {
			t.Fatalf("point %d value = %v, want %v", i, series[i].Value, value)
		}
		if !series[i].Estimated {
			t.Fatalf("point %d is not marked estimated", i)
		}
	}
}

func TestSnapshotDueThrottlesToInterval(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	if !snapshotDue(nil, now) {
		t.Fatal("an account without snapshots should record immediately")
	}
	if snapshotDue([]EquityPoint{{Time: now.Add(-30 * time.Second)}}, now) {
		t.Fatal("a snapshot inside the interval should be skipped")
	}
	if !snapshotDue([]EquityPoint{{Time: now.Add(-snapshotInterval)}}, now) {
		t.Fatal("a snapshot at the interval boundary should be recorded")
	}
	if !snapshotDue([]EquityPoint{{Time: now.Add(time.Minute)}}, now) {
		t.Fatal("a backwards clock must not block recording until it catches up")
	}
}
