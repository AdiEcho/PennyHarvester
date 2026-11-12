package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"

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
	// event_id groups the markets of one settlement event so automatic entries
	// can be capped per event. Rows written before the column existed keep ""
	// and are capped per market only.
	if err := s.ensureColumn(ctx, "positions", "event_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
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
	const indexes = `
	CREATE INDEX IF NOT EXISTS idx_positions_account_status ON positions(account_id, status);
	CREATE INDEX IF NOT EXISTS idx_positions_account_token ON positions(account_id, token_id, status);
	CREATE INDEX IF NOT EXISTS idx_positions_venue_status ON positions(venue, status);
	CREATE INDEX IF NOT EXISTS idx_orders_account_created ON orders(account_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_orders_venue_created ON orders(venue, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_snapshots_account_id ON equity_snapshots(account_id, id DESC);`
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
	INSERT INTO positions_new(id, account_id, market_id, condition_id, question, outcome, token_id, side, kind, exit_kind,
		entry_price, current_price, shares, cost, fee, status, opened_at, closed_at, end_date, realized_pnl, unrealized_pnl)
	SELECT id, account_id, market_id, condition_id, question, outcome, token_id, side, COALESCE(kind, 'manual'), COALESCE(exit_kind, ''),
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
	INSERT INTO orders_new(id, account_id, position_id, market_id, question, outcome, token_id, kind, type, action,
		amount, price, shares, fee, status, reason, external_id, created_at)
	SELECT id, account_id, position_id, market_id, question, outcome, token_id, kind, type, COALESCE(action, ''),
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

func (s *SQLite) LoadSettings(ctx context.Context) (engine.Settings, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM settings WHERE id = 1").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		settings := engine.DefaultSettings()
		return settings, s.SaveSettings(ctx, settings)
	}
	if err != nil {
		return engine.Settings{}, err
	}
	// Start from defaults so a payload written by an older version that lacks
	// a newly added field remains readable instead of failing validation.
	settings := engine.DefaultSettings()
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return engine.Settings{}, fmt.Errorf("读取策略设置: %w", err)
	}
	if err := settings.Validate(); err != nil {
		return engine.Settings{}, fmt.Errorf("数据库中的策略设置无效: %w", err)
	}
	// A payload without fee_model_version was written before paper accounts
	// charged a fee. Its fee_bps of 0 is the old default, not a deliberate
	// choice, so the current default is applied once.
	var stored struct {
		FeeModelVersion int `json:"fee_model_version"`
	}
	if err := json.Unmarshal([]byte(payload), &stored); err != nil {
		return engine.Settings{}, fmt.Errorf("读取策略设置: %w", err)
	}
	if stored.FeeModelVersion >= engine.FeeModelVersion {
		return settings, nil
	}
	if settings.FeeBPS == 0 {
		settings.FeeBPS = engine.DefaultSettings().FeeBPS
	}
	settings.FeeModelVersion = engine.FeeModelVersion
	if err := s.SaveSettings(ctx, settings); err != nil {
		return engine.Settings{}, err
	}
	return settings, nil
}

func (s *SQLite) SaveSettings(ctx context.Context, settings engine.Settings) error {
	payload, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(id, payload, updated_at) VALUES(1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		string(payload), formatTime(time.Now().UTC()))
	return err
}

// LoadAccountSettings reads one account's strategy block. Accounts that never
// saved their own settings inherit the global defaults; the fallback is a live
// read so a fresh installation keeps working without a migration copying rows
// into every account.
func (s *SQLite) LoadAccountSettings(ctx context.Context, accountID int64) (engine.Settings, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM account_settings WHERE account_id = ?", accountID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return s.LoadSettings(ctx)
	}
	if err != nil {
		return engine.Settings{}, err
	}
	// Start from defaults so a payload written by an older version that lacks
	// a newly added field stays readable.
	settings := engine.DefaultSettings()
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return engine.Settings{}, fmt.Errorf("读取账户策略设置: %w", err)
	}
	if err := settings.Validate(); err != nil {
		return engine.Settings{}, fmt.Errorf("账户 %d 的策略设置无效: %w", accountID, err)
	}
	return settings, nil
}

