package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SQLite struct {
	db      *sql.DB
	secrets *secretBox
}

func Open(ctx context.Context, path string) (*SQLite, error) {
	secrets, err := newSecretBox(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化 SQLite: %w", err)
	}
	// WAL frames are reused instead of shrunk by default, so a burst of writes
	// (a backfill or a long catch-up) leaves a file far larger than the database
	// itself: the scan writes a mark for every open position each cycle. Capping
	// the retained WAL keeps ./data honest after the burst is checkpointed.
	if _, err := db.ExecContext(ctx, "PRAGMA journal_size_limit=16777216"); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化 SQLite: %w", err)
	}
	store := &SQLite{db: db, secrets: secrets}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" {
		// Best effort: the database holds wallet credentials, so keep it
		// private even when the process umask is permissive.
		_ = os.Chmod(path, 0o600)
	}
	return store, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) migrate(ctx context.Context) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		mode TEXT NOT NULL CHECK(mode IN ('live', 'paper')),
		venue TEXT NOT NULL DEFAULT 'polymarket',
		is_default INTEGER NOT NULL DEFAULT 0,
		auto_trade INTEGER NOT NULL DEFAULT 0,
		baseline_equity REAL NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_default ON accounts(is_default) WHERE is_default = 1;
	CREATE TABLE IF NOT EXISTS settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		payload TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS system_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		payload TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS account_settings (
		account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
		payload TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS access_password (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		password_hash TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS positions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		venue TEXT NOT NULL DEFAULT 'polymarket',
		currency TEXT NOT NULL DEFAULT 'USDC',
		market_id TEXT NOT NULL,
		event_id TEXT NOT NULL DEFAULT '',
		condition_id TEXT NOT NULL,
		question TEXT NOT NULL,
		outcome TEXT NOT NULL,
		token_id TEXT NOT NULL,
		side TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'manual',
		exit_kind TEXT NOT NULL DEFAULT '',
		entry_price REAL NOT NULL,
		current_price REAL NOT NULL,
		shares REAL NOT NULL,
		cost REAL NOT NULL,
		fee REAL NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('open', 'won', 'lost', 'split')),
		opened_at TEXT NOT NULL,
		closed_at TEXT,
		end_date TEXT NOT NULL,
		realized_pnl REAL NOT NULL DEFAULT 0,
		unrealized_pnl REAL NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		venue TEXT NOT NULL DEFAULT 'polymarket',
		currency TEXT NOT NULL DEFAULT 'USDC',
		position_id INTEGER REFERENCES positions(id),
		market_id TEXT NOT NULL,
		event_id TEXT NOT NULL DEFAULT '',
		question TEXT NOT NULL,
		outcome TEXT NOT NULL,
		token_id TEXT NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('manual', 'auto')),
		type TEXT NOT NULL CHECK(type IN ('buy', 'settle')),
		action TEXT NOT NULL DEFAULT '',
		amount REAL NOT NULL,
		price REAL NOT NULL,
		shares REAL NOT NULL,
		fee REAL NOT NULL DEFAULT 0,
		status TEXT NOT NULL CHECK(status IN ('filled', 'rejected', 'settled', 'pending', 'unknown')),
		reason TEXT NOT NULL,
		external_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS equity_snapshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		venue TEXT NOT NULL DEFAULT 'polymarket',
		currency TEXT NOT NULL DEFAULT 'USDC',
		time TEXT NOT NULL,
		value REAL NOT NULL
	);
	CREATE TABLE IF NOT EXISTS chain_operations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL,
		venue TEXT NOT NULL,
		chain_id INTEGER NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('approve', 'redeem')),
		position_id INTEGER,
		market_id TEXT,
		token_id TEXT,
		from_address TEXT NOT NULL,
		nonce INTEGER,
		tx_hash TEXT,
		status TEXT NOT NULL CHECK(status IN ('intent', 'signed', 'broadcast', 'confirmed', 'reverted', 'unknown')),
		reason TEXT,
		resolved_by TEXT,
		resolution_note TEXT,
		resolved_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}

	liveID, paperID, err := s.seedAccounts(ctx)
	if err != nil {
		return err
	}
	_ = liveID

	// Upgrade databases created before accounts existed. Legacy rows were
	// paper positions, so they are attached to the paper account, never the
	// default live account.
	if err := s.ensureColumn(ctx, "positions", "account_id", fmt.Sprintf("INTEGER NOT NULL DEFAULT %d", paperID)); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "orders", "account_id", fmt.Sprintf("INTEGER NOT NULL DEFAULT %d", paperID)); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "orders", "external_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "positions", "kind", "TEXT NOT NULL DEFAULT 'manual'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "equity_snapshots", "account_id", fmt.Sprintf("INTEGER NOT NULL DEFAULT %d", paperID)); err != nil {
		return err
	}
	// New bookkeeping columns are additive: ALTER TABLE ADD COLUMN preserves
	// every existing column, index, trigger and foreign key. The CHECK
	// constraints on status/type stay untouched so an older or newer database
	// with a different schema is never rebuilt (and never loses columns).
	if err := s.ensureColumn(ctx, "positions", "exit_kind", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// event_id groups the markets of one settlement event. It must exist before
	// the status rebuilds so their explicit column lists can carry it over.
	if err := s.ensureColumn(ctx, "positions", "event_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.migratePositionStatus(ctx); err != nil {
		return err
	}
	// The fee and action columns must exist before the orders table is rebuilt,
	// so the rebuild can carry them over.
	if err := s.ensureColumn(ctx, "orders", "fee", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "orders", "action", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// event_id identifies the settlement event a buy order reserves. Existing
	// pending rows keep "" and only count against their market.
	if err := s.ensureColumn(ctx, "orders", "event_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.migrateOrderStatus(ctx); err != nil {
		return err
	}
	// Venue/currency columns are added only after the positions/orders rebuilds,
	// whose explicit column lists would otherwise drop them on a legacy upgrade.
	if err := s.ensureColumn(ctx, "accounts", "venue", "TEXT NOT NULL DEFAULT 'polymarket'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "positions", "venue", "TEXT NOT NULL DEFAULT 'polymarket'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "positions", "currency", "TEXT NOT NULL DEFAULT 'USDC'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "orders", "venue", "TEXT NOT NULL DEFAULT 'polymarket'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "orders", "currency", "TEXT NOT NULL DEFAULT 'USDC'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "equity_snapshots", "venue", "TEXT NOT NULL DEFAULT 'polymarket'"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "equity_snapshots", "currency", "TEXT NOT NULL DEFAULT 'USDC'"); err != nil {
		return err
	}
	// Manual resolution is additive. Old rows have no audit fields and remain
	// readable; only an operator resolving an unknown row fills them.
	if err := s.ensureColumn(ctx, "chain_operations", "resolved_by", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "chain_operations", "resolution_note", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "chain_operations", "resolved_at", "TEXT"); err != nil {
		return err
	}
	const indexes = `
	CREATE INDEX IF NOT EXISTS idx_positions_account_status ON positions(account_id, status);
	CREATE INDEX IF NOT EXISTS idx_positions_account_token ON positions(account_id, token_id, status);
	CREATE INDEX IF NOT EXISTS idx_positions_venue_status ON positions(venue, status);
	CREATE INDEX IF NOT EXISTS idx_orders_account_created ON orders(account_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_orders_account_event ON orders(account_id, event_id);
	CREATE INDEX IF NOT EXISTS idx_orders_venue_created ON orders(venue, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_orders_account_id ON orders(account_id, id DESC);
	CREATE INDEX IF NOT EXISTS idx_orders_account_status_id ON orders(account_id, status, id DESC);
	CREATE INDEX IF NOT EXISTS idx_orders_unresolved_account ON orders(account_id, id) WHERE status IN ('pending', 'unknown');
	CREATE INDEX IF NOT EXISTS idx_snapshots_account_id ON equity_snapshots(account_id, id DESC);
	CREATE INDEX IF NOT EXISTS idx_chain_operations_status_id ON chain_operations(status, id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_chain_operations_active_redeem ON chain_operations(account_id, position_id) WHERE kind = 'redeem' AND status IN ('intent', 'signed', 'broadcast', 'unknown');
	CREATE UNIQUE INDEX IF NOT EXISTS idx_chain_operations_active_approve ON chain_operations(account_id, venue) WHERE kind = 'approve' AND status IN ('intent', 'signed', 'broadcast', 'unknown');`
	if _, err := s.db.ExecContext(ctx, indexes); err != nil {
		return err
	}
	return nil
}

func (s *SQLite) tableSQL(ctx context.Context, name string) (string, error) {
	var statement string
	err := s.db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&statement)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return statement, err
}

