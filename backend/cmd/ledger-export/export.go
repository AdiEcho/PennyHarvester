package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	exportSchemaVersion = 1
	readOnlyQuery       = "mode=ro&_pragma=query_only(1)"
)

// LedgerExport is the versioned top-level JSON document. It intentionally
// contains ledger rows only: no settings, credentials, cash totals or derived
// performance numbers.
type LedgerExport struct {
	SchemaVersion int             `json:"schema_version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Database      string          `json:"database"`
	Accounts      []AccountExport `json:"accounts"`
}

type AccountExport struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Mode           string  `json:"mode"`
	Venue          string  `json:"venue"`
	Currency       string  `json:"currency"`
	IsDefault      bool    `json:"is_default"`
	AutoTrade      bool    `json:"auto_trade"`
	BaselineEquity float64 `json:"baseline_equity"`
	CreatedAt      string  `json:"created_at"`

	StatusCounts    StatusCounts           `json:"status_counts"`
	Orders          []OrderExport          `json:"orders"`
	Positions       []PositionExport       `json:"positions"`
	ChainOperations []ChainOperationExport `json:"chain_operations"`
	EquitySnapshots []EquitySnapshotExport `json:"equity_snapshots"`
}

type StatusCounts struct {
	Orders          map[string]int `json:"orders"`
	Positions       map[string]int `json:"positions"`
	ChainOperations map[string]int `json:"chain_operations"`
}

type OrderExport struct {
	ID         int64   `json:"id"`
	AccountID  int64   `json:"account_id"`
	Venue      string  `json:"venue"`
	Currency   string  `json:"currency"`
	PositionID *int64  `json:"position_id"`
	MarketID   string  `json:"market_id"`
	EventID    string  `json:"event_id"`
	Question   string  `json:"question"`
	Outcome    string  `json:"outcome"`
	TokenID    string  `json:"token_id"`
	Kind       string  `json:"kind"`
	Type       string  `json:"type"`
	Action     string  `json:"action"`
	Amount     float64 `json:"amount"`
	Price      float64 `json:"price"`
	Shares     float64 `json:"shares"`
	Fee        float64 `json:"fee"`
	Status     string  `json:"status"`
	Reason     string  `json:"reason"`
	ExternalID string  `json:"external_id"`
	CreatedAt  string  `json:"created_at"`
}

type PositionExport struct {
	ID            int64   `json:"id"`
	AccountID     int64   `json:"account_id"`
	Venue         string  `json:"venue"`
	Currency      string  `json:"currency"`
	MarketID      string  `json:"market_id"`
	EventID       string  `json:"event_id"`
	ConditionID   string  `json:"condition_id"`
	Question      string  `json:"question"`
	Outcome       string  `json:"outcome"`
	TokenID       string  `json:"token_id"`
	Side          string  `json:"side"`
	Kind          string  `json:"kind"`
	ExitKind      string  `json:"exit_kind"`
	EntryPrice    float64 `json:"entry_price"`
	CurrentPrice  float64 `json:"current_price"`
	Shares        float64 `json:"shares"`
	Cost          float64 `json:"cost"`
	Fee           float64 `json:"fee"`
	Status        string  `json:"status"`
	OpenedAt      string  `json:"opened_at"`
	ClosedAt      *string `json:"closed_at"`
	EndDate       string  `json:"end_date"`
	RealizedPNL   float64 `json:"realized_pnl"`
	UnrealizedPNL float64 `json:"unrealized_pnl"`
}

type ChainOperationExport struct {
	ID             int64   `json:"id"`
	AccountID      int64   `json:"account_id"`
	Venue          string  `json:"venue"`
	ChainID        int64   `json:"chain_id"`
	Kind           string  `json:"kind"`
	PositionID     *int64  `json:"position_id"`
	MarketID       string  `json:"market_id"`
	TokenID        string  `json:"token_id"`
	FromAddress    string  `json:"from_address"`
	Nonce          *int64  `json:"nonce"`
	TxHash         string  `json:"tx_hash"`
	Status         string  `json:"status"`
	Reason         string  `json:"reason"`
	ResolvedBy     string  `json:"resolved_by"`
	ResolutionNote string  `json:"resolution_note"`
	ResolvedAt     *string `json:"resolved_at"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type EquitySnapshotExport struct {
	ID        int64   `json:"id"`
	AccountID int64   `json:"account_id"`
	Venue     string  `json:"venue"`
	Currency  string  `json:"currency"`
	Time      string  `json:"time"`
	Value     float64 `json:"value"`
}

func encodeExport(w io.Writer, ledger *LedgerExport, pretty bool) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(ledger); err != nil {
		return fmt.Errorf("encode ledger export: %w", err)
	}
	return nil
}