func (s *SQLite) SaveAccountSettings(ctx context.Context, accountID int64, settings engine.Settings) error {
	payload, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO account_settings(account_id, payload, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		accountID, string(payload), formatTime(time.Now().UTC()))
	return err
}

func (s *SQLite) LoadSystemSettings(ctx context.Context) (engine.SystemSettings, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM system_settings WHERE id = 1").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		settings := engine.DefaultSystemSettings()
		return settings, s.SaveSystemSettings(ctx, settings)
	}
	if err != nil {
		return engine.SystemSettings{}, err
	}
	// Start from defaults so a payload written before a field existed stays
	// readable instead of failing validation.
	settings := engine.DefaultSystemSettings()
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	legacyPlaintext := false
	decrypt := func(value string) (string, error) {
		plain, legacy, err := s.decryptSetting(value)
		if err != nil {
			return "", err
		}
		if legacy {
			legacyPlaintext = true
		}
		return plain, nil
	}
	if settings.Proxy.URL, err = decrypt(settings.Proxy.URL); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	if settings.Live.PrivateKey, err = decrypt(settings.Live.PrivateKey); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	if settings.Live.APISecret, err = decrypt(settings.Live.APISecret); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	if settings.Live.APIPassphrase, err = decrypt(settings.Live.APIPassphrase); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("读取系统设置: %w", err)
	}
	for v, entry := range settings.Venues {
		if entry.Live.PrivateKey, err = decrypt(entry.Live.PrivateKey); err != nil {
			return engine.SystemSettings{}, fmt.Errorf("读取 %s 系统设置: %w", v.Label(), err)
		}
		if entry.Live.APISecret, err = decrypt(entry.Live.APISecret); err != nil {
			return engine.SystemSettings{}, fmt.Errorf("读取 %s 系统设置: %w", v.Label(), err)
		}
		if entry.Live.APIPassphrase, err = decrypt(entry.Live.APIPassphrase); err != nil {
			return engine.SystemSettings{}, fmt.Errorf("读取 %s 系统设置: %w", v.Label(), err)
		}
		settings.Venues[v] = entry
	}
	settings = settings.Normalize()
	if err := settings.Validate(); err != nil {
		return engine.SystemSettings{}, fmt.Errorf("数据库中的系统设置无效: %w", err)
	}
	if legacyPlaintext {
		// Upgrade rows written before secrets were encrypted at rest.
		if err := s.SaveSystemSettings(ctx, settings); err != nil {
			return engine.SystemSettings{}, err
		}
	}
	return settings, nil
}

func (s *SQLite) decryptSetting(value string) (string, bool, error) {
	if s.secrets == nil {
		return value, false, nil
	}
	plain, encrypted, err := s.secrets.Decrypt(value)
	if err != nil {
		if s.secrets.keyPath != "" {
			return "", true, fmt.Errorf("%w（请确认密钥文件 %s 与数据库一同保留；如确认要丢弃已保存的网页凭据，可删除 system_settings 表中的该行后重启）", err, s.secrets.keyPath)
		}
		return "", true, err
	}
	return plain, value != "" && !encrypted, nil
}

