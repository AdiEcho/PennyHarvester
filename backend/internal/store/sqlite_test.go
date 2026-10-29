package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"pennyharvester/backend/internal/engine"
)

func TestLegacyDatabaseMigratesToPaperAccount(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const legacySchema = `
	CREATE TABLE settings (id INTEGER PRIMARY KEY CHECK (id = 1), payload TEXT NOT NULL, updated_at TEXT NOT NULL);
	CREATE TABLE positions (
		id INTEGER PRIMARY KEY AUTOINCREMENT, market_id TEXT NOT NULL, condition_id TEXT NOT NULL,
		question TEXT NOT NULL, outcome TEXT NOT NULL, token_id TEXT NOT NULL, side TEXT NOT NULL,
		entry_price REAL NOT NULL, current_price REAL NOT NULL, shares REAL NOT NULL, cost REAL NOT NULL,
		fee REAL NOT NULL, status TEXT NOT NULL, opened_at TEXT NOT NULL, closed_at TEXT,
		end_date TEXT NOT NULL, realized_pnl REAL NOT NULL DEFAULT 0, unrealized_pnl REAL NOT NULL DEFAULT 0
	);
	CREATE TABLE orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT, position_id INTEGER, market_id TEXT NOT NULL,
		question TEXT NOT NULL, outcome TEXT NOT NULL, token_id TEXT NOT NULL,
		kind TEXT NOT NULL, type TEXT NOT NULL, amount REAL NOT NULL, price REAL NOT NULL,
		shares REAL NOT NULL, status TEXT NOT NULL, reason TEXT NOT NULL, created_at TEXT NOT NULL
	);
	CREATE TABLE equity_snapshots (id INTEGER PRIMARY KEY AUTOINCREMENT, time TEXT NOT NULL, value REAL NOT NULL);`
	if _, err := db.ExecContext(ctx, legacySchema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `INSERT INTO positions(
		market_id, condition_id, question, outcome, token_id, side, entry_price, current_price,
		shares, cost, fee, status, opened_at, end_date
	) VALUES('m1', 'c1', 'legacy question', 'Yes', 't1', 'BUY', 0.98, 0.98, 102, 100, 0, 'open', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO orders(
		position_id, market_id, question, outcome, token_id, kind, type, amount, price, shares, status, reason, created_at
	) VALUES(1, 'm1', 'legacy question', 'Yes', 't1', 'manual', 'buy', 100, 0.98, 102, 'filled', 'legacy', ?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO equity_snapshots(time, value) VALUES(?, 10100)`, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	accounts, err := repository.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var live, paper engine.Account
	for _, account := range accounts {
		switch account.Mode {
		case engine.AccountModeLive:
			live = account
		case engine.AccountModePaper:
			paper = account
		}
	}
	if live.ID == 0 || paper.ID == 0 {
		t.Fatalf("expected seeded live and paper accounts: %+v", accounts)
	}
	if !live.IsDefault {
		t.Fatal("live account must be the default after migration")
	}
	positions, err := repository.ListPositions(ctx, paper.ID, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 || positions[0].AccountID != paper.ID {
		t.Fatalf("legacy positions must migrate to the paper account: %+v", positions)
	}
	orders, err := repository.ListOrders(ctx, paper.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].ExternalID != "" {
		t.Fatalf("legacy orders must migrate with an empty external_id: %+v", orders)
	}
	points, err := repository.ListSnapshots(ctx, paper.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("legacy snapshots must migrate to the paper account: %+v", points)
	}
}
