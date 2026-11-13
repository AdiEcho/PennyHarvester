package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pennyharvester/backend/internal/store"
)

const (
	privateKeySentinel = "PRIVATE_KEY_SENTINEL_9f3a"
	apiSecretSentinel  = "API_SECRET_SENTINEL_9f3a"
	passwordSentinel   = "PASSWORD_HASH_SENTINEL_9f3a"
	accountSecret      = "ACCOUNT_SETTINGS_SENTINEL_9f3a"
)

func TestExportAllAccounts(t *testing.T) {
	dbPath := createFixture(t)
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database before export: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "ledger.json")
	stdout, stderr, err := runExport(t, dbPath, "-out", outPath, "-pretty")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if stdout != "" {
		t.Fatalf("stdout must stay empty when -out is set, got %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr must stay empty on success, got %q", stderr)
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database after export: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("database file changed during a read-only export")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dbPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read-only export left sidecar %s (stat err=%v)", suffix, err)
		}
	}
	for _, entry := range mustReadDir(t, filepath.Dir(outPath)) {
		if strings.HasPrefix(entry.Name(), "."+filepath.Base(outPath)+".tmp-") {
			t.Fatalf("atomic export left temporary file %s", entry.Name())
		}
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	ledger := decodeLedger(t, data)

	if ledger.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", ledger.SchemaVersion)
	}
	if ledger.Database != filepath.Base(dbPath) {
		t.Fatalf("database = %q, want %q", ledger.Database, filepath.Base(dbPath))
	}
	if ledger.GeneratedAt.IsZero() {
		t.Fatal("generated_at is zero")
	}
	if _, offset := ledger.GeneratedAt.Zone(); offset != 0 {
		t.Fatalf("generated_at is not UTC: %s", ledger.GeneratedAt)
	}
	if len(ledger.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(ledger.Accounts))
	}
	if ledger.Accounts[0].ID != 1 || ledger.Accounts[1].ID != 2 {
		t.Fatalf("account order = [%d %d], want ascending [1 2]", ledger.Accounts[0].ID, ledger.Accounts[1].ID)
	}

	alpha := accountByID(t, ledger, 1)
	if alpha.Name != "Alpha Live" || alpha.Mode != "live" || alpha.Venue != "polymarket" || alpha.Currency != "USDC" {
		t.Fatalf("unexpected account 1 identity: %+v", alpha)
	}
	if alpha.IsDefault || !alpha.AutoTrade {
		t.Fatalf("unexpected account 1 flags: default=%v auto=%v", alpha.IsDefault, alpha.AutoTrade)
	}
	if alpha.BaselineEquity != 1000.25 {
		t.Fatalf("baseline_equity = %v, want 1000.25", alpha.BaselineEquity)
	}
	if alpha.CreatedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("created_at = %q", alpha.CreatedAt)
	}

	beta := accountByID(t, ledger, 2)
	if beta.Name != "Beta Paper" || beta.Mode != "paper" || beta.Venue != "predictfun" || beta.Currency != "USDT" {
		t.Fatalf("unexpected account 2 identity: %+v", beta)
	}
	if !beta.IsDefault || beta.AutoTrade {
		t.Fatalf("unexpected account 2 flags: default=%v auto=%v", beta.IsDefault, beta.AutoTrade)
	}

	assertIDs(t, "orders", orderIDs(alpha.Orders), []int64{101, 102, 103})
	assertIDs(t, "positions", positionIDs(alpha.Positions), []int64{201, 202, 203})
	assertIDs(t, "chain_operations", chainOperationIDs(alpha.ChainOperations), []int64{300, 301})
	assertIDs(t, "equity_snapshots", snapshotIDs(alpha.EquitySnapshots), []int64{401, 402})
	assertIDs(t, "account 2 orders", orderIDs(beta.Orders), []int64{111})
	assertIDs(t, "account 2 positions", positionIDs(beta.Positions), []int64{211})
	assertIDs(t, "account 2 chain_operations", chainOperationIDs(beta.ChainOperations), []int64{311})
	assertIDs(t, "account 2 equity_snapshots", snapshotIDs(beta.EquitySnapshots), []int64{411})

	if got := alpha.StatusCounts.Orders; got["filled"] != 2 || got["pending"] != 1 || len(got) != 2 {
		t.Fatalf("account 1 order status_counts = %v", got)
	}
	if got := alpha.StatusCounts.Positions; got["open"] != 2 || got["won"] != 1 || len(got) != 2 {
		t.Fatalf("account 1 position status_counts = %v", got)
	}
	if got := alpha.StatusCounts.ChainOperations; got["confirmed"] != 1 || got["unknown"] != 1 || len(got) != 2 {
		t.Fatalf("account 1 chain status_counts = %v", got)
	}

	order101 := orderByID(t, alpha.Orders, 101)
	if order101.ExternalID != "venue-order-101" || order101.Status != "filled" || order101.Amount != 10.5 ||
		order101.Price != 0.52 || order101.Shares != 20.19 || order101.Fee != 0.1 ||
		order101.Reason != "filled at venue" || order101.CreatedAt != "2026-02-01T00:00:00Z" {
		t.Fatalf("order 101 not exported faithfully: %+v", order101)
	}
	if order101.PositionID != nil {
		t.Fatalf("order 101 position_id = %v, want nil", *order101.PositionID)
	}

	position201 := positionByID(t, alpha.Positions, 201)
	if position201.Status != "open" || position201.Shares != 20 || position201.Cost != 10.5 ||
		position201.CurrentPrice != 0.6 || position201.RealizedPNL != 0 || position201.UnrealizedPNL != 1.6 {
		t.Fatalf("position 201 not exported faithfully: %+v", position201)
	}

	chain301 := chainOperationByID(t, alpha.ChainOperations, 301)
	if chain301.FromAddress != "0xdef" || chain301.Nonce == nil || *chain301.Nonce != 8 ||
		chain301.TxHash != "0xtx301" || chain301.Status != "unknown" ||
		chain301.ResolvedBy != "operator@example.test" || chain301.ResolutionNote != "matched venue tx" ||
		chain301.ResolvedAt == nil || *chain301.ResolvedAt != "2026-01-04T00:00:00Z" {
		t.Fatalf("chain operation 301 not exported faithfully: %+v", chain301)
	}

	position202 := positionByID(t, alpha.Positions, 202)
	if position202.ClosedAt == nil || *position202.ClosedAt != "2026-01-05T00:00:00Z" {
		t.Fatalf("position 202 closed_at = %v", position202.ClosedAt)
	}

	raw := string(data)
	for _, secret := range []string{privateKeySentinel, apiSecretSentinel, passwordSentinel, accountSecret} {
		if strings.Contains(raw, secret) {
			t.Fatalf("export contains secret sentinel %q", secret)
		}
	}
	if !strings.Contains(raw, "\n  \"schema_version\": 1") {
		t.Fatal("pretty output is not indented")
	}
	assertStableFields(t, data)
}

