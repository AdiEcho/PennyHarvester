package engine

import (
	"testing"
	"time"
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