// writeExportAtomically writes into the destination directory and renames only
// after the complete document has been flushed. A failed export never leaves a
// truncated file at the requested path.
func writeExportAtomically(path string, ledger *LedgerExport, pretty bool) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	if info, err := os.Stat(absPath); err == nil && info.IsDir() {
		return fmt.Errorf("output path %q is a directory", absPath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat output path %q: %w", absPath, err)
	}

	dir := filepath.Dir(absPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(absPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary export file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Remove is a no-op after a successful rename; it cleans up every error
	// path before the atomic replacement.
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("set export file permissions: %w", err)
	}
	if err := encodeExport(tmp, ledger, pretty); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flush export file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close export file: %w", err)
	}
	if err := os.Rename(tmpName, absPath); err != nil {
		return fmt.Errorf("replace %q: %w", absPath, err)
	}
	return nil
}

func buildExport(ctx context.Context, databasePath string, accountID int64) (*LedgerExport, error) {
	db, absPath, err := openReadOnly(ctx, databasePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// All reads share one deferred transaction so accounts, orders and
	// positions come from one SQLite snapshot. Without it a live writer could
	// land between two SELECTs and produce a document that never existed.
	if _, err := db.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, fmt.Errorf("begin read snapshot: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	schema, err := inspectSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	accounts, err := loadAccounts(ctx, db, schema, accountID)
	if err != nil {
		return nil, err
	}
	if accountID != 0 && len(accounts) == 0 {
		return nil, fmt.Errorf("account %d not found in database %q", accountID, filepath.Base(absPath))
	}

	ledger := &LedgerExport{
		SchemaVersion: exportSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Database:      filepath.Base(absPath),
		Accounts:      accounts,
	}
	for i := range ledger.Accounts {
		account := &ledger.Accounts[i]
		orders, err := loadOrders(ctx, db, schema, account.ID)
		if err != nil {
			return nil, err
		}
		positions, err := loadPositions(ctx, db, schema, account.ID)
		if err != nil {
			return nil, err
		}
		chainOperations, err := loadChainOperations(ctx, db, schema, account.ID)
		if err != nil {
			return nil, err
		}
		snapshots, err := loadEquitySnapshots(ctx, db, schema, account.ID)
		if err != nil {
			return nil, err
		}

		account.Orders = orders
		account.Positions = positions
		account.ChainOperations = chainOperations
		account.EquitySnapshots = snapshots
		account.StatusCounts = StatusCounts{
			Orders:          countStatuses(orders, func(order OrderExport) string { return order.Status }),
			Positions:       countStatuses(positions, func(position PositionExport) string { return position.Status }),
			ChainOperations: countStatuses(chainOperations, func(operation ChainOperationExport) string { return operation.Status }),
		}
	}
	return ledger, nil
}

func countStatuses[T any](items []T, statusOf func(T) string) map[string]int {
	counts := make(map[string]int)
	for _, item := range items {
		counts[statusOf(item)]++
	}
	return counts
}

// openReadOnly opens an existing database with mode=ro and query_only=1, then
// verifies that the connection really is read-only. The absolute path is
// returned so the export can report the database basename deterministically.
func openReadOnly(ctx context.Context, databasePath string) (*sql.DB, string, error) {
	absPath, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, "", fmt.Errorf("resolve database path: %w", err)
	}
	info, err := os.Stat(absPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("database file %q does not exist", absPath)
	}
	if err != nil {
		return nil, "", fmt.Errorf("stat database file %q: %w", absPath, err)
	}
	if info.IsDir() {
		return nil, "", fmt.Errorf("database path %q is a directory", absPath)
	}

	dsn := (&url.URL{Scheme: "file", Path: absPath, RawQuery: readOnlyQuery}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, "", fmt.Errorf("open database %q read-only: %w", absPath, err)
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, "", fmt.Errorf("open database %q read-only: %w", absPath, err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA query_only = 1"); err != nil {
		db.Close()
		return nil, "", fmt.Errorf("enable query_only on %q: %w", absPath, err)
	}
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		db.Close()
		return nil, "", fmt.Errorf("verify query_only on %q: %w", absPath, err)
	}
	if queryOnly != 1 {
		db.Close()
		return nil, "", fmt.Errorf("database %q was not opened read-only", absPath)
	}
	return db, absPath, nil
}

type rowValues map[string]any

type columnSet map[string]struct{}

func (c columnSet) has(name string) bool {
	_, ok := c[name]
	return ok
}

type databaseSchema struct {
	columns map[string]columnSet
}

func inspectSchema(ctx context.Context, db *sql.DB) (*databaseSchema, error) {
	schema := &databaseSchema{columns: make(map[string]columnSet)}
	for _, table := range []string{"accounts", "orders", "positions", "chain_operations", "equity_snapshots"} {
		columns, err := tableColumns(ctx, db, table)
		if err != nil {
			return nil, err
		}
		schema.columns[table] = columns
	}
	return schema, nil
}

func tableColumns(ctx context.Context, db *sql.DB, table string) (columnSet, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+quoteIdent(table)+")")
	if err != nil {
		return nil, fmt.Errorf("inspect table %q: %w", table, err)
	}
	defer rows.Close()

	columns := make(columnSet)
	for rows.Next() {
		var (
			cid          int
			name         string
			kind         string
			notNull      int
			defaultValue any
			primaryKey   int
		)
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("inspect table %q: %w", table, err)
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspect table %q: %w", table, err)
	}
	return columns, nil
}

