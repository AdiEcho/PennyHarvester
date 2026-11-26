package store

import (
	"context"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

// snapshotRetention bounds the rows kept per account. At the one-minute
// snapshot cadence this covers about 69 days, well beyond the dashboard's
// 1m range, while keeping an aggressive scan interval from growing the
// database and the ledger export without bound.
const snapshotRetention = 100000

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
	// Trim by the id of the oldest retained row instead of NOT IN over the
	// whole account: the index seek of one id stays cheap as the table grows,
	// and fewer rows than the cap makes the subquery NULL so nothing is deleted.
	if _, err := tx.ExecContext(ctx, `DELETE FROM equity_snapshots WHERE account_id = ? AND id < (
		SELECT id FROM equity_snapshots WHERE account_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?
	)`, accountID, accountID, snapshotRetention-1); err != nil {
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
	return scanSnapshots(rows)
}

// ListSnapshotWindow returns the last snapshot at or before since (the window's
// opening anchor), followed by every snapshot after since, oldest first. The
// anchor lets the dashboard draw the equity the window started from even when
// the first snapshot inside the window arrived later. The SQL time bound is
// widened by one second because RFC3339Nano trims trailing zeros, which makes
// same-second timestamps compare imprecisely as strings; the exact cutoff is
// applied again on the parsed values.
func (s *SQLite) ListSnapshotWindow(ctx context.Context, accountID int64, since time.Time) (*engine.EquityPoint, []engine.EquityPoint, error) {
	boundary, err := s.snapshotAtOrBefore(ctx, accountID, since)
	if err != nil {
		return nil, nil, err
	}
	if boundary != nil && boundary.Time.After(since) {
		// A same-second timestamp that sorts before the cutoff string is still
		// inside the window; let the window query return it in order.
		boundary = nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT venue, currency, time, value FROM equity_snapshots
		WHERE account_id = ? AND time > ? ORDER BY id ASC`, accountID, formatTime(since.Add(-time.Second)))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	points, err := scanSnapshots(rows)
	if err != nil {
		return nil, nil, err
	}
	kept := points[:0]
	for _, point := range points {
		if point.Time.After(since) {
			kept = append(kept, point)
			continue
		}
		// A same-second timestamp can sort on the wrong side of the string
		// comparison used by the SQL bound. The widened query recovers it here,
		// and the newest such row becomes the anchor instead of being lost
		// between the two queries.
		if boundary == nil || !point.Time.Before(boundary.Time) {
			candidate := point
			boundary = &candidate
		}
	}
	return boundary, kept, nil
}

func (s *SQLite) snapshotAtOrBefore(ctx context.Context, accountID int64, since time.Time) (*engine.EquityPoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT venue, currency, time, value FROM equity_snapshots
		WHERE account_id = ? AND time <= ? ORDER BY id DESC LIMIT 1`, accountID, formatTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points, err := scanSnapshots(rows)
	if err != nil || len(points) == 0 {
		return nil, err
	}
	return &points[0], nil
}

func scanSnapshots(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]engine.EquityPoint, error) {
	points := []engine.EquityPoint{}
	for rows.Next() {
		var point engine.EquityPoint
		var timestamp string
		if err := rows.Scan(&point.Venue, &point.Currency, &timestamp, &point.Value); err != nil {
			return nil, err
		}
		parsed, err := parseTime(timestamp)
		if err != nil {
			return nil, err
		}
		point.Time = parsed
		points = append(points, point)
	}
	return points, rows.Err()
}
