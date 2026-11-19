package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

func TestListSnapshotWindowAnchorsAtWindowStart(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(t.TempDir(), "equity-window.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	account, err := repository.CreateAccount(ctx, engine.Account{
		Name: "window-paper", Mode: engine.AccountModePaper, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	for i, value := range []float64{100, 101, 102, 103, 104} {
		if err := repository.AddSnapshot(ctx, account.ID, engine.EquityPoint{
			Venue: market.VenuePolymarket, Currency: market.CurrencyUSDC,
			Time: base.Add(time.Duration(i) * time.Hour), Value: value,
		}); err != nil {
			t.Fatal(err)
		}
	}

	boundary, points, err := repository.ListSnapshotWindow(ctx, account.ID, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if boundary == nil || boundary.Value != 102 {
		t.Fatalf("boundary = %+v, want the snapshot at the window start (102)", boundary)
	}
	if len(points) != 2 || points[0].Value != 103 || points[1].Value != 104 {
		t.Fatalf("in-window points = %+v, want 103..104", points)
	}
}

func TestListSnapshotWindowWithoutEarlierPointHasNoBoundary(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(t.TempDir(), "equity-first.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	account, err := repository.CreateAccount(ctx, engine.Account{
		Name: "first-window-paper", Mode: engine.AccountModePaper, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := repository.AddSnapshot(ctx, account.ID, engine.EquityPoint{
		Venue: market.VenuePolymarket, Currency: market.CurrencyUSDC,
		Time: start.Add(10 * time.Minute), Value: 250,
	}); err != nil {
		t.Fatal(err)
	}

	boundary, points, err := repository.ListSnapshotWindow(ctx, account.ID, start)
	if err != nil {
		t.Fatal(err)
	}
	if boundary != nil {
		t.Fatalf("boundary = %+v, want nil before the first snapshot", boundary)
	}
	if len(points) != 1 || points[0].Value != 250 {
		t.Fatalf("in-window points = %+v, want the only snapshot", points)
	}
}

func TestListSnapshotWindowKeepsSameSecondPointBeforeCutoff(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(t.TempDir(), "equity-fraction.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	account, err := repository.CreateAccount(ctx, engine.Account{
		Name: "fraction-paper", Mode: engine.AccountModePaper, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// RFC3339Nano trims trailing zeros, so the anchor's .1s sorts after a .123s
	// cutoff even though it happened first. The widened window query must
	// recover it as the boundary rather than dropping it between queries.
	since := time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
	for _, point := range []engine.EquityPoint{
		{Venue: market.VenuePolymarket, Currency: market.CurrencyUSDC, Time: since.Add(-23 * time.Millisecond), Value: 100},
		{Venue: market.VenuePolymarket, Currency: market.CurrencyUSDC, Time: since.Add(10 * time.Millisecond), Value: 101},
	} {
		if err := repository.AddSnapshot(ctx, account.ID, point); err != nil {
			t.Fatal(err)
		}
	}

	boundary, points, err := repository.ListSnapshotWindow(ctx, account.ID, since)
	if err != nil {
		t.Fatal(err)
	}
	if boundary == nil || boundary.Value != 100 {
		t.Fatalf("boundary = %+v, want the same-second point before the cutoff (100)", boundary)
	}
	if len(points) != 1 || points[0].Value != 101 {
		t.Fatalf("in-window points = %+v, want 101", points)
	}
}
