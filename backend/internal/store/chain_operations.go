package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

const chainOperationColumns = `
	SELECT id, account_id, venue, chain_id, kind, position_id, market_id, token_id,
	       from_address, nonce, tx_hash, status, reason, created_at, updated_at
	FROM chain_operations`

// BeginChainOperation inserts an intent. The caller supplies a non-zero
// CreatedAt when a test needs an old row; production uses now.
func (s *SQLite) BeginChainOperation(ctx context.Context, op engine.ChainOperation) (engine.ChainOperation, error) {
	now := time.Now().UTC()
	if op.CreatedAt.IsZero() {
		op.CreatedAt = now
	}
	op.CreatedAt = op.CreatedAt.UTC()
	op.UpdatedAt = op.CreatedAt
	op.Status = "intent"
	op.TxHash = ""
	op.Reason = ""
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO chain_operations (
			account_id, venue, chain_id, kind, position_id, market_id, token_id,
			from_address, nonce, tx_hash, status, reason, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 'intent', NULL, ?, ?)`,
		op.AccountID, string(op.Venue), op.ChainID, op.Kind, nullableInt64Value(op.PositionID),
		nullableStringValue(op.MarketID), nullableStringValue(op.TokenID), op.FromAddress,
		nullableUint64Value(op.Nonce), formatTime(op.CreatedAt), formatTime(op.UpdatedAt))
	if err != nil {
		// The partial unique indexes are the duplicate guard even if two
		// processes race past the engine's read. The sentinel keeps the API's
		// "already confirming" response identical to the read-path branch.
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED") {
			return engine.ChainOperation{}, engine.ErrChainOperationActive
		}
		return engine.ChainOperation{}, fmt.Errorf("创建链上操作: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return engine.ChainOperation{}, err
	}
	op.ID = id
	return op, nil
}

// SetChainOperationHash journals the signed hash before the transaction is
// broadcast. Only an intent may acquire a hash; a late or repeated write cannot
// change a different transaction's identity.
func (s *SQLite) SetChainOperationHash(ctx context.Context, id int64, hash, from string, nonce *uint64, chainID int64) error {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return errors.New("链上操作哈希为空")
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE chain_operations
		SET tx_hash = ?, from_address = ?, nonce = ?, chain_id = ?, status = 'signed', updated_at = ?
		WHERE id = ? AND status = 'intent' AND tx_hash IS NULL`,
		hash, from, nullableUint64Value(nonce), chainID, formatTime(now), id)
	if err != nil {
		return fmt.Errorf("记录链上交易哈希: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		return nil
	}
	current, ok, err := s.ChainOperationByID(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return engine.ErrNotFound
	}
	if current.TxHash == hash {
		// The first journal write succeeded but the caller retried after an
		// ambiguous error. The transaction identity is already durable.
		return nil
	}
	return fmt.Errorf("%w: 链上操作 %d 已记录其他交易", engine.ErrChainOperationConflict, id)
}

// FinishChainOperation moves an active row to a resolved state. Resolved states
// are final: repeating the same finish is idempotent, but a different outcome is
// a conflict. The update never touches tx_hash, from_address, or nonce. Note
// that "unknown" remains active for duplicate blocking and can later become
// confirmed or reverted after an operator checks the chain.
func (s *SQLite) FinishChainOperation(ctx context.Context, id int64, status, reason string) error {
	switch status {
	case "confirmed", "reverted", "unknown":
	default:
		return fmt.Errorf("%w: 无效的链上操作终态 %q", engine.ErrChainOperationConflict, status)
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE chain_operations
		SET status = ?, reason = ?, updated_at = ?
		WHERE id = ? AND status IN ('intent', 'signed', 'broadcast', 'unknown')`,
		status, nullableStringValue(reason), formatTime(now), id)
	if err != nil {
		return fmt.Errorf("结束链上操作: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		return nil
	}
	current, ok, err := s.ChainOperationByID(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return engine.ErrNotFound
	}
	if current.Status == status {
		return nil
	}
	return fmt.Errorf("%w: 链上操作 %d 当前状态 %s，无法改为 %s", engine.ErrChainOperationConflict, id, current.Status, status)
}

func (s *SQLite) ActiveChainOperation(ctx context.Context, accountID int64, v market.Venue, kind string, positionID *int64) (engine.ChainOperation, bool, error) {
	query := chainOperationColumns + `
		WHERE account_id = ? AND venue = ? AND kind = ?
		  AND status IN ('intent', 'signed', 'broadcast', 'unknown')`
	args := []any{accountID, string(v), kind}
	if positionID == nil {
		query += " AND position_id IS NULL"
	} else {
		query += " AND position_id = ?"
		args = append(args, *positionID)
	}
	query += " ORDER BY id DESC LIMIT 1"
	return scanChainOperation(s.db.QueryRowContext(ctx, query, args...))
}

func (s *SQLite) ListUnresolvedChainOperations(ctx context.Context, limit int) ([]engine.ChainOperation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, chainOperationColumns+`
		WHERE status IN ('intent', 'signed', 'broadcast', 'unknown')
		ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]engine.ChainOperation, 0, limit)
	for rows.Next() {
		op, ok, err := scanChainOperation(rows)
		if err != nil {
			return nil, err
		}
		if ok {
			operations = append(operations, op)
		}
	}
	return operations, rows.Err()
}

func (s *SQLite) ChainOperationByID(ctx context.Context, id int64) (engine.ChainOperation, bool, error) {
	return scanChainOperation(s.db.QueryRowContext(ctx, chainOperationColumns+" WHERE id = ?", id))
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanChainOperation(row rowScanner) (engine.ChainOperation, bool, error) {
	var op engine.ChainOperation
	var venueName, kind string
	var positionID, nonce sql.NullInt64
	var marketID, tokenID, txHash, reason sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(&op.ID, &op.AccountID, &venueName, &op.ChainID, &kind, &positionID,
		&marketID, &tokenID, &op.FromAddress, &nonce, &txHash, &op.Status, &reason,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return engine.ChainOperation{}, false, nil
	}
	if err != nil {
		return engine.ChainOperation{}, false, err
	}
	op.Venue = market.Venue(venueName)
	op.Kind = kind
	if positionID.Valid {
		value := positionID.Int64
		op.PositionID = &value
	}
	if nonce.Valid {
		value := uint64(nonce.Int64)
		op.Nonce = &value
	}
	op.MarketID = marketID.String
	op.TokenID = tokenID.String
	op.TxHash = txHash.String
	op.Reason = reason.String
	op.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return engine.ChainOperation{}, false, err
	}
	op.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return engine.ChainOperation{}, false, err
	}
	return op, true, nil
}

func nullableInt64Value(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableUint64Value(value *uint64) any {
	if value == nil {
		return nil
	}
	return int64(*value)
}

func nullableStringValue(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