func (s *SQLite) SaveSystemSettings(ctx context.Context, settings engine.SystemSettings) error {
	stored := settings
	if s.secrets != nil {
		var err error
		if stored.Proxy.URL, err = s.secrets.Encrypt(stored.Proxy.URL); err != nil {
			return fmt.Errorf("加密代理设置: %w", err)
		}
		if stored.Live.PrivateKey, err = s.secrets.Encrypt(stored.Live.PrivateKey); err != nil {
			return fmt.Errorf("加密实盘私钥: %w", err)
		}
		if stored.Live.APISecret, err = s.secrets.Encrypt(stored.Live.APISecret); err != nil {
			return fmt.Errorf("加密 L2 API Secret: %w", err)
		}
		if stored.Live.APIPassphrase, err = s.secrets.Encrypt(stored.Live.APIPassphrase); err != nil {
			return fmt.Errorf("加密 L2 API Passphrase: %w", err)
		}
		if stored.Venues != nil {
			venues := make(map[market.Venue]engine.VenueSystemSettings, len(stored.Venues))
			for v, entry := range stored.Venues {
				if entry.Live.PrivateKey, err = s.secrets.Encrypt(entry.Live.PrivateKey); err != nil {
					return fmt.Errorf("加密 %s 实盘私钥: %w", v.Label(), err)
				}
				if entry.Live.APISecret, err = s.secrets.Encrypt(entry.Live.APISecret); err != nil {
					return fmt.Errorf("加密 %s L2 API Secret: %w", v.Label(), err)
				}
				if entry.Live.APIPassphrase, err = s.secrets.Encrypt(entry.Live.APIPassphrase); err != nil {
					return fmt.Errorf("加密 %s L2 API Passphrase: %w", v.Label(), err)
				}
				venues[v] = entry
			}
			stored.Venues = venues
		}
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO system_settings(id, payload, updated_at) VALUES(1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		string(payload), formatTime(time.Now().UTC()))
	return err
}

// LoadAccessHash returns the bcrypt hash of the web access password, or an
// empty string when password protection is disabled.
func (s *SQLite) LoadAccessHash(ctx context.Context) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, "SELECT password_hash FROM access_password WHERE id = 1").Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return hash, nil
}

// SaveAccessHash stores a new hash, or removes the row when hash is empty so
// password protection can be turned off again.
func (s *SQLite) SaveAccessHash(ctx context.Context, hash string) error {
	if hash == "" {
		_, err := s.db.ExecContext(ctx, "DELETE FROM access_password WHERE id = 1")
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO access_password(id, password_hash, updated_at) VALUES(1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET password_hash=excluded.password_hash, updated_at=excluded.updated_at`,
		hash, formatTime(time.Now().UTC()))
	return err
}

func (s *SQLite) ListAccounts(ctx context.Context) ([]engine.Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, mode, venue, is_default, auto_trade, baseline_equity, created_at
		FROM accounts ORDER BY is_default DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []engine.Account{}
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (s *SQLite) AccountByID(ctx context.Context, id int64) (engine.Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, mode, venue, is_default, auto_trade, baseline_equity, created_at
		FROM accounts WHERE id = ?`, id)
	account, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return engine.Account{}, engine.ErrAccountNotFound
	}
	return account, err
}

func (s *SQLite) DefaultAccount(ctx context.Context) (engine.Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, mode, venue, is_default, auto_trade, baseline_equity, created_at
		FROM accounts ORDER BY is_default DESC, id ASC LIMIT 1`)
	account, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return engine.Account{}, engine.ErrAccountNotFound
	}
	return account, err
}

func (s *SQLite) CreateAccount(ctx context.Context, account engine.Account) (engine.Account, error) {
	venue := account.Venue
	if venue == "" {
		venue = market.VenuePolymarket
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO accounts(name, mode, venue, is_default, auto_trade, baseline_equity, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, account.Name, account.Mode, venue, account.IsDefault, account.AutoTrade,
		account.BaselineEquity, formatTime(account.CreatedAt))
	if err != nil {
		return engine.Account{}, err
	}
	account.Venue = venue
	account.Currency = account.EffectiveCurrency()
	account.ID, err = result.LastInsertId()
	return account, err
}

func (s *SQLite) UpdateAccount(ctx context.Context, account engine.Account) error {
	// baseline_equity is updated separately by UpdateAccountBaseline so a
	// concurrent live-balance sync cannot be overwritten by a name/auto-trade
	// edit.
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET name = ?, auto_trade = ? WHERE id = ?`,
		account.Name, account.AutoTrade, account.ID)
	return err
}

