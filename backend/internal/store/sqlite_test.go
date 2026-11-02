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
		Outcome: "Yes", TokenID: "t1", Kind: "manual", Type: "buy", Amount: 100.15,
		Price: 0.98, Shares: 102.19, Fee: 0.15, Status: "pending", Reason: "pending", ExternalID: "order-1", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("pending order was not accepted after migration: %v", err)
	}
	if order.ID == 0 {
		t.Fatal("pending order was not inserted")
	}
	stopped, err := repository.CreatePosition(ctx, engine.Position{
		AccountID: paper.ID, MarketID: "m2", ConditionID: "c2", Question: "stopped", Outcome: "Yes",
		TokenID: "t2", Side: "BUY", Kind: "auto", ExitKind: "stop", EntryPrice: 0.98, CurrentPrice: 0.75,
		Shares: 100, Cost: 98, Fee: 0.1, Status: "lost", OpenedAt: now, ClosedAt: &now,
		EndDate: now.Add(time.Hour), RealizedPNL: -24,
	})
	if err != nil {
		t.Fatalf("stopped position was not accepted after migration: %v", err)
	}
	if _, err := repository.CreateOrder(ctx, engine.Order{
		AccountID: paper.ID, PositionID: &stopped.ID, MarketID: "m2", Question: "stopped",
		Outcome: "Yes", TokenID: "t2", Kind: "auto", Type: "settle", Action: "stop", Amount: 74,
		Price: 0.75, Shares: 100, Fee: 0.1, Status: "filled", Reason: "stop", CreatedAt: now,
	}); err != nil {
		t.Fatalf("stop-loss order was not accepted after migration: %v", err)
	}
	orders, err := repository.ListOrders(ctx, paper.ID, "all", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 {
		t.Fatalf("expected a buy and a sell order after migration: %+v", orders)
	}
}

