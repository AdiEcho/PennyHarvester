package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"pennyharvester/backend/internal/engine"

	_ "modernc.org/sqlite"
)

type SQLite struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化 SQLite: %w", err)
	}
	store := &SQLite{db: db}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
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
	CREATE TABLE IF NOT EXISTS positions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		market_id TEXT NOT NULL,
		condition_id TEXT NOT NULL,
		question TEXT NOT NULL,
		outcome TEXT NOT NULL,
		token_id TEXT NOT NULL,
		side TEXT NOT NULL,
		entry_price REAL NOT NULL,
		current_price REAL NOT NULL,
		shares REAL NOT NULL,
		cost REAL NOT NULL,
		fee REAL NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('open', 'won', 'lost')),
		opened_at TEXT NOT NULL,
		closed_at TEXT,
		end_date TEXT NOT NULL,
		realized_pnl REAL NOT NULL DEFAULT 0,
		unrealized_pnl REAL NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
		position_id INTEGER REFERENCES positions(id),
		market_id TEXT NOT NULL,
		question TEXT NOT NULL,
		outcome TEXT NOT NULL,
		token_id TEXT NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('manual', 'auto')),
		type TEXT NOT NULL CHECK(type IN ('buy', 'settle')),
		amount REAL NOT NULL,
		price REAL NOT NULL,
		shares REAL NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('filled', 'rejected', 'settled')),
		reason TEXT NOT NULL,
		external_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS equity_snapshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL REFERENCES accounts(id),
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
	if err := s.ensureColumn(ctx, "equity_snapshots", "account_id", fmt.Sprintf("INTEGER NOT NULL DEFAULT %d", paperID)); err != nil {
		return err
	}
	const indexes = `
	CREATE INDEX IF NOT EXISTS idx_positions_account_status ON positions(account_id, status);
	CREATE INDEX IF NOT EXISTS idx_positions_account_token ON positions(account_id, token_id, status);
	CREATE INDEX IF NOT EXISTS idx_orders_account_created ON orders(account_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_snapshots_account_id ON equity_snapshots(account_id, id DESC);`
	if _, err := s.db.ExecContext(ctx, indexes); err != nil {
		return err
	}
	return nil
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
	var settings engine.Settings
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return engine.Settings{}, fmt.Errorf("读取策略设置: %w", err)
	}
	if err := settings.Validate(); err != nil {
		return engine.Settings{}, fmt.Errorf("数据库中的策略设置无效: %w", err)
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

func (s *SQLite) ListAccounts(ctx context.Context) ([]engine.Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, mode, is_default, auto_trade, baseline_equity, created_at
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
	row := s.db.QueryRowContext(ctx, `SELECT id, name, mode, is_default, auto_trade, baseline_equity, created_at
		FROM accounts WHERE id = ?`, id)
	account, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return engine.Account{}, engine.ErrAccountNotFound
	}
	return account, err
}

func (s *SQLite) DefaultAccount(ctx context.Context) (engine.Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, mode, is_default, auto_trade, baseline_equity, created_at
		FROM accounts ORDER BY is_default DESC, id ASC LIMIT 1`)
	account, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return engine.Account{}, engine.ErrAccountNotFound
	}
	return account, err
}

func (s *SQLite) CreateAccount(ctx context.Context, account engine.Account) (engine.Account, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO accounts(name, mode, is_default, auto_trade, baseline_equity, created_at)
		VALUES(?, ?, ?, ?, ?, ?)`, account.Name, account.Mode, account.IsDefault, account.AutoTrade,
		account.BaselineEquity, formatTime(account.CreatedAt))
	if err != nil {
		return engine.Account{}, err
	}
	account.ID, err = result.LastInsertId()
	return account, err
}

