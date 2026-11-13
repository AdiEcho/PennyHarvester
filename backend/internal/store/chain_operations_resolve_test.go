package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

func TestChainOperationResolutionColumnsMigrateLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy.db"
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacySchema := `
		CREATE TABLE chain_operations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL DEFAULT 0,
			venue TEXT NOT NULL,
			chain_id INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL,
			position_id INTEGER,
			market_id TEXT,
			token_id TEXT,
			from_address TEXT NOT NULL,
			nonce INTEGER,
			tx_hash TEXT,
			status TEXT NOT NULL CHECK (status IN ('intent','signed','broadcast','confirmed','reverted','unknown')),
			reason TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		INSERT INTO chain_operations (
			account_id, venue, chain_id, kind, position_id, from_address, tx_hash,
			status, reason, created_at, updated_at
		) VALUES (7, 'predictfun', 97, 'redeem', 41, '0xabc', '0x' || printf('%064d', 1),
			'unknown', 'operator must check', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z');
	`
	if _, err := raw.ExecContext(ctx, legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer repository.Close()
	// migrate is idempotent: a second pass must not duplicate or fail.
	if err := repository.migrate(ctx); err != nil {
		t.Fatalf("second migration pass: %v", err)
	}

	rows, err := repository.db.QueryContext(ctx, "PRAGMA table_info(chain_operations)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]int{}
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defaultV, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"resolved_by", "resolution_note", "resolved_at"} {
		if columns[column] != 1 {
			t.Fatalf("expected exactly one %s column after migration, got %d (%v)", column, columns[column], columns)
		}
	}

	operation, found, err := repository.ChainOperationByID(ctx, 1)
	if err != nil || !found {
		t.Fatalf("legacy row was not readable: found=%v err=%v", found, err)
	}
	if operation.Status != "unknown" || operation.Reason != "operator must check" {
		t.Fatalf("migration changed legacy data: %+v", operation)
	}
	if operation.TxHash != "0x"+strings.Repeat("0", 63)+"1" {
		t.Fatalf("legacy tx hash changed: %q", operation.TxHash)
	}
	if operation.ResolvedBy != "" || operation.ResolutionNote != "" || operation.ResolvedAt != nil {
		t.Fatalf("legacy row must start unresolved: %+v", operation)
	}
}