func TestMigrationKeepsNewerSchemaAndTriggers(t *testing.T) {
	// A database written by a newer build can carry extra columns and triggers
	// that reference positions/orders. The stop-loss bookkeeping must be added
	// with ALTER TABLE ADD COLUMN, without rebuilding those tables (which would
	// drop the extra columns and make the triggers fail).
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const newerSchema = `
	CREATE TABLE accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL,
		mode TEXT NOT NULL CHECK(mode IN ('live', 'paper')),
		is_default INTEGER NOT NULL DEFAULT 0, auto_trade INTEGER NOT NULL DEFAULT 0,
		baseline_equity REAL NOT NULL DEFAULT 0, created_at TEXT NOT NULL,
		platform TEXT NOT NULL DEFAULT 'polymarket', network TEXT NOT NULL DEFAULT 'mainnet',
		connection_id INTEGER REFERENCES connections(id), extra TEXT NOT NULL DEFAULT 'keep'
	);
	CREATE TABLE connections (
		id INTEGER PRIMARY KEY AUTOINCREMENT, platform TEXT NOT NULL, network TEXT NOT NULL
	);
	CREATE TABLE settings (id INTEGER PRIMARY KEY CHECK (id = 1), payload TEXT NOT NULL, updated_at TEXT NOT NULL);
	CREATE TABLE system_settings (id INTEGER PRIMARY KEY CHECK (id = 1), payload TEXT NOT NULL, updated_at TEXT NOT NULL);
	CREATE TABLE positions (
		id INTEGER PRIMARY KEY AUTOINCREMENT, account_id INTEGER NOT NULL REFERENCES accounts(id),
		market_id TEXT NOT NULL, condition_id TEXT NOT NULL, question TEXT NOT NULL,
		outcome TEXT NOT NULL, token_id TEXT NOT NULL, side TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'manual', entry_price REAL NOT NULL, current_price REAL NOT NULL,
		shares REAL NOT NULL, cost REAL NOT NULL, fee REAL NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('open', 'won', 'lost', 'split')),
		opened_at TEXT NOT NULL, closed_at TEXT, end_date TEXT NOT NULL,
		realized_pnl REAL NOT NULL DEFAULT 0, unrealized_pnl REAL NOT NULL DEFAULT 0,
		extra TEXT NOT NULL DEFAULT 'keep'
	);
	CREATE TABLE orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT, account_id INTEGER NOT NULL REFERENCES accounts(id),
		position_id INTEGER REFERENCES positions(id), market_id TEXT NOT NULL, question TEXT NOT NULL,
		outcome TEXT NOT NULL, token_id TEXT NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('manual', 'auto')),
		type TEXT NOT NULL CHECK(type IN ('buy', 'settle')),
		amount REAL NOT NULL, price REAL NOT NULL, shares REAL NOT NULL, fee REAL NOT NULL DEFAULT 0,
		status TEXT NOT NULL CHECK(status IN ('filled', 'rejected', 'settled', 'pending', 'unknown')),
		reason TEXT NOT NULL, external_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
		extra TEXT NOT NULL DEFAULT 'keep'
	);
	CREATE TABLE equity_snapshots (id INTEGER PRIMARY KEY AUTOINCREMENT, account_id INTEGER NOT NULL, time TEXT NOT NULL, value REAL NOT NULL);
	CREATE TRIGGER trg_accounts_connection_guarded
		BEFORE UPDATE OF connection_id ON accounts
		FOR EACH ROW WHEN NEW.connection_id IS NOT OLD.connection_id AND (
			EXISTS (SELECT 1 FROM positions WHERE account_id = OLD.id AND status = 'open')
			OR EXISTS (SELECT 1 FROM orders WHERE account_id = OLD.id AND status IN ('pending', 'unknown'))
		)
		BEGIN SELECT RAISE(ABORT, '存在未平仓持仓或未决订单时不能更换连接'); END;`
	if _, err := db.ExecContext(ctx, newerSchema); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migration must not rebuild a newer schema: %v", err)
	}
	defer repository.Close()
	var triggerCount int
	if err := repository.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='trg_accounts_connection_guarded'`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if triggerCount != 1 {
		t.Fatal("the newer schema trigger was dropped")
	}
	for _, table := range []string{"positions", "orders"} {
		rows, err := repository.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
		if err != nil {
			t.Fatal(err)
		}
		columns := map[string]bool{}
		for rows.Next() {
			var cid int
			var name, kind string
			var notNull, pk int
			var defaultValue any
			if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			columns[name] = true
		}
		rows.Close()
		if !columns["extra"] {
			t.Fatalf("%s lost its extra column: %v", table, columns)
		}
	}
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
	now := time.Now().UTC()
	position, err := repository.CreatePosition(ctx, engine.Position{
		AccountID: paper.ID, MarketID: "m1", ConditionID: "c1", Question: "stop", Outcome: "Yes",
		TokenID: "t1", Side: "BUY", Kind: "auto", ExitKind: "stop", EntryPrice: 0.98,
		CurrentPrice: 0.70, Shares: 10, Cost: 9.8, Fee: 0.01, Status: "lost",
		OpenedAt: now, ClosedAt: &now, EndDate: now.Add(time.Hour), RealizedPNL: -2,
	})
	if err != nil {
		t.Fatalf("stop-loss position was rejected by the newer schema: %v", err)
	}
	if _, err := repository.CreateOrder(ctx, engine.Order{
		AccountID: paper.ID, PositionID: &position.ID, MarketID: "m1", Question: "stop",
		Outcome: "Yes", TokenID: "t1", Kind: "auto", Type: "settle", Action: "stop",
		Amount: 7, Price: 0.70, Shares: 10, Fee: 0.01, Status: "filled", Reason: "stop", CreatedAt: now,
	}); err != nil {
		t.Fatalf("stop-loss order was rejected by the newer schema: %v", err)
	}
	sold, err := repository.ListPositions(ctx, paper.ID, "sold")
	if err != nil || len(sold) != 1 || sold[0].ExitKind != "stop" {
		t.Fatalf("sold filter did not read exit_kind: %+v err=%v", sold, err)
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
	// The old row predates paper fees: its zero fee_bps is the old default and
	// must be upgraded once, then left alone.
	if settings.FeeBPS != engine.DefaultSettings().FeeBPS {
		t.Fatalf("legacy zero fee should be upgraded, got %v", settings.FeeBPS)
	}
	if settings.FeeModelVersion != engine.FeeModelVersion {
		t.Fatalf("fee model version not stamped: %+v", settings)
	}
	defaults := engine.DefaultSettings()
	if settings.EntryWindowMinutes != defaults.EntryWindowMinutes || settings.SportsEntryWindowMinutes != defaults.SportsEntryWindowMinutes {
		t.Fatalf("missing entry windows should use the defaults: %+v", settings)
	}
	if settings.StopLossEnabled || settings.StopLossRatio != defaults.StopLossRatio {
		t.Fatalf("missing stop-loss fields should use the defaults: %+v", settings)
	}

	// A row written by the current build keeps an explicit zero fee.
	deliberate := settings
	deliberate.FeeBPS = 0
	if err := repository.SaveSettings(ctx, deliberate); err != nil {
		t.Fatal(err)
	}
	reloaded, err := repository.LoadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.FeeBPS != 0 {
		t.Fatalf("an explicit zero fee must survive a reload, got %v", reloaded.FeeBPS)
	}
}

func TestOrderFeeIsPersisted(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, filepath.Join(t.TempDir(), "orders.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	account, err := repository.DefaultAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	order, err := repository.CreateOrder(ctx, engine.Order{
		AccountID: account.ID, MarketID: "m", Question: "q", Outcome: "Yes", TokenID: "t",
		Kind: "manual", Type: "buy", Amount: 100.15, Price: 0.98, Shares: 102.19,
		Fee: 0.15, Status: "filled", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	orders, err := repository.ListOrders(ctx, account.ID, "all", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 {
		t.Fatalf("unexpected orders: %+v", orders)
	}
	if orders[0].ID != order.ID || orders[0].Fee != 0.15 || orders[0].Amount != 100.15 {
		t.Fatalf("order fee was not persisted: %+v", orders[0])
	}
}