// migratePositionStatus rebuilds the positions table when an older CHECK
// constraint does not allow the split status. SQLite cannot alter a CHECK in
// place, so the table has to be recreated. Foreign keys are disabled for the
// duration because orders references positions.
func (s *SQLite) migratePositionStatus(ctx context.Context) error {
	statement, err := s.tableSQL(ctx, "positions")
	if err != nil || strings.Contains(statement, "'split'") {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer func() { _, _ = s.db.ExecContext(context.Background(), "PRAGMA foreign_keys=ON") }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const rebuild = `
	DROP TABLE IF EXISTS positions_new;
	CREATE TABLE positions_new (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		market_id TEXT NOT NULL,
		event_id TEXT NOT NULL DEFAULT '',
		condition_id TEXT NOT NULL,
		question TEXT NOT NULL,
		outcome TEXT NOT NULL,
		token_id TEXT NOT NULL,
		side TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'manual',
		exit_kind TEXT NOT NULL DEFAULT '',
		entry_price REAL NOT NULL,
		current_price REAL NOT NULL,
		shares REAL NOT NULL,
		cost REAL NOT NULL,
		fee REAL NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('open', 'won', 'lost', 'split')),
		opened_at TEXT NOT NULL,
		closed_at TEXT,
		end_date TEXT NOT NULL,
		realized_pnl REAL NOT NULL DEFAULT 0,
		unrealized_pnl REAL NOT NULL DEFAULT 0
	);
	INSERT INTO positions_new(id, account_id, market_id, event_id, condition_id, question, outcome, token_id, side, kind, exit_kind,
		entry_price, current_price, shares, cost, fee, status, opened_at, closed_at, end_date, realized_pnl, unrealized_pnl)
	SELECT id, account_id, market_id, event_id, condition_id, question, outcome, token_id, side, COALESCE(kind, 'manual'), COALESCE(exit_kind, ''),
		entry_price, current_price, shares, cost, fee, status, opened_at, closed_at, end_date, realized_pnl, unrealized_pnl
	FROM positions;
	DROP TABLE positions;
	ALTER TABLE positions_new RENAME TO positions;`
	if _, err := tx.ExecContext(ctx, rebuild); err != nil {
		return fmt.Errorf("迁移 positions 状态约束: %w", err)
	}
	return tx.Commit()
}

// migrateOrderStatus adds pending/unknown to the orders status CHECK. Orders
// have no child tables, so this rebuild does not need foreign keys disabled.
func (s *SQLite) migrateOrderStatus(ctx context.Context) error {
	statement, err := s.tableSQL(ctx, "orders")
	if err != nil || (strings.Contains(statement, "'pending'") && strings.Contains(statement, "'unknown'")) {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const rebuild = `
	DROP TABLE IF EXISTS orders_new;
	CREATE TABLE orders_new (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		position_id INTEGER REFERENCES positions(id),
		market_id TEXT NOT NULL,
		event_id TEXT NOT NULL DEFAULT '',
		question TEXT NOT NULL,
		outcome TEXT NOT NULL,
		token_id TEXT NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('manual', 'auto')),
		type TEXT NOT NULL CHECK(type IN ('buy', 'settle')),
		action TEXT NOT NULL DEFAULT '',
		amount REAL NOT NULL,
		price REAL NOT NULL,
		shares REAL NOT NULL,
		fee REAL NOT NULL DEFAULT 0,
		status TEXT NOT NULL CHECK(status IN ('filled', 'rejected', 'settled', 'pending', 'unknown')),
		reason TEXT NOT NULL,
		external_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
	INSERT INTO orders_new(id, account_id, position_id, market_id, event_id, question, outcome, token_id, kind, type, action,
		amount, price, shares, fee, status, reason, external_id, created_at)
	SELECT id, account_id, position_id, market_id, event_id, question, outcome, token_id, kind, type, COALESCE(action, ''),
		amount, price, shares, fee, status, reason, external_id, created_at
	FROM orders;
	DROP TABLE orders;
	ALTER TABLE orders_new RENAME TO orders;`
	if _, err := tx.ExecContext(ctx, rebuild); err != nil {
		return fmt.Errorf("迁移 orders 状态约束: %w", err)
	}
	return tx.Commit()
}

func (s *SQLite) seedAccounts(ctx context.Context) (liveID, paperID int64, err error) {
	var count int64
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts").Scan(&count); err != nil {
		return 0, 0, err
	}
	if count == 0 {
		now := formatTime(time.Now().UTC())
		result, execErr := s.db.ExecContext(ctx, `INSERT INTO accounts(name, mode, is_default, auto_trade, baseline_equity, created_at)
			VALUES(?, 'live', 1, 0, 0, ?)`, "实盘账户", now)
		if execErr != nil {
			return 0, 0, execErr
		}
		if liveID, err = result.LastInsertId(); err != nil {
			return 0, 0, err
		}
		result, execErr = s.db.ExecContext(ctx, `INSERT INTO accounts(name, mode, is_default, auto_trade, baseline_equity, created_at)
			VALUES(?, 'paper', 0, 0, 0, ?)`, "模拟账户", now)
		if execErr != nil {
			return 0, 0, execErr
		}
		if paperID, err = result.LastInsertId(); err != nil {
			return 0, 0, err
		}
		return liveID, paperID, nil
	}

	var liveCount, paperCount, defaultCount int64
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE mode = 'live'").Scan(&liveCount); err != nil {
		return 0, 0, err
	}
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE mode = 'paper'").Scan(&paperCount); err != nil {
		return 0, 0, err
	}
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE is_default = 1").Scan(&defaultCount); err != nil {
		return 0, 0, err
	}
	if defaultCount == 0 {
		if _, err = s.db.ExecContext(ctx, "UPDATE accounts SET is_default = 1 WHERE id = (SELECT id FROM accounts ORDER BY id LIMIT 1)"); err != nil {
			return 0, 0, err
		}
	}
	if liveCount > 0 {
		_ = s.db.QueryRowContext(ctx, "SELECT id FROM accounts WHERE mode = 'live' ORDER BY id LIMIT 1").Scan(&liveID)
	}
	if paperCount > 0 {
		_ = s.db.QueryRowContext(ctx, "SELECT id FROM accounts WHERE mode = 'paper' ORDER BY id LIMIT 1").Scan(&paperID)
	}
	if paperID == 0 {
		result, execErr := s.db.ExecContext(ctx, `INSERT INTO accounts(name, mode, is_default, auto_trade, baseline_equity, created_at)
			VALUES(?, 'paper', 0, 0, 0, ?)`, "模拟账户", formatTime(time.Now().UTC()))
		if execErr != nil {
			return 0, 0, execErr
		}
		paperID, err = result.LastInsertId()
	}
	return liveID, paperID, err
}

func (s *SQLite) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("解析数据库时间 %q: %w", value, err)
	}
	return parsed, nil
}