// columnsFor returns the table's columns when it exists. Optional tables are
// allowed to be absent so an older ledger can still be exported.
func (s *databaseSchema) columnsFor(table string, required bool, requiredColumns ...string) (columnSet, error) {
	columns, ok := s.columns[table]
	if !ok || len(columns) == 0 {
		if required {
			return nil, fmt.Errorf("required table %q is missing from the database", table)
		}
		return nil, nil
	}
	for _, name := range requiredColumns {
		if !columns.has(name) {
			return nil, fmt.Errorf("table %q is missing required column %q", table, name)
		}
	}
	return columns, nil
}

func selectRows(
	ctx context.Context,
	db *sql.DB,
	schema *databaseSchema,
	table string,
	required bool,
	requiredColumns []string,
	wantedColumns []string,
	orderBy string,
	accountID int64,
) ([]rowValues, error) {
	columns, err := schema.columnsFor(table, required, requiredColumns...)
	if err != nil {
		return nil, err
	}
	if columns == nil {
		return nil, nil
	}
	selected := make([]string, 0, len(wantedColumns))
	for _, name := range wantedColumns {
		if columns.has(name) {
			selected = append(selected, quoteIdent(name))
		}
	}
	query := "SELECT " + strings.Join(selected, ", ") + " FROM " + quoteIdent(table) +
		" WHERE " + quoteIdent("account_id") + " = ? ORDER BY " + orderBy
	rows, err := queryRows(ctx, db, query, accountID)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", table, err)
	}
	return rows, nil
}