func (s *SQLite) UpdateAccount(ctx context.Context, account engine.Account) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET name = ?, auto_trade = ?, baseline_equity = ? WHERE id = ?`,
		account.Name, account.AutoTrade, account.BaselineEquity, account.ID)
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
	if err := scanner.Scan(&account.ID, &account.Name, &mode, &isDefault, &autoTrade,
		&account.BaselineEquity, &createdAt); err != nil {
		return engine.Account{}, err
	}
	account.Mode = engine.AccountMode(mode)
	account.IsDefault = isDefault == 1
	account.AutoTrade = autoTrade == 1
	parsed, err := parseTime(createdAt)
	if err != nil {
		return engine.Account{}, err
	}
	account.CreatedAt = parsed
	return account, nil
}

func (s *SQLite) CreatePosition(ctx context.Context, p engine.Position) (engine.Position, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO positions(
		account_id, market_id, condition_id, question, outcome, token_id, side, entry_price,
		current_price, shares, cost, fee, status, opened_at, closed_at, end_date,
		realized_pnl, unrealized_pnl
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.AccountID, p.MarketID, p.ConditionID, p.Question, p.Outcome, p.TokenID, p.Side, p.EntryPrice,
		p.CurrentPrice, p.Shares, p.Cost, p.Fee, p.Status, formatTime(p.OpenedAt),
		nullableTime(p.ClosedAt), formatTime(p.EndDate), p.RealizedPNL, p.UnrealizedPNL)
	if err != nil {
		return engine.Position{}, err
	}
	p.ID, err = result.LastInsertId()
	return p, err
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
		account_id, position_id, market_id, question, outcome, token_id, kind, type,
		amount, price, shares, status, reason, external_id, created_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		order.AccountID, order.PositionID, order.MarketID, order.Question, order.Outcome, order.TokenID,
		order.Kind, order.Type, order.Amount, order.Price, order.Shares, order.Status,
		order.Reason, order.ExternalID, formatTime(order.CreatedAt))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *SQLite) ListPositions(ctx context.Context, accountID int64, status string) ([]engine.Position, error) {
	query := `SELECT id, account_id, market_id, condition_id, question, outcome, token_id, side,
		entry_price, current_price, shares, cost, fee, status, opened_at, closed_at,
		end_date, realized_pnl, unrealized_pnl FROM positions WHERE account_id = ?`
	args := []any{accountID}
	if status != "all" {
		query += " AND status = ?"
		args = append(args, status)
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
		if err := rows.Scan(&p.ID, &p.AccountID, &p.MarketID, &p.ConditionID, &p.Question, &p.Outcome,
			&p.TokenID, &p.Side, &p.EntryPrice, &p.CurrentPrice, &p.Shares, &p.Cost,
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

func (s *SQLite) ListOrders(ctx context.Context, accountID int64, limit int) ([]engine.Order, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_id, position_id, market_id, question, outcome,
		token_id, kind, type, amount, price, shares, status, reason, external_id, created_at
		FROM orders WHERE account_id = ? ORDER BY id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []engine.Order{}
	for rows.Next() {
		var order engine.Order
		var positionID sql.NullInt64
		var createdAt string
		if err := rows.Scan(&order.ID, &order.AccountID, &positionID, &order.MarketID, &order.Question,
			&order.Outcome, &order.TokenID, &order.Kind, &order.Type, &order.Amount,
			&order.Price, &order.Shares, &order.Status, &order.Reason, &order.ExternalID, &createdAt); err != nil {
			return nil, err
		}
		if positionID.Valid {
			order.PositionID = &positionID.Int64
		}
		order.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (s *SQLite) CountOrders(ctx context.Context, accountID int64) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders WHERE account_id = ?", accountID).Scan(&count)
	return count, err
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO equity_snapshots(account_id, time, value) VALUES(?, ?, ?)",
		accountID, formatTime(point.Time), point.Value); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM equity_snapshots WHERE account_id = ? AND id NOT IN
		(SELECT id FROM equity_snapshots WHERE account_id = ? ORDER BY id DESC LIMIT 5000)`, accountID, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) ListSnapshots(ctx context.Context, accountID int64, limit int) ([]engine.EquityPoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT time, value FROM (
		SELECT id, time, value FROM equity_snapshots WHERE account_id = ? ORDER BY id DESC LIMIT ?
	) ORDER BY id ASC`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []engine.EquityPoint{}
	for rows.Next() {
		var point engine.EquityPoint
		var timestamp string
		if err := rows.Scan(&timestamp, &point.Value); err != nil {
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