func TestExportFilterAndStdout(t *testing.T) {
	dbPath := createFixture(t)
	outPath := filepath.Join(t.TempDir(), "account-2.json")
	stdout, stderr, err := runExport(t, dbPath, "-account", "2", "-out", outPath)
	if err != nil {
		t.Fatalf("filtered export: %v", err)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("unexpected CLI output: stdout=%q stderr=%q", stdout, stderr)
	}
	filtered := decodeLedger(t, mustReadFile(t, outPath))
	if len(filtered.Accounts) != 1 || filtered.Accounts[0].ID != 2 {
		t.Fatalf("filtered accounts = %+v, want only account 2", filtered.Accounts)
	}
	account := filtered.Accounts[0]
	assertIDs(t, "filtered orders", orderIDs(account.Orders), []int64{111})
	assertIDs(t, "filtered positions", positionIDs(account.Positions), []int64{211})
	assertIDs(t, "filtered chain_operations", chainOperationIDs(account.ChainOperations), []int64{311})
	assertIDs(t, "filtered snapshots", snapshotIDs(account.EquitySnapshots), []int64{411})

	// No -out means the JSON document is the only thing on stdout.
	stdout, stderr, err = runExport(t, dbPath, "-account", "1")
	if err != nil {
		t.Fatalf("stdout export: %v", err)
	}
	if stderr != "" {
		t.Fatalf("stderr must stay empty, got %q", stderr)
	}
	if !strings.HasPrefix(stdout, `{"schema_version":1,`) {
		t.Fatalf("stdout does not start with compact JSON: %q", firstLine(stdout))
	}
	printed := decodeLedger(t, []byte(stdout))
	if len(printed.Accounts) != 1 || printed.Accounts[0].ID != 1 {
		t.Fatalf("stdout accounts = %+v, want only account 1", printed.Accounts)
	}
}

func TestExportMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "absent.db")
	outPath := filepath.Join(dir, "export.json")

	_, _, err := runExport(t, dbPath, "-out", outPath)
	if err == nil {
		t.Fatal("expected an error for a missing database")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error = %v, want a clear missing-file error", err)
	}
	if _, statErr := os.Stat(outPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing database created output file (stat err=%v)", statErr)
	}
}

func TestExportMissingAccount(t *testing.T) {
	dbPath := createFixture(t)
	outPath := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(outPath, []byte("previous export"), 0o600); err != nil {
		t.Fatalf("seed output file: %v", err)
	}

	_, _, err := runExport(t, dbPath, "-account", "999", "-out", outPath)
	if err == nil {
		t.Fatal("expected an error for a missing account")
	}
	if !strings.Contains(err.Error(), "account 999 not found") {
		t.Fatalf("error = %v, want missing-account error", err)
	}
	if got := string(mustReadFile(t, outPath)); got != "previous export" {
		t.Fatalf("failed export clobbered an existing output file: %q", got)
	}
}

func TestExportToleratesMissingOptionalColumns(t *testing.T) {
	dbPath := createFixture(t)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture for schema change: %v", err)
	}
	for _, statement := range []string{
		`ALTER TABLE chain_operations DROP COLUMN resolved_by`,
		`ALTER TABLE chain_operations DROP COLUMN resolution_note`,
		`ALTER TABLE chain_operations DROP COLUMN resolved_at`,
		`ALTER TABLE orders DROP COLUMN external_id`,
		`ALTER TABLE positions DROP COLUMN exit_kind`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "legacy.json")
	if _, _, err := runExport(t, dbPath, "-out", outPath); err != nil {
		t.Fatalf("export without optional columns: %v", err)
	}
	ledger := decodeLedger(t, mustReadFile(t, outPath))
	alpha := accountByID(t, ledger, 1)

	order := orderByID(t, alpha.Orders, 101)
	if order.ExternalID != "" {
		t.Fatalf("external_id = %q, want empty when the column is absent", order.ExternalID)
	}
	position := positionByID(t, alpha.Positions, 201)
	if position.ExitKind != "" {
		t.Fatalf("exit_kind = %q, want empty when the column is absent", position.ExitKind)
	}
	operation := chainOperationByID(t, alpha.ChainOperations, 300)
	if operation.TxHash != "0xdeadbeef" || operation.Nonce == nil || *operation.Nonce != 7 {
		t.Fatalf("chain operation lost available columns: %+v", operation)
	}
	if operation.ResolvedBy != "" || operation.ResolutionNote != "" || operation.ResolvedAt != nil {
		t.Fatalf("missing audit columns were invented: %+v", operation)
	}

	var raw map[string]any
	if err := json.Unmarshal(mustReadFile(t, outPath), &raw); err != nil {
		t.Fatalf("unmarshal raw export: %v", err)
	}
	accountRaw := raw["accounts"].([]any)[0].(map[string]any)
	orderRaw := accountRaw["orders"].([]any)[0].(map[string]any)
	for _, field := range []string{"external_id", "status", "amount", "price", "shares", "fee", "reason", "created_at"} {
		if _, ok := orderRaw[field]; !ok {
			t.Fatalf("order object is missing stable field %q", field)
		}
	}
}

func TestExportRefusesToOverwriteDatabase(t *testing.T) {
	dbPath := createFixture(t)
	before := mustReadFile(t, dbPath)

	_, _, err := runExport(t, dbPath, "-out", dbPath)
	if err == nil {
		t.Fatal("expected an error when -out equals -db")
	}
	if !strings.Contains(err.Error(), "must not be the database file") {
		t.Fatalf("error = %v", err)
	}
	if !bytes.Equal(before, mustReadFile(t, dbPath)) {
		t.Fatal("database changed after a rejected output path")
	}
}

