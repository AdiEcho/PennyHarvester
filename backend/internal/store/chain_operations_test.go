package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

func TestChainOperationSchemaAndActiveUniqueness(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	var table string
	if err := repository.db.QueryRowContext(ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'chain_operations'").Scan(&table); err != nil {
		t.Fatalf("chain_operations table was not migrated: %v", err)
	}
	for _, column := range []string{"account_id", "chain_id", "kind", "position_id", "from_address", "nonce", "tx_hash", "status"} {
		if !containsColumn(table, column) {
			t.Fatalf("chain_operations is missing column %s:\n%s", column, table)
		}
	}
	for _, index := range []string{"idx_chain_operations_active_redeem", "idx_chain_operations_active_approve"} {
		var name string
		if err := repository.db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?", index).Scan(&name); err != nil {
			t.Fatalf("partial unique index %s was not migrated: %v", index, err)
		}
	}

	accounts, err := repository.ListAccounts(ctx)
	if err != nil || len(accounts) == 0 {
		t.Fatalf("expected seeded accounts: %+v err=%v", accounts, err)
	}
	accountID := accounts[0].ID
	positionID := int64(41)
	op, err := repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: accountID, Venue: market.VenuePredictFun, ChainID: 97, Kind: "redeem",
		PositionID: &positionID, MarketID: "market-1", TokenID: "token-1",
		FromAddress: "0x0000000000000000000000000000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != "intent" || op.TxHash != "" {
		t.Fatalf("begin must create an empty intent: %+v", op)
	}
	if _, err := repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: accountID, Venue: market.VenuePredictFun, ChainID: 97, Kind: "redeem",
		PositionID: &positionID, FromAddress: "0x0000000000000000000000000000000000000001",
	}); !errors.Is(err, engine.ErrChainOperationActive) {
		t.Fatalf("a second active redeem must be refused with ErrChainOperationActive, got %v", err)
	}

	otherPosition := int64(42)
	if _, err := repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: accountID, Venue: market.VenuePredictFun, ChainID: 97, Kind: "redeem",
		PositionID: &otherPosition, FromAddress: "0x0000000000000000000000000000000000000001",
	}); err != nil {
		t.Fatalf("a different position must have its own active row: %v", err)
	}

	approval, err := repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: 0, Venue: market.VenuePredictFun, ChainID: 97, Kind: "approve",
		FromAddress: "0x0000000000000000000000000000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.BeginChainOperation(ctx, engine.ChainOperation{
		AccountID: 0, Venue: market.VenuePredictFun, ChainID: 97, Kind: "approve",
		FromAddress: "0x0000000000000000000000000000000000000001",
	}); !errors.Is(err, engine.ErrChainOperationActive) {
		t.Fatalf("a second active approval must be refused with ErrChainOperationActive, got %v", err)
	}
	if err := repository.FinishChainOperation(ctx, approval.ID, "confirmed", "ok"); err != nil {
		t.Fatal(err)
	}

	nonce := uint64(9)
	if err := repository.SetChainOperationHash(ctx, op.ID, "0xabc", "0x0000000000000000000000000000000000000001", &nonce, 97); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := repository.ChainOperationByID(ctx, op.ID)
	if err != nil || !ok || stored.Status != "signed" || stored.TxHash != "0xabc" || stored.Nonce == nil || *stored.Nonce != nonce {
		t.Fatalf("hash was not journaled: %+v ok=%v err=%v", stored, ok, err)
	}
	if err := repository.SetChainOperationHash(ctx, op.ID, "0xabc", "0x0000000000000000000000000000000000000001", &nonce, 97); err != nil {
		t.Fatalf("repeating the same hash must be idempotent: %v", err)
	}
	if err := repository.SetChainOperationHash(ctx, op.ID, "0xdef", "0x0000000000000000000000000000000000000001", &nonce, 97); !errors.Is(err, engine.ErrChainOperationConflict) {
		t.Fatalf("a different hash must be a conflict, got %v", err)
	}
	if err := repository.FinishChainOperation(ctx, op.ID, "confirmed", "done"); err != nil {
		t.Fatal(err)
	}
	if err := repository.FinishChainOperation(ctx, op.ID, "confirmed", "again"); err != nil {
		t.Fatalf("repeating the same terminal state must be idempotent: %v", err)
	}
	if err := repository.FinishChainOperation(ctx, op.ID, "reverted", "late"); !errors.Is(err, engine.ErrChainOperationConflict) {
		t.Fatalf("a different terminal state must be a conflict, got %v", err)
	}
	if err := repository.SetChainOperationHash(ctx, op.ID, "0x999", "", &nonce, 97); !errors.Is(err, engine.ErrChainOperationConflict) {
		t.Fatalf("a late hash write must not overwrite a finished operation, got %v", err)
	}
	stored, _, err = repository.ChainOperationByID(ctx, op.ID)
	if err != nil || stored.TxHash != "0xabc" || stored.Status != "confirmed" {
		t.Fatalf("finish changed the transaction identity: %+v err=%v", stored, err)
	}
}

func containsColumn(table, column string) bool {
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 && strings.EqualFold(fields[0], column) {
			return true
		}
	}
	return false
}
