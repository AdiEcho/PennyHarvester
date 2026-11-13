package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

const benchOrderInsert = `INSERT INTO orders(
	account_id, venue, currency, position_id, market_id, event_id, question, outcome, token_id, kind, type, action,
	amount, price, shares, fee, status, reason, external_id, created_at
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const benchPositionInsert = `INSERT INTO positions(
	account_id, venue, currency, market_id, event_id, condition_id, question, outcome, token_id, side, kind, exit_kind,
	entry_price, current_price, shares, cost, fee, status, opened_at, closed_at, end_date, realized_pnl, unrealized_pnl
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// newBenchmarkStore opens a temporary SQLite database with the production
// schema and seeds deterministic history. The two seeded accounts are the
// default live account and the paper account created by migrate.
func newBenchmarkStore(b *testing.B, primaryOrders, secondaryOrders, positions int) (*SQLite, int64, int64) {
	b.Helper()
	ctx := context.Background()
	instance, err := Open(ctx, filepath.Join(b.TempDir(), "benchmark.db"))
	if err != nil {
		b.Fatalf("open benchmark store: %v", err)
	}
	b.Cleanup(func() { instance.Close() })

	accounts, err := instance.ListAccounts(ctx)
	if err != nil {
		b.Fatalf("list benchmark accounts: %v", err)
	}
	if len(accounts) < 2 {
		b.Fatalf("expected the two seeded accounts, got %d", len(accounts))
	}
	primary, secondary := accounts[0].ID, accounts[1].ID

	if primaryOrders+secondaryOrders > 0 {
		tx, err := instance.db.BeginTx(ctx, nil)
		if err != nil {
			b.Fatalf("begin order seed: %v", err)
		}
		defer tx.Rollback()
		statement, err := tx.PrepareContext(ctx, benchOrderInsert)
		if err != nil {
			b.Fatalf("prepare order seed: %v", err)
		}
		defer statement.Close()
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		seedOrders := func(accountID int64, count int) {
			for i := 0; i < count; i++ {
				status := "filled"
				if i%10 == 0 {
					status = "rejected"
				}
				if _, err := statement.ExecContext(ctx, accountID, market.VenuePolymarket, market.CurrencyUSDC,
					nil, fmt.Sprintf("bench-market-%05d", i), "", fmt.Sprintf("Benchmark question %05d", i),
					"Yes", fmt.Sprintf("bench-token-%05d", i), "manual", "buy", "",
					100.0, 0.96, 100.0/0.96, 0.0, status, "benchmark seed", "",
					formatTime(base.Add(time.Duration(i)*time.Minute))); err != nil {
					b.Fatalf("seed order %d for account %d: %v", i, accountID, err)
				}
			}
		}
		seedOrders(primary, primaryOrders)
		seedOrders(secondary, secondaryOrders)
		if err := tx.Commit(); err != nil {
			b.Fatalf("commit order seed: %v", err)
		}
	}

	if positions > 0 {
		tx, err := instance.db.BeginTx(ctx, nil)
		if err != nil {
			b.Fatalf("begin position seed: %v", err)
		}
		defer tx.Rollback()
		statement, err := tx.PrepareContext(ctx, benchPositionInsert)
		if err != nil {
			b.Fatalf("prepare position seed: %v", err)
		}
		defer statement.Close()
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < positions; i++ {
			marketID := fmt.Sprintf("bench-position-market-%05d", i)
			if _, err := statement.ExecContext(ctx, primary, market.VenuePolymarket, market.CurrencyUSDC,
				marketID, "", "condition-"+marketID, "Benchmark position "+marketID,
				"Yes", "bench-position-token-"+marketID, "buy", "manual", "",
				0.96, 0.96, 100.0/0.96, 100.0, 0.0, "open",
				formatTime(base.Add(time.Duration(i)*time.Minute)), nil,
				formatTime(base.AddDate(0, 0, 30)), 0.0, 0.0); err != nil {
				b.Fatalf("seed position %d: %v", i, err)
			}
		}
		if err := tx.Commit(); err != nil {
			b.Fatalf("commit position seed: %v", err)
		}
	}
	return instance, primary, secondary
}

func BenchmarkListOrders(b *testing.B) {
	const (
		primaryOrders   = 9900
		secondaryOrders = 100
	)
	instance, primary, _ := newBenchmarkStore(b, primaryOrders, secondaryOrders, 0)
	ctx := context.Background()

	check := func(b *testing.B, first []engine.Order, want int) {
		b.Helper()
		if len(first) != want {
			b.Fatalf("expected %d orders, got %d", want, len(first))
		}
	}

	b.Run("TypicalPage", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			orders, err := instance.ListOrders(ctx, primary, "", 100, 0, "")
			if err != nil {
				b.Fatalf("typical page: %v", err)
			}
			if i == 0 {
				check(b, orders, 100)
			}
		}
	})

	b.Run("DeepPage", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			orders, err := instance.ListOrders(ctx, primary, "", 100, 9000, "")
			if err != nil {
				b.Fatalf("deep page: %v", err)
			}
			if i == 0 {
				check(b, orders, 100)
			}
		}
	})

	b.Run("StatusFiltered", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			orders, err := instance.ListOrders(ctx, primary, "rejected", 100, 0, "")
			if err != nil {
				b.Fatalf("status filter: %v", err)
			}
			if i == 0 {
				check(b, orders, 100)
				for _, order := range orders {
					if order.Status != "rejected" {
						b.Fatalf("status filter returned status %q", order.Status)
					}
				}
			}
		}
	})
}

func BenchmarkListPositions(b *testing.B) {
	const positions = 2000
	instance, primary, _ := newBenchmarkStore(b, 0, 0, positions)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		items, err := instance.ListPositions(ctx, primary, "open", "")
		if err != nil {
			b.Fatalf("list positions: %v", err)
		}
		if i == 0 && len(items) != positions {
			b.Fatalf("expected %d positions, got %d", positions, len(items))
		}
	}
}