func newChainOperationFixture(t *testing.T, repository *SQLite, accountID int64, venue market.Venue, kind string, positionID int64) engine.ChainOperation {
	t.Helper()
	ctx := context.Background()
	var position *int64
	if kind == "redeem" {
		position = &positionID
	}
	operation, err := repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: accountID, Venue: venue, ChainID: 97, Kind: kind, PositionID: position,
		MarketID: "market", TokenID: "token",
		FromAddress: "0x0000000000000000000000000000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func unresolvedChainOperation(t *testing.T, repository *SQLite, accountID int64, venue market.Venue, positionID int64, hash string) engine.ChainOperation {
	t.Helper()
	ctx := context.Background()
	operation := newChainOperationFixture(t, repository, accountID, venue, "redeem", positionID)
	if hash != "" {
		if err := repository.SetChainOperationHash(ctx, operation.ID, hash, operation.FromAddress, nil, 97); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.FinishChainOperation(ctx, operation.ID, "unknown", "等待运营核对链上记录"); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repository.ChainOperationByID(ctx, operation.ID)
	if err != nil || !found {
		t.Fatalf("reload unknown operation: found=%v err=%v", found, err)
	}
	return stored
}

func TestListChainOperationsFiltersPaginationAndCount(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	accounts, err := repository.ListAccounts(ctx)
	if err != nil || len(accounts) < 2 {
		t.Fatalf("expected two seeded accounts: %+v err=%v", accounts, err)
	}
	accountID := accounts[0].ID
	otherAccountID := accounts[1].ID

	for i := 0; i < 55; i++ {
		unresolvedChainOperation(t, repository, accountID, market.VenuePredictFun, int64(1000+i), "")
	}
	confirmed := unresolvedChainOperation(t, repository, accountID, market.VenuePredictFun, 2000, "0x"+strings.Repeat("2", 64))
	if _, err := repository.ResolveChainOperation(ctx, confirmed.ID, "confirmed", "链上成功", "ops", ""); err != nil {
		t.Fatal(err)
	}
	reverted := unresolvedChainOperation(t, repository, accountID, market.VenuePredictFun, 2001, "0x"+strings.Repeat("3", 64))
	if _, err := repository.ResolveChainOperation(ctx, reverted.ID, "reverted", "链上回滚", "ops", ""); err != nil {
		t.Fatal(err)
	}
	unresolvedChainOperation(t, repository, otherAccountID, market.VenuePolymarket, 3000, "")

	unknown, err := repository.ListChainOperations(ctx, engine.ChainOperationFilter{
		AccountID: accountID, Venue: market.VenuePredictFun, Status: "unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Total != 55 || len(unknown.Items) != 50 || unknown.Limit != 50 || unknown.Offset != 0 {
		t.Fatalf("unexpected default unknown page: total=%d items=%d limit=%d offset=%d", unknown.Total, len(unknown.Items), unknown.Limit, unknown.Offset)
	}

	secondPage, err := repository.ListChainOperations(ctx, engine.ChainOperationFilter{
		AccountID: accountID, Venue: market.VenuePredictFun, Status: "unknown", Limit: 10, Offset: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondPage.Total != 55 || len(secondPage.Items) != 5 || secondPage.Limit != 10 || secondPage.Offset != 50 {
		t.Fatalf("unexpected second page: total=%d items=%d limit=%d offset=%d", secondPage.Total, len(secondPage.Items), secondPage.Limit, secondPage.Offset)
	}
	emptyPage, err := repository.ListChainOperations(ctx, engine.ChainOperationFilter{
		AccountID: accountID, Venue: market.VenuePredictFun, Status: "unknown", Offset: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if emptyPage.Total != 55 || len(emptyPage.Items) != 0 {
		t.Fatalf("offset past the end must keep total and return no rows: %+v", emptyPage)
	}

	otherAccount, err := repository.ListChainOperations(ctx, engine.ChainOperationFilter{AccountID: otherAccountID})
	if err != nil {
		t.Fatal(err)
	}
	if otherAccount.Total != 1 || len(otherAccount.Items) != 1 || otherAccount.Items[0].Venue != market.VenuePolymarket {
		t.Fatalf("account filter leaked rows: %+v", otherAccount)
	}
	polymarket, err := repository.ListChainOperations(ctx, engine.ChainOperationFilter{Venue: market.VenuePolymarket})
	if err != nil {
		t.Fatal(err)
	}
	if polymarket.Total != 1 {
		t.Fatalf("venue filter expected 1 row, got %+v", polymarket)
	}
	confirmedPage, err := repository.ListChainOperations(ctx, engine.ChainOperationFilter{Status: "confirmed"})
	if err != nil {
		t.Fatal(err)
	}
	if confirmedPage.Total != 1 || confirmedPage.Items[0].ID != confirmed.ID {
		t.Fatalf("status filter expected the confirmed row, got %+v", confirmedPage)
	}

	all, err := repository.ListChainOperations(ctx, engine.ChainOperationFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 58 || len(all.Items) != 58 || all.Limit != 200 {
		t.Fatalf("limit must clamp to 200 while total counts all rows: total=%d items=%d limit=%d", all.Total, len(all.Items), all.Limit)
	}
	for i := 1; i < len(all.Items); i++ {
		previous, current := all.Items[i-1], all.Items[i]
		if current.UpdatedAt.After(previous.UpdatedAt) || (current.UpdatedAt.Equal(previous.UpdatedAt) && current.ID > previous.ID) {
			t.Fatalf("page is not ordered updated_at DESC, id DESC at %d: %+v then %+v", i, previous, current)
		}
	}

	for _, filter := range []engine.ChainOperationFilter{
		{AccountID: -1},
		{Venue: market.Venue("nowhere")},
		{Status: "done"},
	} {
		if _, err := repository.ListChainOperations(ctx, filter); !errors.Is(err, engine.ErrChainOperationInvalid) {
			t.Fatalf("filter %+v must be rejected with ErrChainOperationInvalid, got %v", filter, err)
		}
	}
	// Store-level pagination is forgiving: handlers validate raw query input, and
	// the store still clamps a non-positive limit to its default.
	normalized, err := repository.ListChainOperations(ctx, engine.ChainOperationFilter{Limit: -1, Offset: -5})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Limit != 50 || normalized.Offset != 0 {
		t.Fatalf("negative pagination must normalize to defaults, got limit=%d offset=%d", normalized.Limit, normalized.Offset)
	}
}

func TestResolveChainOperationRequiresUnknownAndPersistsAudit(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	accounts, err := repository.ListAccounts(ctx)
	if err != nil || len(accounts) == 0 {
		t.Fatalf("expected seeded accounts: %+v err=%v", accounts, err)
	}
	accountID := accounts[0].ID

	existingHash := "0x" + strings.Repeat("a", 64)
	operation := unresolvedChainOperation(t, repository, accountID, market.VenuePredictFun, 41, existingHash)
	before := time.Now().UTC().Add(-time.Second)
	resolved, err := repository.ResolveChainOperation(ctx, operation.ID, "confirmed", "  已在链上确认  ", "  alice  ", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != "confirmed" || resolved.ResolvedBy != "alice" || resolved.ResolutionNote != "已在链上确认" {
		t.Fatalf("audit fields were not persisted: %+v", resolved)
	}
	if resolved.ResolvedAt == nil || resolved.ResolvedAt.Before(before) {
		t.Fatalf("resolved_at was not set to the resolution time: %+v", resolved.ResolvedAt)
	}
	if resolved.Reason != "等待运营核对链上记录" {
		t.Fatalf("resolution must keep the original reason, got %q", resolved.Reason)
	}
	if resolved.TxHash != existingHash {
		t.Fatalf("resolution must keep the existing hash, got %q", resolved.TxHash)
	}
	if err := repository.FinishChainOperation(ctx, operation.ID, "unknown", "should not overwrite"); !errors.Is(err, engine.ErrChainOperationConflict) {
		t.Fatalf("FinishChainOperation must refuse a resolved row, got %v", err)
	}
	stored, found, err := repository.ChainOperationByID(ctx, operation.ID)
	if err != nil || !found || stored.Status != "confirmed" {
		t.Fatalf("resolved rows must be final, got found=%v err=%v op=%+v", found, err, stored)
	}
	if stored.Reason != "等待运营核对链上记录" || stored.ResolvedBy != "alice" {
		t.Fatalf("FinishChainOperation overwrote a resolved row: %+v", stored)
	}

	replayed, err := repository.ResolveChainOperation(ctx, operation.ID, "confirmed", "重复提交", "alice", strings.ToUpper(existingHash))
	if err != nil {
		t.Fatalf("repeating the same resolution must be idempotent: %v", err)
	}
	if replayed.ID != operation.ID || replayed.Status != "confirmed" || replayed.ResolvedAt == nil {
		t.Fatalf("idempotent replay returned a different row: %+v", replayed)
	}
	if _, err := repository.ResolveChainOperation(ctx, operation.ID, "reverted", "另一个结果", "alice", ""); !errors.Is(err, engine.ErrChainOperationConflict) {
		t.Fatalf("a different final outcome must conflict, got %v", err)
	}

	withoutHash := unresolvedChainOperation(t, repository, accountID, market.VenuePredictFun, 42, "")
	if _, err := repository.ResolveChainOperation(ctx, withoutHash.ID, "confirmed", "确认", "alice", ""); !errors.Is(err, engine.ErrChainOperationInvalid) {
		t.Fatalf("confirmed without any hash must be refused, got %v", err)
	}
	newHash := "0x" + strings.Repeat("b", 64)
	reverted, err := repository.ResolveChainOperation(ctx, withoutHash.ID, "reverted", "链上回滚", "bob", newHash)
	if err != nil {
		t.Fatal(err)
	}
	if reverted.Status != "reverted" || reverted.TxHash != newHash {
		t.Fatalf("operator-supplied hash should be stored for an empty row: %+v", reverted)
	}

	conflicting := unresolvedChainOperation(t, repository, accountID, market.VenuePredictFun, 43, existingHash)
	if _, err := repository.ResolveChainOperation(ctx, conflicting.ID, "reverted", "回滚", "alice", "0x"+strings.Repeat("c", 64)); !errors.Is(err, engine.ErrChainOperationConflict) {
		t.Fatalf("a different supplied hash must conflict, got %v", err)
	}
	storedConflict, found, err := repository.ChainOperationByID(ctx, conflicting.ID)
	if err != nil || !found || storedConflict.Status != "unknown" || storedConflict.ResolvedBy != "" {
		t.Fatalf("a rejected resolution must not mutate the row: found=%v err=%v op=%+v", found, err, storedConflict)
	}

	intent := newChainOperationFixture(t, repository, accountID, market.VenuePredictFun, "redeem", 44)
	if _, err := repository.ResolveChainOperation(ctx, intent.ID, "confirmed", "确认", "alice", existingHash); !errors.Is(err, engine.ErrChainOperationConflict) {
		t.Fatalf("intent rows belong to recovery, not manual resolution, got %v", err)
	}
	if _, err := repository.ResolveChainOperation(ctx, 99999, "confirmed", "确认", "alice", existingHash); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("missing row must return ErrNotFound, got %v", err)
	}

	for _, test := range []struct {
		name   string
		id     int64
		status string
		reason string
		actor  string
		hash   string
	}{
		{"zero id", 0, "confirmed", "r", "a", existingHash},
		{"unknown outcome", operation.ID, "unknown", "r", "a", existingHash},
		{"empty actor", operation.ID, "confirmed", "r", "   ", existingHash},
		{"long actor", operation.ID, "confirmed", "r", strings.Repeat("a", 121), existingHash},
		{"empty reason", operation.ID, "confirmed", "   ", "a", existingHash},
		{"long reason", operation.ID, "confirmed", strings.Repeat("r", 501), "a", existingHash},
		{"short hash", operation.ID, "confirmed", "r", "a", "0x1234"},
		{"non-hex hash", operation.ID, "confirmed", "r", "a", "0x" + strings.Repeat("z", 64)},
	} {
		if _, err := repository.ResolveChainOperation(ctx, test.id, test.status, test.reason, test.actor, test.hash); !errors.Is(err, engine.ErrChainOperationInvalid) && !errors.Is(err, engine.ErrNotFound) {
			t.Fatalf("%s must be rejected, got %v", test.name, err)
		}
	}
}
