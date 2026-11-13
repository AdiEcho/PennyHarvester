package store

import (
	"context"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

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
