package store

import (
	"context"
	"database/sql"
	"fmt"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

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
