// Command ledger-export writes a read-only JSON snapshot of the local ledger.
//
// It is intended for reconciliation against a venue export. The command never
// opens the database writable, never reads settings or credential tables, and
// never calls a venue API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const defaultDatabasePath = "data/pennyharvester.db"

type options struct {
	databasePath string
	accountID    int64
	outPath      string
	pretty       bool
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ledger-export:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	return exportTo(ctx, opts, stdout)
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("ledger-export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ledger-export [-db <path>] [-account <id>] [-out <file>] [-pretty]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Write a read-only, JSON ledger snapshot for reconciliation.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}

	var opts options
	fs.StringVar(&opts.databasePath, "db", defaultDatabasePath, "SQLite database path (opened read-only)")
	fs.Int64Var(&opts.accountID, "account", 0, "only export this account id (0 exports all accounts)")
	fs.StringVar(&opts.outPath, "out", "", "write JSON to this file atomically (default: stdout)")
	fs.BoolVar(&opts.pretty, "pretty", false, "indent JSON output")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(opts.databasePath) == "" {
		return options{}, errors.New("database path is empty")
	}
	if opts.accountID < 0 {
		return options{}, fmt.Errorf("account id must be >= 0, got %d", opts.accountID)
	}
	return opts, nil
}

func exportTo(ctx context.Context, opts options, stdout io.Writer) error {
	if opts.outPath != "" {
		if err := ensureDistinctOutput(opts.databasePath, opts.outPath); err != nil {
			return err
		}
	}

	ledger, err := buildExport(ctx, opts.databasePath, opts.accountID)
	if err != nil {
		return err
	}
	if opts.outPath == "" {
		return encodeExport(stdout, ledger, opts.pretty)
	}
	return writeExportAtomically(opts.outPath, ledger, opts.pretty)
}

// ensureDistinctOutput refuses a destination that would replace the database
// after it was read. The export is read-only; tearing down its source would be
// the opposite of reconciliation.
func ensureDistinctOutput(databasePath, outPath string) error {
	dbAbs, err := filepath.Abs(databasePath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	outAbs, err := filepath.Abs(outPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	if filepath.Clean(dbAbs) == filepath.Clean(outAbs) {
		return errors.New("-out must not be the database file")
	}
	return nil
}