func queryRows(ctx context.Context, db *sql.DB, query string, args ...any) ([]rowValues, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := make([]rowValues, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, err
		}
		row := make(rowValues, len(columns))
		for i, name := range columns {
			row[name] = values[i]
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func loadAccounts(ctx context.Context, db *sql.DB, schema *databaseSchema, accountID int64) ([]AccountExport, error) {
	columns, err := schema.columnsFor("accounts", true,
		"id", "name", "mode", "is_default", "auto_trade", "baseline_equity", "created_at")
	if err != nil {
		return nil, err
	}
	wanted := []string{
		"id", "name", "mode", "venue", "currency", "is_default", "auto_trade",
		"baseline_equity", "created_at",
	}
	selected := make([]string, 0, len(wanted))
	for _, name := range wanted {
		if columns.has(name) {
			selected = append(selected, quoteIdent(name))
		}
	}
	query := "SELECT " + strings.Join(selected, ", ") + " FROM " + quoteIdent("accounts")
	args := []any{}
	if accountID != 0 {
		query += " WHERE " + quoteIdent("id") + " = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY " + quoteIdent("id") + " ASC"

	rows, err := queryRows(ctx, db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load accounts: %w", err)
	}
	accounts := make([]AccountExport, 0, len(rows))
	for _, row := range rows {
		venue := effectiveVenue(stringValue(row, "venue"))
		currency := strings.TrimSpace(stringValue(row, "currency"))
		if currency == "" {
			currency = currencyForVenue(venue)
		}
		accounts = append(accounts, AccountExport{
			ID:             intValue(row, "id"),
			Name:           stringValue(row, "name"),
			Mode:           stringValue(row, "mode"),
			Venue:          venue,
			Currency:       currency,
			IsDefault:      intValue(row, "is_default") != 0,
			AutoTrade:      intValue(row, "auto_trade") != 0,
			BaselineEquity: floatValue(row, "baseline_equity"),
			CreatedAt:      stringValue(row, "created_at"),
		})
	}
	return accounts, nil
}

func loadOrders(ctx context.Context, db *sql.DB, schema *databaseSchema, accountID int64) ([]OrderExport, error) {
	rows, err := selectRows(ctx, db, schema, "orders", true,
		[]string{"id", "account_id", "market_id", "question", "outcome", "token_id", "kind", "type", "amount", "price", "shares", "status", "reason", "created_at"},
		[]string{
			"id", "account_id", "venue", "currency", "position_id", "market_id", "event_id",
			"question", "outcome", "token_id", "kind", "type", "action", "amount", "price",
			"shares", "fee", "status", "reason", "external_id", "created_at",
		},
		`julianday("created_at") ASC, "id" ASC`, accountID)
	if err != nil {
		return nil, err
	}
	orders := make([]OrderExport, 0, len(rows))
	for _, row := range rows {
		venue := effectiveVenue(stringValue(row, "venue"))
		currency := strings.TrimSpace(stringValue(row, "currency"))
		if currency == "" {
			currency = currencyForVenue(venue)
		}
		orders = append(orders, OrderExport{
			ID:         intValue(row, "id"),
			AccountID:  intValue(row, "account_id"),
			Venue:      venue,
			Currency:   currency,
			PositionID: nullableInt64(row, "position_id"),
			MarketID:   stringValue(row, "market_id"),
			EventID:    stringValue(row, "event_id"),
			Question:   stringValue(row, "question"),
			Outcome:    stringValue(row, "outcome"),
			TokenID:    stringValue(row, "token_id"),
			Kind:       stringValue(row, "kind"),
			Type:       stringValue(row, "type"),
			Action:     stringValue(row, "action"),
			Amount:     floatValue(row, "amount"),
			Price:      floatValue(row, "price"),
			Shares:     floatValue(row, "shares"),
			Fee:        floatValue(row, "fee"),
			Status:     stringValue(row, "status"),
			Reason:     stringValue(row, "reason"),
			ExternalID: stringValue(row, "external_id"),
			CreatedAt:  stringValue(row, "created_at"),
		})
	}
	return orders, nil
}

func loadPositions(ctx context.Context, db *sql.DB, schema *databaseSchema, accountID int64) ([]PositionExport, error) {
	rows, err := selectRows(ctx, db, schema, "positions", true,
		[]string{"id", "account_id", "market_id", "condition_id", "question", "outcome", "token_id", "side", "entry_price", "current_price", "shares", "cost", "fee", "status", "opened_at", "end_date", "realized_pnl", "unrealized_pnl"},
		[]string{
			"id", "account_id", "venue", "currency", "market_id", "event_id", "condition_id",
			"question", "outcome", "token_id", "side", "kind", "exit_kind", "entry_price",
			"current_price", "shares", "cost", "fee", "status", "opened_at", "closed_at",
			"end_date", "realized_pnl", "unrealized_pnl",
		},
		`julianday("opened_at") ASC, "id" ASC`, accountID)
	if err != nil {
		return nil, err
	}
	positions := make([]PositionExport, 0, len(rows))
	for _, row := range rows {
		venue := effectiveVenue(stringValue(row, "venue"))
		currency := strings.TrimSpace(stringValue(row, "currency"))
		if currency == "" {
			currency = currencyForVenue(venue)
		}
		positions = append(positions, PositionExport{
			ID:            intValue(row, "id"),
			AccountID:     intValue(row, "account_id"),
			Venue:         venue,
			Currency:      currency,
			MarketID:      stringValue(row, "market_id"),
			EventID:       stringValue(row, "event_id"),
			ConditionID:   stringValue(row, "condition_id"),
			Question:      stringValue(row, "question"),
			Outcome:       stringValue(row, "outcome"),
			TokenID:       stringValue(row, "token_id"),
			Side:          stringValue(row, "side"),
			Kind:          stringValue(row, "kind"),
			ExitKind:      stringValue(row, "exit_kind"),
			EntryPrice:    floatValue(row, "entry_price"),
			CurrentPrice:  floatValue(row, "current_price"),
			Shares:        floatValue(row, "shares"),
			Cost:          floatValue(row, "cost"),
			Fee:           floatValue(row, "fee"),
			Status:        stringValue(row, "status"),
			OpenedAt:      stringValue(row, "opened_at"),
			ClosedAt:      nullableString(row, "closed_at"),
			EndDate:       stringValue(row, "end_date"),
			RealizedPNL:   floatValue(row, "realized_pnl"),
			UnrealizedPNL: floatValue(row, "unrealized_pnl"),
		})
	}
	return positions, nil
}

func loadChainOperations(ctx context.Context, db *sql.DB, schema *databaseSchema, accountID int64) ([]ChainOperationExport, error) {
	rows, err := selectRows(ctx, db, schema, "chain_operations", false,
		[]string{"id", "account_id", "chain_id", "kind", "from_address", "status", "created_at", "updated_at"},
		[]string{
			"id", "account_id", "venue", "chain_id", "kind", "position_id", "market_id",
			"token_id", "from_address", "nonce", "tx_hash", "status", "reason", "resolved_by",
			"resolution_note", "resolved_at", "created_at", "updated_at",
		},
		`julianday("created_at") ASC, "id" ASC`, accountID)
	if err != nil {
		return nil, err
	}
	operations := make([]ChainOperationExport, 0, len(rows))
	for _, row := range rows {
		operations = append(operations, ChainOperationExport{
			ID:             intValue(row, "id"),
			AccountID:      intValue(row, "account_id"),
			Venue:          effectiveVenue(stringValue(row, "venue")),
			ChainID:        intValue(row, "chain_id"),
			Kind:           stringValue(row, "kind"),
			PositionID:     nullableInt64(row, "position_id"),
			MarketID:       stringValue(row, "market_id"),
			TokenID:        stringValue(row, "token_id"),
			FromAddress:    stringValue(row, "from_address"),
			Nonce:          nullableInt64(row, "nonce"),
			TxHash:         stringValue(row, "tx_hash"),
			Status:         stringValue(row, "status"),
			Reason:         stringValue(row, "reason"),
			ResolvedBy:     stringValue(row, "resolved_by"),
			ResolutionNote: stringValue(row, "resolution_note"),
			ResolvedAt:     nullableString(row, "resolved_at"),
			CreatedAt:      stringValue(row, "created_at"),
			UpdatedAt:      stringValue(row, "updated_at"),
		})
	}
	return operations, nil
}

func loadEquitySnapshots(ctx context.Context, db *sql.DB, schema *databaseSchema, accountID int64) ([]EquitySnapshotExport, error) {
	rows, err := selectRows(ctx, db, schema, "equity_snapshots", false,
		[]string{"id", "account_id", "time", "value"},
		[]string{"id", "account_id", "venue", "currency", "time", "value"},
		`julianday("time") ASC, "id" ASC`, accountID)
	if err != nil {
		return nil, err
	}
	snapshots := make([]EquitySnapshotExport, 0, len(rows))
	for _, row := range rows {
		venue := effectiveVenue(stringValue(row, "venue"))
		currency := strings.TrimSpace(stringValue(row, "currency"))
		if currency == "" {
			currency = currencyForVenue(venue)
		}
		snapshots = append(snapshots, EquitySnapshotExport{
			ID:        intValue(row, "id"),
			AccountID: intValue(row, "account_id"),
			Venue:     venue,
			Currency:  currency,
			Time:      stringValue(row, "time"),
			Value:     floatValue(row, "value"),
		})
	}
	return snapshots, nil
}

func effectiveVenue(venue string) string {
	venue = strings.TrimSpace(venue)
	if venue == "" {
		return "polymarket"
	}
	return venue
}

func currencyForVenue(venue string) string {
	if strings.EqualFold(venue, "predictfun") {
		return "USDT"
	}
	return "USDC"
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func intValue(row rowValues, name string) int64 {
	switch value := row[name].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	case bool:
		if value {
			return 1
		}
		return 0
	case []byte:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(string(value)), 10, 64)
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		return parsed
	default:
		return 0
	}
}

func floatValue(row rowValues, name string) float64 {
	switch value := row[name].(type) {
	case float64:
		return value
	case int64:
		return float64(value)
	case int:
		return float64(value)
	case []byte:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(string(value)), 64)
		return parsed
	case string:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed
	default:
		return 0
	}
}

func stringValue(row rowValues, name string) string {
	switch value := row[name].(type) {
	case string:
		return value
	case []byte:
		return string(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case int:
		return strconv.Itoa(value)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	default:
		return ""
	}
}

func nullableInt64(row rowValues, name string) *int64 {
	if row[name] == nil {
		return nil
	}
	value := intValue(row, name)
	return &value
}

func nullableString(row rowValues, name string) *string {
	if row[name] == nil {
		return nil
	}
	value := stringValue(row, name)
	return &value
}
