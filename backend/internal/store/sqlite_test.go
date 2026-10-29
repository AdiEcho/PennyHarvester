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
	orders, err := repository.ListOrders(ctx, paper.ID, "all", 10, 0)
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

func TestSchemaMigratesLegacyStatusConstraints(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old-check.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const oldSchema = `
	CREATE TABLE positions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		market_id TEXT NOT NULL, condition_id TEXT NOT NULL, question TEXT NOT NULL,
		outcome TEXT NOT NULL, token_id TEXT NOT NULL, side TEXT NOT NULL,
		entry_price REAL NOT NULL, current_price REAL NOT NULL, shares REAL NOT NULL,
		cost REAL NOT NULL, fee REAL NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('open', 'won', 'lost')),
		opened_at TEXT NOT NULL, closed_at TEXT, end_date TEXT NOT NULL,
		realized_pnl REAL NOT NULL DEFAULT 0, unrealized_pnl REAL NOT NULL DEFAULT 0
	);
	CREATE TABLE orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		position_id INTEGER REFERENCES positions(id),
		market_id TEXT NOT NULL, question TEXT NOT NULL, outcome TEXT NOT NULL,
		token_id TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('manual', 'auto')),
		type TEXT NOT NULL CHECK(type IN ('buy', 'settle')),
		amount REAL NOT NULL, price REAL NOT NULL, shares REAL NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('filled', 'rejected', 'settled')),
		reason TEXT NOT NULL, external_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
	);`
	if _, err := db.ExecContext(ctx, oldSchema); err != nil {
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
	var paper engine.Account
	for _, account := range accounts {
		if account.Mode == engine.AccountModePaper {
			paper = account
		}
	}
	if paper.ID == 0 {
		t.Fatal("paper account was not seeded")
	}
	now := time.Now().UTC()
	position, err := repository.CreatePosition(ctx, engine.Position{
		AccountID: paper.ID, MarketID: "m1", ConditionID: "c1", Question: "split", Outcome: "Yes",
		TokenID: "t1", Side: "BUY", Kind: "manual", EntryPrice: 0.98, CurrentPrice: 0.5,
		Shares: 100, Cost: 100, Fee: 0, Status: "split", OpenedAt: now, EndDate: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("split status was not accepted after migration: %v", err)
	}
	order, err := repository.CreateOrder(ctx, engine.Order{
		AccountID: paper.ID, PositionID: &position.ID, MarketID: "m1", Question: "split",
		Outcome: "Yes", TokenID: "t1", Kind: "manual", Type: "buy", Amount: 100,
		Price: 0.98, Shares: 100, Status: "pending", Reason: "pending", ExternalID: "order-1", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("pending order was not accepted after migration: %v", err)
	}
	if order.ID == 0 {
		t.Fatal("pending order was not inserted")
	}
}

func TestLegacySettingsPayloadUsesDefaultsForMissingFields(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	legacy := `{"min_price":0.96,"max_price":0.99,"max_days":30,"min_liquidity":10000,` +
		`"order_size":100,"max_position":250,"max_exposure":1000,"scan_interval_seconds":60,` +
		`"slippage_bps":20,"fee_bps":0}`
	if _, err := repository.db.ExecContext(ctx, `INSERT INTO settings(id, payload, updated_at) VALUES(1, ?, ?)`,
		legacy, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	settings, err := repository.LoadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.InitialBankroll != engine.DefaultSettings().InitialBankroll {
		t.Fatalf("missing initial_bankroll should use the default, got %v", settings.InitialBankroll)
	}
	if settings.MinPrice != 0.96 {
		t.Fatalf("existing values must be preserved, got %v", settings.MinPrice)
	}
}
