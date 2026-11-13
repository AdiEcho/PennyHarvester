package store

import (
	"context"
	"database/sql"
	"errors"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

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

// InitializeAccountBaseline is the compare-and-set used by the live baseline
// bootstrap. Unconditional writes let a Dashboard that read stale positions
// replace a baseline committed by a scan.
func (s *SQLite) InitializeAccountBaseline(ctx context.Context, id int64, value float64) (bool, error) {
	result, err := s.db.ExecContext(ctx, "UPDATE accounts SET baseline_equity = ? WHERE id = ? AND baseline_equity = 0", value, id)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
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