func (s *SQLite) SetDefaultAccount(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE accounts SET is_default = 0 WHERE is_default = 1")
	if err != nil {
		return err
	}
	_ = result
	if _, err := tx.ExecContext(ctx, "UPDATE accounts SET is_default = 1 WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) DeleteAccount(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"DELETE FROM orders WHERE account_id = ?",
		"DELETE FROM positions WHERE account_id = ?",
		"DELETE FROM equity_snapshots WHERE account_id = ?",
		"DELETE FROM account_settings WHERE account_id = ?",
		"DELETE FROM accounts WHERE id = ?",
	} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLite) UpdateAccountBaseline(ctx context.Context, id int64, value float64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE accounts SET baseline_equity = ? WHERE id = ?", value, id)
	return err
}

type accountScanner interface {
	Scan(dest ...any) error
}

func scanAccount(scanner accountScanner) (engine.Account, error) {
	var account engine.Account
	var mode, createdAt string
	var isDefault, autoTrade int
	if err := scanner.Scan(&account.ID, &account.Name, &mode, &account.Venue, &isDefault, &autoTrade,
		&account.BaselineEquity, &createdAt); err != nil {
		return engine.Account{}, err
	}
	account.Mode = engine.AccountMode(mode)
	account.Currency = account.EffectiveCurrency()
	account.IsDefault = isDefault == 1
	account.AutoTrade = autoTrade == 1
	parsed, err := parseTime(createdAt)
	if err != nil {
		return engine.Account{}, err
	}
	account.CreatedAt = parsed
	return account, nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertPosition(ctx context.Context, db execer, p engine.Position) (engine.Position, error) {
	kind := p.Kind
	if kind == "" {
		kind = "manual"
	}
	venue := p.EffectiveVenue()
	currency := p.EffectiveCurrency()
	result, err := db.ExecContext(ctx, `INSERT INTO positions(
		account_id, venue, currency, market_id, event_id, condition_id, question, outcome, token_id, side, kind, exit_kind, entry_price,
		current_price, shares, cost, fee, status, opened_at, closed_at, end_date,
		realized_pnl, unrealized_pnl
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.AccountID, venue, currency, p.MarketID, p.EventID, p.ConditionID, p.Question, p.Outcome, p.TokenID, p.Side, kind, p.ExitKind, p.EntryPrice,
		p.CurrentPrice, p.Shares, p.Cost, p.Fee, p.Status, formatTime(p.OpenedAt),
		nullableTime(p.ClosedAt), formatTime(p.EndDate), p.RealizedPNL, p.UnrealizedPNL)
	if err != nil {
		return engine.Position{}, err
	}
	p.Kind = kind
	p.Venue = venue
	p.Currency = currency
	p.ID, err = result.LastInsertId()
	return p, err
}

func (s *SQLite) CreatePosition(ctx context.Context, p engine.Position) (engine.Position, error) {
	return insertPosition(ctx, s.db, p)
}

func (s *SQLite) CreatePositionWithOrder(ctx context.Context, p engine.Position, order engine.Order) (engine.Position, engine.Order, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return engine.Position{}, engine.Order{}, err
	}
	defer tx.Rollback()
	p, err = insertPosition(ctx, tx, p)
	if err != nil {
		return engine.Position{}, engine.Order{}, err
	}
	order.PositionID = &p.ID
	order.ID, err = insertOrder(ctx, tx, order)
	if err != nil {
		return engine.Position{}, engine.Order{}, err
	}
	if err := tx.Commit(); err != nil {
		return engine.Position{}, engine.Order{}, err
	}
	return p, order, nil
}

func (s *SQLite) CreateOrder(ctx context.Context, order engine.Order) (engine.Order, error) {
	id, err := insertOrder(ctx, s.db, order)
	order.ID = id
	return order, err
}

type orderInserter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertOrder(ctx context.Context, db orderInserter, order engine.Order) (int64, error) {
	result, err := db.ExecContext(ctx, `INSERT INTO orders(
		account_id, venue, currency, position_id, market_id, question, outcome, token_id, kind, type, action,
		amount, price, shares, fee, status, reason, external_id, created_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		order.AccountID, order.EffectiveVenue(), order.EffectiveCurrency(), order.PositionID, order.MarketID,
		order.Question, order.Outcome, order.TokenID, order.Kind, order.Type, order.Action, order.Amount,
		order.Price, order.Shares, order.Fee, order.Status, order.Reason, order.ExternalID, formatTime(order.CreatedAt))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *SQLite) ListPositions(ctx context.Context, accountID int64, status string, v market.Venue) ([]engine.Position, error) {
	query := `SELECT id, account_id, venue, currency, market_id, event_id, condition_id, question, outcome, token_id, side, kind, exit_kind,
		entry_price, current_price, shares, cost, fee, status, opened_at, closed_at,
		end_date, realized_pnl, unrealized_pnl FROM positions WHERE account_id = ?`
	args := []any{accountID}
	switch status {
	case "", "all":
		// No extra filter.
	case "sold":
		// "sold" is the UI/API name for a stop-loss exit. It is stored as the
		// normal lost status plus exit_kind='stop' so existing CHECK
		// constraints never have to change.
		query += " AND status <> 'open' AND exit_kind = 'stop'"
	case "lost":
		// Keep stopped positions out of the ordinary loss tab; they have their
		// own tab and are counted as stop-loss exits in the metrics.
		query += " AND status = 'lost' AND exit_kind <> 'stop'"
	default:
		query += " AND status = ?"
		args = append(args, status)
	}
	if v != "" {
		query += " AND venue = ?"
		args = append(args, v)
	}
	query += " ORDER BY opened_at DESC, id DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	positions := []engine.Position{}
	for rows.Next() {
		var p engine.Position
		var openedAt, endDate string
		var closedAt sql.NullString
		if err := rows.Scan(&p.ID, &p.AccountID, &p.Venue, &p.Currency, &p.MarketID, &p.EventID, &p.ConditionID, &p.Question, &p.Outcome,
			&p.TokenID, &p.Side, &p.Kind, &p.ExitKind, &p.EntryPrice, &p.CurrentPrice, &p.Shares, &p.Cost,
			&p.Fee, &p.Status, &openedAt, &closedAt, &endDate, &p.RealizedPNL, &p.UnrealizedPNL); err != nil {
			return nil, err
		}
		p.OpenedAt, err = parseTime(openedAt)
		if err != nil {
			return nil, err
		}
		p.EndDate, err = parseTime(endDate)
		if err != nil {
			return nil, err
		}
		if closedAt.Valid {
			parsed, err := parseTime(closedAt.String)
			if err != nil {
				return nil, err
			}
			p.ClosedAt = &parsed
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

func orderStatusClause(status string) (string, []any) {
	switch status {
	case "", "all":
		return "", nil
	case "unresolved":
		return " AND status IN ('pending', 'unknown')", nil
	default:
		return " AND status = ?", []any{status}
	}
}

const orderColumns = `SELECT id, account_id, venue, currency, position_id, market_id, question, outcome,
	token_id, kind, type, action, amount, price, shares, fee, status, reason, external_id, created_at
	FROM orders`

func scanOrderRows(rows *sql.Rows) ([]engine.Order, error) {
	orders := []engine.Order{}
	for rows.Next() {
		var order engine.Order
		var positionID sql.NullInt64
		var createdAt string
		if err := rows.Scan(&order.ID, &order.AccountID, &order.Venue, &order.Currency, &positionID, &order.MarketID,
			&order.Question, &order.Outcome, &order.TokenID, &order.Kind, &order.Type, &order.Action, &order.Amount,
			&order.Price, &order.Shares, &order.Fee, &order.Status, &order.Reason, &order.ExternalID, &createdAt); err != nil {
			return nil, err
		}
		if positionID.Valid {
			order.PositionID = &positionID.Int64
		}
		parsed, err := parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		order.CreatedAt = parsed
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (s *SQLite) ListOrders(ctx context.Context, accountID int64, status string, limit, offset int, v market.Venue) ([]engine.Order, error) {
	query := orderColumns + " WHERE account_id = ?"
	args := []any{accountID}
	clause, filterArgs := orderStatusClause(status)
	query += clause
	args = append(args, filterArgs...)
	if v != "" {
		query += " AND venue = ?"
		args = append(args, v)
	}
	query += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrderRows(rows)
}

func (s *SQLite) CountOrders(ctx context.Context, accountID int64, status string, v market.Venue) (int64, error) {
	query := "SELECT COUNT(*) FROM orders WHERE account_id = ?"
	args := []any{accountID}
	clause, filterArgs := orderStatusClause(status)
	query += clause
	args = append(args, filterArgs...)
	if v != "" {
		query += " AND venue = ?"
		args = append(args, v)
	}
	var count int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

// EarliestAutoBuyAt returns the oldest automatic buy by creation time, not by
// insertion order, so a backfilled history row still marks the real start of
// the strategy. Ties inside one second fall back to id: the stored RFC3339Nano
// text has a variable number of fraction digits, and a sub-second difference
// cannot move an annualized figure.
func (s *SQLite) EarliestAutoBuyAt(ctx context.Context, accountID int64) (*time.Time, error) {
	var createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT created_at FROM orders
		WHERE account_id = ? AND kind = 'auto' AND type = 'buy'
		ORDER BY created_at ASC, id ASC LIMIT 1`, accountID).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (s *SQLite) ListUnresolvedOrders(ctx context.Context, accountID int64) ([]engine.Order, error) {
	rows, err := s.db.QueryContext(ctx, orderColumns+" WHERE account_id = ? AND status IN ('pending', 'unknown') AND external_id <> '' ORDER BY id ASC", accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrderRows(rows)
}

func (s *SQLite) MarkOrderStatus(ctx context.Context, orderID int64, status, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE orders SET status = ?, reason = ? WHERE id = ? AND status IN ('pending', 'unknown')`, status, reason, orderID)
	return err
}

// liveFillReason names the venue that produced the fill. A hardcoded
// "Polymarket" label would mislabel every Predict.fun order in the order list.
func liveFillReason(p engine.Position, kind string) string {
	return p.EffectiveVenue().Label() + " " + kind
}

func (s *SQLite) FinalizeLiveBuy(ctx context.Context, orderID int64, p engine.Position, fill engine.LiveFill) (engine.Position, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return engine.Position{}, err
	}
	defer tx.Rollback()
	p, err = insertPosition(ctx, tx, p)
	if err != nil {
		return engine.Position{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE orders SET position_id = ?, amount = ?, price = ?, shares = ?, fee = ?,
		status = 'filled', reason = ?, external_id = CASE WHEN external_id = '' THEN ? ELSE external_id END
		WHERE id = ? AND status IN ('pending', 'unknown')`,
		p.ID, fill.Cost, fill.Price, fill.Shares, fill.Fee, liveFillReason(p, "实盘成交"), fill.OrderID, orderID)
	if err != nil {
		return engine.Position{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return engine.Position{}, fmt.Errorf("实盘订单 %d 已处理或不存在", orderID)
	}
	if err := tx.Commit(); err != nil {
		return engine.Position{}, err
	}
	return p, nil
}

// applySellUpdate persists the position fields a sell changes. It is shared by
// paper and live sells so both paths keep shares, cost basis, cumulative fee and
// realized PnL consistent, including a partial fill that leaves the position
// open with the remaining shares.
func applySellUpdate(ctx context.Context, tx *sql.Tx, p engine.Position) error {
	result, err := tx.ExecContext(ctx, `UPDATE positions SET current_price=?, shares=?, cost=?, fee=?,
		status=?, exit_kind=?, closed_at=?, realized_pnl=?, unrealized_pnl=? WHERE id=? AND status='open'`,
		p.CurrentPrice, p.Shares, p.Cost, p.Fee, p.Status, p.ExitKind, nullableTime(p.ClosedAt),
		p.RealizedPNL, p.UnrealizedPNL, p.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return fmt.Errorf("持仓 %d 已平仓或不存在", p.ID)
	}
	return nil
}

// SellPosition books a paper sell (full or partial) and its order in one
// transaction.
func (s *SQLite) SellPosition(ctx context.Context, p engine.Position, order engine.Order) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applySellUpdate(ctx, tx, p); err != nil {
		return err
	}
	if _, err := insertOrder(ctx, tx, order); err != nil {
		return err
	}
	return tx.Commit()
}

// FinalizeLiveSell marks a signed sell order as filled and applies its fill to
// the position. A partial fill leaves the position open with the remaining
// shares; a full fill closes it with exit_kind='stop'.
func (s *SQLite) FinalizeLiveSell(ctx context.Context, orderID int64, p engine.Position, fill engine.LiveFill) (engine.Position, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return engine.Position{}, err
	}
	defer tx.Rollback()
	if err := applySellUpdate(ctx, tx, p); err != nil {
		return engine.Position{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE orders SET position_id = ?, amount = ?, price = ?, shares = ?, fee = ?,
		status = 'filled', reason = ?, external_id = CASE WHEN external_id = '' THEN ? ELSE external_id END
		WHERE id = ? AND status IN ('pending', 'unknown')`,
		p.ID, fill.Cost, fill.Price, fill.Shares, fill.Fee, liveFillReason(p, "止损成交"), fill.OrderID, orderID)
	if err != nil {
		return engine.Position{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return engine.Position{}, fmt.Errorf("实盘订单 %d 已处理或不存在", orderID)
	}
	if err := tx.Commit(); err != nil {
		return engine.Position{}, err
	}
	return p, nil
}

func (s *SQLite) UpdateMark(ctx context.Context, id int64, price, unrealized float64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE positions SET current_price=?, unrealized_pnl=? WHERE id=? AND status='open'", price, unrealized, id)
	return err
}

func (s *SQLite) SettlePosition(ctx context.Context, p engine.Position, order engine.Order) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE positions SET status=?, current_price=?, closed_at=?,
		realized_pnl=?, unrealized_pnl=0 WHERE id=? AND status='open'`,
		p.Status, p.CurrentPrice, nullableTime(p.ClosedAt), p.RealizedPNL, p.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return fmt.Errorf("持仓 %d 未结算，可能已被处理", p.ID)
	}
	if _, err := insertOrder(ctx, tx, order); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) AddSnapshot(ctx context.Context, accountID int64, point engine.EquityPoint) error {
	venue := point.Venue
	if venue == "" {
		venue = market.VenuePolymarket
	}
	currency := point.Currency
	if currency == "" {
		currency = market.CurrencyUSDC
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO equity_snapshots(account_id, venue, currency, time, value) VALUES(?, ?, ?, ?, ?)",
		accountID, venue, currency, formatTime(point.Time), point.Value); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM equity_snapshots WHERE account_id = ? AND id NOT IN
		(SELECT id FROM equity_snapshots WHERE account_id = ? ORDER BY id DESC LIMIT 5000)`, accountID, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) ListSnapshots(ctx context.Context, accountID int64, limit int) ([]engine.EquityPoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT venue, currency, time, value FROM (
		SELECT id, venue, currency, time, value FROM equity_snapshots WHERE account_id = ? ORDER BY id DESC LIMIT ?
	) ORDER BY id ASC`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []engine.EquityPoint{}
	for rows.Next() {
		var point engine.EquityPoint
		var timestamp string
		if err := rows.Scan(&point.Venue, &point.Currency, &timestamp, &point.Value); err != nil {
			return nil, err
		}
		point.Time, err = parseTime(timestamp)
		if err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
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
