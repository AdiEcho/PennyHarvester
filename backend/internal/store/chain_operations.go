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
	       from_address, nonce, tx_hash, status, reason, resolved_by, resolution_note,
	       resolved_at, created_at, updated_at
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
	op.ResolvedBy = ""
	op.ResolutionNote = ""
	op.ResolvedAt = nil
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

const (
	defaultChainOperationLimit = 50
	maxChainOperationLimit     = 200
)

// ListChainOperations returns the audit view over the journal. The count and
// the page use the same WHERE clause so Total always describes the filter, and
// the order is fully deterministic: updated_at carries a nanosecond RFC3339Nano
// value and id breaks a tie written within the same clock tick.
func (s *SQLite) ListChainOperations(ctx context.Context, filter engine.ChainOperationFilter) (engine.ChainOperationPage, error) {
	filter.Status = strings.TrimSpace(filter.Status)
	if strings.EqualFold(filter.Status, "all") {
		filter.Status = ""
	}
	if filter.AccountID < 0 {
		return engine.ChainOperationPage{}, fmt.Errorf("%w: account_id 不能为负数", engine.ErrChainOperationInvalid)
	}
	if filter.Venue != "" && !filter.Venue.Valid() {
		return engine.ChainOperationPage{}, fmt.Errorf("%w: 未知场所 %q", engine.ErrChainOperationInvalid, filter.Venue)
	}
	if filter.Status != "" && !validChainOperationStatus(filter.Status) {
		return engine.ChainOperationPage{}, fmt.Errorf("%w: 无效的链上操作状态 %q", engine.ErrChainOperationInvalid, filter.Status)
	}
	limit, offset := normalizeChainOperationPage(filter.Limit, filter.Offset)
	where, args := chainOperationFilterSQL(filter)

	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM chain_operations"+where, args...).Scan(&total); err != nil {
		return engine.ChainOperationPage{}, fmt.Errorf("统计链上操作: %w", err)
	}
	// updated_at is RFC3339Nano text, whose fraction width varies; julianday
	// compares the instants instead of the lexicographic strings. id is the
	// deterministic tie-breaker for equal timestamps.
	query := chainOperationColumns + where + " ORDER BY julianday(updated_at) DESC, id DESC LIMIT ? OFFSET ?"
	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return engine.ChainOperationPage{}, err
	}
	defer rows.Close()
	page := engine.ChainOperationPage{Items: make([]engine.ChainOperation, 0, limit), Total: total, Limit: limit, Offset: offset}
	for rows.Next() {
		op, ok, err := scanChainOperation(rows)
		if err != nil {
			return engine.ChainOperationPage{}, err
		}
		if ok {
			page.Items = append(page.Items, op)
		}
	}
	if err := rows.Err(); err != nil {
		return engine.ChainOperationPage{}, err
	}
	return page, nil
}

// ResolveChainOperation manually closes an unknown row. The write is one
// conditional UPDATE guarded by status = 'unknown'; the database is opened with
// MaxOpenConns(1), so statements are serialized and the guard, not a read-then-
// write check in Go, decides which concurrent resolution wins. A zero-row
// result is re-read to distinguish an idempotent replay from a different final
// outcome or a deleted row.
func (s *SQLite) ResolveChainOperation(ctx context.Context, id int64, status, reason, actor, txHash string) (engine.ChainOperation, error) {
	return s.resolveChainOperation(ctx, id, status, reason, actor, txHash, 0, "")
}

// ResolveChainOperationAndOrder closes an unknown row and, when orderID is
// non-zero, applies the matching redemption status in the same SQLite
// transaction. A manual resolution must not leave a confirmed redemption with a
// pending order: the row would leave the unresolved list and the order would
// never be retried. The transaction rolls back if either write fails, so the
// operator can retry the same resolution.
func (s *SQLite) ResolveChainOperationAndOrder(ctx context.Context, id int64, status, reason, actor, txHash string, orderID int64, orderStatus string) (engine.ChainOperation, error) {
	return s.resolveChainOperation(ctx, id, status, reason, actor, txHash, orderID, orderStatus)
}

