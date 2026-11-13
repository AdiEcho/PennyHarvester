package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

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
		account_id, venue, currency, position_id, market_id, event_id, question, outcome, token_id, kind, type, action,
		amount, price, shares, fee, status, reason, external_id, created_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		order.AccountID, order.EffectiveVenue(), order.EffectiveCurrency(), order.PositionID, order.MarketID,
		order.EventID, order.Question, order.Outcome, order.TokenID, order.Kind, order.Type, order.Action, order.Amount,
		order.Price, order.Shares, order.Fee, order.Status, order.Reason, order.ExternalID, formatTime(order.CreatedAt))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
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

const orderColumns = `SELECT id, account_id, venue, currency, position_id, market_id, event_id, question, outcome,
	token_id, kind, type, action, amount, price, shares, fee, status, reason, external_id, created_at
	FROM orders`

func scanOrderRows(rows *sql.Rows) ([]engine.Order, error) {
	orders := []engine.Order{}
	for rows.Next() {
		var order engine.Order
		var positionID sql.NullInt64
		var createdAt string
		if err := rows.Scan(&order.ID, &order.AccountID, &order.Venue, &order.Currency, &positionID, &order.MarketID,
			&order.EventID, &order.Question, &order.Outcome, &order.TokenID, &order.Kind, &order.Type, &order.Action, &order.Amount,
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

func scanOrderRow(row *sql.Row) (engine.Order, error) {
	var order engine.Order
	var positionID sql.NullInt64
	var createdAt string
	if err := row.Scan(&order.ID, &order.AccountID, &order.Venue, &order.Currency, &positionID, &order.MarketID,
		&order.EventID, &order.Question, &order.Outcome, &order.TokenID, &order.Kind, &order.Type, &order.Action, &order.Amount,
		&order.Price, &order.Shares, &order.Fee, &order.Status, &order.Reason, &order.ExternalID, &createdAt); err != nil {
		return engine.Order{}, err
	}
	if positionID.Valid {
		order.PositionID = &positionID.Int64
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return engine.Order{}, err
	}
	order.CreatedAt = parsed
	return order, nil
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

// RedeemIntent looks up the redemption row for one position directly. The
// engine must never send a second on-chain redemption because the first one
// scrolled past the recent-orders page.
func (s *SQLite) RedeemIntent(ctx context.Context, accountID, positionID int64) (engine.Order, bool, error) {
	row := s.db.QueryRowContext(ctx, orderColumns+` WHERE account_id = ? AND type = 'settle' AND action = 'redeem'
		AND position_id = ? AND status IN ('pending', 'unknown', 'filled') ORDER BY id DESC LIMIT 1`, accountID, positionID)
	order, err := scanOrderRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return engine.Order{}, false, nil
	}
	if err != nil {
		return engine.Order{}, false, err
	}
	return order, true, nil
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
	// Only a filled entry starts the strategy clock. Rejected or still-pending
	// attempts must not make an account look older (and its return lower) than
	// the first position it actually opened.
	err := s.db.QueryRowContext(ctx, `SELECT created_at FROM orders
		WHERE account_id = ? AND kind = 'auto' AND type = 'buy' AND status = 'filled'
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
	rows, err := s.db.QueryContext(ctx, orderColumns+" WHERE account_id = ? AND status IN ('pending', 'unknown') ORDER BY id ASC", accountID)
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
		status = 'filled', reason = ?, external_id = CASE WHEN external_id = '' THEN ? ELSE external_id END,
		event_id = CASE WHEN event_id = '' THEN ? ELSE event_id END
		WHERE id = ? AND status IN ('pending', 'unknown')`,
		p.ID, fill.Cost, fill.Price, fill.Shares, fill.Fee, liveFillReason(p, "实盘成交"), fill.OrderID, p.EventID, orderID)
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
		status = 'filled', reason = ?, external_id = CASE WHEN external_id = '' THEN ? ELSE external_id END,
		event_id = CASE WHEN event_id = '' THEN ? ELSE event_id END
		WHERE id = ? AND status IN ('pending', 'unknown')`,
		p.ID, fill.Cost, fill.Price, fill.Shares, fill.Fee, liveFillReason(p, "止损成交"), fill.OrderID, p.EventID, orderID)
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