func runExport(t *testing.T, dbPath string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	all := append([]string{"-db", dbPath}, args...)
	err := run(context.Background(), all, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func decodeLedger(t *testing.T, data []byte) LedgerExport {
	t.Helper()
	var ledger LedgerExport
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatalf("decode ledger export: %v\n%s", err, data)
	}
	return ledger
}

func accountByID(t *testing.T, ledger LedgerExport, id int64) AccountExport {
	t.Helper()
	for _, account := range ledger.Accounts {
		if account.ID == id {
			return account
		}
	}
	t.Fatalf("account %d not found in export", id)
	return AccountExport{}
}

func orderByID(t *testing.T, orders []OrderExport, id int64) OrderExport {
	t.Helper()
	for _, order := range orders {
		if order.ID == id {
			return order
		}
	}
	t.Fatalf("order %d not found", id)
	return OrderExport{}
}

func positionByID(t *testing.T, positions []PositionExport, id int64) PositionExport {
	t.Helper()
	for _, position := range positions {
		if position.ID == id {
			return position
		}
	}
	t.Fatalf("position %d not found", id)
	return PositionExport{}
}

func chainOperationByID(t *testing.T, operations []ChainOperationExport, id int64) ChainOperationExport {
	t.Helper()
	for _, operation := range operations {
		if operation.ID == id {
			return operation
		}
	}
	t.Fatalf("chain operation %d not found", id)
	return ChainOperationExport{}
}

func orderIDs(orders []OrderExport) []int64 {
	ids := make([]int64, 0, len(orders))
	for _, order := range orders {
		ids = append(ids, order.ID)
	}
	return ids
}

func positionIDs(positions []PositionExport) []int64 {
	ids := make([]int64, 0, len(positions))
	for _, position := range positions {
		ids = append(ids, position.ID)
	}
	return ids
}

func chainOperationIDs(operations []ChainOperationExport) []int64 {
	ids := make([]int64, 0, len(operations))
	for _, operation := range operations {
		ids = append(ids, operation.ID)
	}
	return ids
}

func snapshotIDs(snapshots []EquitySnapshotExport) []int64 {
	ids := make([]int64, 0, len(snapshots))
	for _, snapshot := range snapshots {
		ids = append(ids, snapshot.ID)
	}
	return ids
}

func assertIDs(t *testing.T, label string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s ids = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s ids = %v, want %v", label, got, want)
		}
	}
}

func assertStableFields(t *testing.T, data []byte) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw export: %v", err)
	}
	for _, field := range []string{"schema_version", "generated_at", "database", "accounts"} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("top-level export is missing %q", field)
		}
	}
	account := raw["accounts"].([]any)[0].(map[string]any)
	for _, field := range []string{
		"id", "name", "mode", "venue", "currency", "is_default", "auto_trade",
		"baseline_equity", "created_at", "status_counts", "orders", "positions",
		"chain_operations", "equity_snapshots",
	} {
		if _, ok := account[field]; !ok {
			t.Fatalf("account object is missing %q", field)
		}
	}
	order := account["orders"].([]any)[0].(map[string]any)
	for _, field := range []string{
		"id", "account_id", "venue", "currency", "position_id", "market_id", "event_id",
		"question", "outcome", "token_id", "kind", "type", "action", "amount", "price",
		"shares", "fee", "status", "reason", "external_id", "created_at",
	} {
		if _, ok := order[field]; !ok {
			t.Fatalf("order object is missing %q", field)
		}
	}
	position := account["positions"].([]any)[0].(map[string]any)
	for _, field := range []string{
		"id", "status", "shares", "cost", "entry_price", "current_price", "fee",
		"opened_at", "closed_at", "end_date", "realized_pnl", "unrealized_pnl",
	} {
		if _, ok := position[field]; !ok {
			t.Fatalf("position object is missing %q", field)
		}
	}
	operation := account["chain_operations"].([]any)[0].(map[string]any)
	for _, field := range []string{
		"id", "kind", "chain_id", "from_address", "nonce", "tx_hash", "status",
		"reason", "resolved_by", "resolution_note", "resolved_at", "created_at", "updated_at",
	} {
		if _, ok := operation[field]; !ok {
			t.Fatalf("chain operation object is missing %q", field)
		}
	}
	snapshot := account["equity_snapshots"].([]any)[0].(map[string]any)
	for _, field := range []string{"id", "time", "value"} {
		if _, ok := snapshot[field]; !ok {
			t.Fatalf("snapshot object is missing %q", field)
		}
	}
}

func createFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("create fixture store: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close fixture store: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `PRAGMA foreign_keys = OFF`)
	for _, table := range []string{
		"chain_operations", "equity_snapshots", "orders", "positions",
		"account_settings", "access_password", "system_settings", "settings", "accounts",
	} {
		mustExec(t, db, `DELETE FROM "`+table+`"`)
	}

	mustExec(t, db, `INSERT INTO accounts(id, name, mode, venue, is_default, auto_trade, baseline_equity, created_at)
		VALUES (1, 'Alpha Live', 'live', 'polymarket', 0, 1, 1000.25, '2026-01-01T00:00:00Z')`)
	mustExec(t, db, `INSERT INTO accounts(id, name, mode, venue, is_default, auto_trade, baseline_equity, created_at)
		VALUES (2, 'Beta Paper', 'paper', 'predictfun', 1, 0, 500, '2026-01-02T00:00:00Z')`)

	// Credential-bearing tables must never contribute rows to the export.
	mustExec(t, db, `INSERT INTO settings(id, payload, updated_at) VALUES (1, ?, '2026-01-01T00:00:00Z')`,
		`{"private_key":"`+privateKeySentinel+`","api_secret":"`+apiSecretSentinel+`"}`)
	mustExec(t, db, `INSERT INTO system_settings(id, payload, updated_at) VALUES (1, ?, '2026-01-01T00:00:00Z')`,
		`{"private_key":"`+privateKeySentinel+`","api_secret":"`+apiSecretSentinel+`"}`)
	mustExec(t, db, `INSERT INTO account_settings(account_id, payload, updated_at) VALUES (1, ?, '2026-01-01T00:00:00Z')`,
		`{"wallet":"`+accountSecret+`"}`)
	mustExec(t, db, `INSERT INTO access_password(id, password_hash, updated_at) VALUES (1, ?, '2026-01-01T00:00:00Z')`,
		passwordSentinel)

	mustExec(t, db, orderInsert, 103, 1, "polymarket", "USDC", nil, "mkt-a", "event-a", "Question A?", "Yes", "token-a", "auto", "settle", "redeem", 5.0, 0.7, 7.14, 0.05, "filled", "filled at venue", "venue-order-103", "2026-02-02T00:00:00Z")
	mustExec(t, db, orderInsert, 101, 1, "polymarket", "USDC", nil, "mkt-a", "event-a", "Question A?", "Yes", "token-a", "manual", "buy", "", 10.5, 0.52, 20.19, 0.1, "filled", "filled at venue", "venue-order-101", "2026-02-01T00:00:00Z")
	mustExec(t, db, orderInsert, 102, 1, "polymarket", "USDC", nil, "mkt-b", "event-b", "Question B?", "No", "token-b", "auto", "buy", "", 3.0, 0.4, 7.5, 0, "pending", "awaiting confirmation", "", "2026-02-02T00:00:00Z")
	mustExec(t, db, orderInsert, 111, 2, "predictfun", "USDT", nil, "mkt-pf", "event-pf", "Question PF?", "Yes", "pf-token", "manual", "buy", "", 2.0, 0.5, 4.0, 0.01, "rejected", "insufficient balance", "venue-order-111", "2026-02-03T00:00:00Z")

	mustExec(t, db, positionInsert, 203, 1, "polymarket", "USDC", "mkt-c", "event-c", "cond-c", "Question C?", "Yes", "token-c", "Yes", "auto", "", 0.45, 0.5, 8.0, 3.6, 0.02, "open", "2026-01-02T00:00:00Z", nil, "2026-12-31T00:00:00Z", 0.0, 0.1)
	mustExec(t, db, positionInsert, 201, 1, "polymarket", "USDC", "mkt-a", "event-a", "cond-a", "Question A?", "Yes", "token-a", "Yes", "manual", "", 0.52, 0.6, 20.0, 10.5, 0.1, "open", "2026-01-01T00:00:00Z", nil, "2026-12-31T00:00:00Z", 0.0, 1.6)
	mustExec(t, db, positionInsert, 202, 1, "polymarket", "USDC", "mkt-b", "event-b", "cond-b", "Question B?", "No", "token-b", "No", "manual", "", 0.4, 1.0, 10.0, 4.0, 0.05, "won", "2026-01-01T00:00:00Z", "2026-01-05T00:00:00Z", "2026-01-05T00:00:00Z", 5.25, 0.0)
	mustExec(t, db, positionInsert, 211, 2, "predictfun", "USDT", "mkt-pf", "event-pf", "cond-pf", "Question PF?", "Yes", "pf-token", "Yes", "manual", "", 0.5, 0.0, 4.0, 2.0, 0.01, "lost", "2026-01-03T00:00:00Z", "2026-01-06T00:00:00Z", "2026-01-06T00:00:00Z", -2.0, 0.0)

	mustExec(t, db, chainInsert, 300, 1, "polymarket", 137, "approve", nil, nil, nil, "0xabc", 7, "0xdeadbeef", "confirmed", "confirmed", "", "", nil, "2026-01-01T00:00:00Z", "2026-01-01T00:01:00Z")
	mustExec(t, db, chainInsert, 301, 1, "polymarket", 137, "redeem", int64(202), "mkt-b", "token-b", "0xdef", 8, "0xtx301", "unknown", "receipt timeout", "operator@example.test", "matched venue tx", "2026-01-04T00:00:00Z", "2026-01-03T00:00:00Z", "2026-01-04T00:00:00Z")
	mustExec(t, db, chainInsert, 311, 2, "predictfun", 56, "redeem", int64(211), "mkt-pf", "pf-token", "0xpf", 9, "0xpf311", "reverted", "reverted on chain", "ops", "gas too low", "2026-01-04T00:00:00Z", "2026-01-03T00:00:00Z", "2026-01-04T00:00:00Z")

	mustExec(t, db, `INSERT INTO equity_snapshots(id, account_id, venue, currency, time, value) VALUES (402, 1, 'polymarket', 'USDC', '2026-01-02T00:00:00Z', 1010.5)`)
	mustExec(t, db, `INSERT INTO equity_snapshots(id, account_id, venue, currency, time, value) VALUES (401, 1, 'polymarket', 'USDC', '2026-01-01T00:00:00Z', 1000.0)`)
	mustExec(t, db, `INSERT INTO equity_snapshots(id, account_id, venue, currency, time, value) VALUES (411, 2, 'predictfun', 'USDT', '2026-01-03T00:00:00Z', 500.0)`)

	var busy, logFrames, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatalf("checkpoint fixture: %v", err)
	}
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode=DELETE`).Scan(&journalMode); err != nil {
		t.Fatalf("switch fixture journal mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "delete") {
		t.Fatalf("journal_mode = %q, want delete", journalMode)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}
	return path
}

const orderInsert = `INSERT INTO orders(
	id, account_id, venue, currency, position_id, market_id, event_id, question, outcome,
	token_id, kind, type, action, amount, price, shares, fee, status, reason, external_id, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const positionInsert = `INSERT INTO positions(
	id, account_id, venue, currency, market_id, event_id, condition_id, question, outcome,
	token_id, side, kind, exit_kind, entry_price, current_price, shares, cost, fee, status,
	opened_at, closed_at, end_date, realized_pnl, unrealized_pnl
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const chainInsert = `INSERT INTO chain_operations(
	id, account_id, venue, chain_id, kind, position_id, market_id, token_id, from_address,
	nonce, tx_hash, status, reason, resolved_by, resolution_note, resolved_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func mustReadDir(t *testing.T, path string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("read dir %s: %v", path, err)
	}
	return entries
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index]
	}
	return value
}