func (s *SQLite) resolveChainOperation(ctx context.Context, id int64, status, reason, actor, txHash string, orderID int64, orderStatus string) (engine.ChainOperation, error) {
	status = strings.TrimSpace(status)
	reason = strings.TrimSpace(reason)
	actor = strings.TrimSpace(actor)
	if id <= 0 {
		return engine.ChainOperation{}, fmt.Errorf("%w: 链上操作 %d 不存在", engine.ErrNotFound, id)
	}
	if status != "confirmed" && status != "reverted" {
		return engine.ChainOperation{}, fmt.Errorf("%w: 状态仅支持 confirmed 或 reverted", engine.ErrChainOperationInvalid)
	}
	if actor == "" || len([]rune(actor)) > 120 {
		return engine.ChainOperation{}, fmt.Errorf("%w: 操作人必填且不超过 120 个字符", engine.ErrChainOperationInvalid)
	}
	if reason == "" || len([]rune(reason)) > 500 {
		return engine.ChainOperation{}, fmt.Errorf("%w: 核对说明必填且不超过 500 个字符", engine.ErrChainOperationInvalid)
	}
	hash, err := normalizeStoredTxHash(txHash)
	if err != nil {
		return engine.ChainOperation{}, err
	}

	current, ok, err := s.ChainOperationByID(ctx, id)
	if err != nil {
		return engine.ChainOperation{}, err
	}
	if !ok {
		return engine.ChainOperation{}, fmt.Errorf("%w: 链上操作 %d 不存在", engine.ErrNotFound, id)
	}
	if hash != "" && current.TxHash != "" && !strings.EqualFold(hash, current.TxHash) {
		return engine.ChainOperation{}, fmt.Errorf("%w: 链上操作 %d 已记录其他交易哈希", engine.ErrChainOperationConflict, id)
	}
	if current.Status != "unknown" {
		if current.Status == status {
			return current, nil
		}
		return engine.ChainOperation{}, fmt.Errorf("%w: 链上操作 %d 当前状态 %s，无法改为 %s", engine.ErrChainOperationConflict, id, current.Status, status)
	}
	if status == "confirmed" && hash == "" && current.TxHash == "" {
		return engine.ChainOperation{}, fmt.Errorf("%w: 确认上链必须提供交易哈希", engine.ErrChainOperationInvalid)
	}

	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return engine.ChainOperation{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE chain_operations
		SET status = ?, resolved_by = ?, resolution_note = ?, resolved_at = ?,
		    tx_hash = CASE WHEN (tx_hash IS NULL OR tx_hash = '') AND ? <> '' THEN ? ELSE tx_hash END,
		    updated_at = ?
		WHERE id = ? AND status = 'unknown'`,
		status, actor, reason, formatTime(now), hash, hash, formatTime(now), id)
	if err != nil {
		return engine.ChainOperation{}, fmt.Errorf("解决链上操作: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return engine.ChainOperation{}, err
	}
	if affected > 0 && orderID > 0 {
		// The order may already be final (for example a repeated attempt after
		// a timeout); a zero-row update is not an error. A real write failure
		// rolls back the chain row too, keeping the retry path open.
		if _, err := tx.ExecContext(ctx, `UPDATE orders SET status = ?, reason = ? WHERE id = ? AND status IN ('pending', 'unknown')`,
			orderStatus, reason, orderID); err != nil {
			return engine.ChainOperation{}, fmt.Errorf("同步赎回订单: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return engine.ChainOperation{}, err
	}
	if affected > 0 {
		resolved, ok, err := s.ChainOperationByID(ctx, id)
		if err != nil {
			return engine.ChainOperation{}, err
		}
		if !ok {
			return engine.ChainOperation{}, fmt.Errorf("%w: 链上操作 %d 不存在", engine.ErrNotFound, id)
		}
		return resolved, nil
	}

	// Another request resolved the row between the read and the conditional
	// update. Its state is final; only the same status is an idempotent replay.
	latest, ok, err := s.ChainOperationByID(ctx, id)
	if err != nil {
		return engine.ChainOperation{}, err
	}
	if !ok {
		return engine.ChainOperation{}, fmt.Errorf("%w: 链上操作 %d 不存在", engine.ErrNotFound, id)
	}
	if hash != "" && latest.TxHash != "" && !strings.EqualFold(hash, latest.TxHash) {
		return engine.ChainOperation{}, fmt.Errorf("%w: 链上操作 %d 已记录其他交易哈希", engine.ErrChainOperationConflict, id)
	}
	if latest.Status == status {
		return latest, nil
	}
	return engine.ChainOperation{}, fmt.Errorf("%w: 链上操作 %d 当前状态 %s，无法改为 %s", engine.ErrChainOperationConflict, id, latest.Status, status)
}

func normalizeChainOperationPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultChainOperationLimit
	}
	if limit > maxChainOperationLimit {
		limit = maxChainOperationLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func chainOperationFilterSQL(filter engine.ChainOperationFilter) (string, []any) {
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if filter.AccountID > 0 {
		clauses = append(clauses, "account_id = ?")
		args = append(args, filter.AccountID)
	}
	if filter.Venue != "" {
		clauses = append(clauses, "venue = ?")
		args = append(args, string(filter.Venue))
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, filter.Status)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func validChainOperationStatus(status string) bool {
	switch status {
	case "intent", "signed", "broadcast", "confirmed", "reverted", "unknown":
		return true
	default:
		return false
	}
}

func normalizeStoredTxHash(raw string) (string, error) {
	hash := strings.TrimSpace(raw)
	if hash == "" {
		return "", nil
	}
	if len(hash) != 66 || hash[0] != '0' || (hash[1] != 'x' && hash[1] != 'X') {
		return "", fmt.Errorf("%w: 交易哈希须为 0x 加 64 位十六进制", engine.ErrChainOperationInvalid)
	}
	for _, digit := range hash[2:] {
		if !(digit >= '0' && digit <= '9') && !(digit >= 'a' && digit <= 'f') && !(digit >= 'A' && digit <= 'F') {
			return "", fmt.Errorf("%w: 交易哈希须为 0x 加 64 位十六进制", engine.ErrChainOperationInvalid)
		}
	}
	return strings.ToLower(hash), nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanChainOperation(row rowScanner) (engine.ChainOperation, bool, error) {
	var op engine.ChainOperation
	var venueName, kind string
	var positionID, nonce sql.NullInt64
	var marketID, tokenID, txHash, reason sql.NullString
	var resolvedBy, resolutionNote, resolvedAt sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(&op.ID, &op.AccountID, &venueName, &op.ChainID, &kind, &positionID,
		&marketID, &tokenID, &op.FromAddress, &nonce, &txHash, &op.Status, &reason,
		&resolvedBy, &resolutionNote, &resolvedAt, &createdAt, &updatedAt)
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
	op.ResolvedBy = resolvedBy.String
	op.ResolutionNote = resolutionNote.String
	if resolvedAt.Valid && strings.TrimSpace(resolvedAt.String) != "" {
		parsed, err := parseTime(resolvedAt.String)
		if err != nil {
			return engine.ChainOperation{}, false, err
		}
		op.ResolvedAt = &parsed
	}
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
