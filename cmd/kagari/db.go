package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"kagari/internal/app"
	"kagari/internal/store"
)

var dbTables = map[string]struct{}{
	"jobs": {}, "deliveries": {}, "source_cache": {}, "meta": {}, "update_state": {},
}

type dbInfoOutput struct {
	Path string `json:"path"`
	store.DatabaseInfo
}

type dbListOutput struct {
	Path   string           `json:"path"`
	Table  string           `json:"table"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
	Rows   []map[string]any `json:"rows"`
}

type dbClearOutput struct {
	Path                string           `json:"path"`
	Scope               string           `json:"scope"`
	Deleted             map[string]int64 `json:"deleted"`
	TelegramOffsetReset bool             `json:"telegram_offset_reset"`
}

func runDB(ctx context.Context, path string, args []string, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		return errors.New("database command output is required")
	}
	if len(args) == 0 {
		return errors.New("usage: kagari db {info|list|clear}")
	}
	if isDBHelp(args[0]) && len(args) == 1 {
		return writeDBHelp(out, "")
	}
	switch args[0] {
	case "info":
		if len(args) == 2 && isDBHelp(args[1]) {
			return writeDBHelp(out, "info")
		}
		if len(args) != 1 {
			return errors.New("usage: kagari db info")
		}
		return runDBInfo(ctx, path, out)
	case "list":
		return runDBList(ctx, path, args[1:], out)
	case "clear":
		return runDBClear(ctx, path, args[1:], out)
	default:
		return fmt.Errorf("unknown db command %q; usage: kagari db {info|list|clear}", args[0])
	}
}

func runDBInfo(ctx context.Context, path string, out io.Writer) error {
	absolute, err := absoluteDBPath(path)
	if err != nil {
		return err
	}
	db, err := store.OpenExisting(ctx, path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	info, err := db.DatabaseInfo(ctx)
	if err != nil {
		return err
	}
	return encodeDBJSON(out, dbInfoOutput{Path: absolute, DatabaseInfo: info})
}

func runDBList(ctx context.Context, path string, args []string, out io.Writer) error {
	limit, offset := 20, 0
	flags := flag.NewFlagSet("db list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.IntVar(&limit, "limit", 20, "maximum rows (1..100)")
	flags.IntVar(&offset, "offset", 0, "number of rows to skip")
	flags.Usage = func() { _ = writeDBHelp(out, "list") }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	positionals := flags.Args()
	if len(positionals) != 1 {
		return errors.New("usage: kagari db list [-limit 20] [-offset 0] TABLE")
	}
	table := positionals[0]
	if _, ok := dbTables[table]; !ok {
		return errors.New("table must be one of jobs, deliveries, source_cache, meta, update_state")
	}
	if limit < 1 || limit > 100 {
		return errors.New("limit must be between 1 and 100")
	}
	if offset < 0 {
		return errors.New("offset must be nonnegative")
	}
	absolute, err := absoluteDBPath(path)
	if err != nil {
		return err
	}
	db, err := store.OpenExisting(ctx, path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.TableRows(ctx, table, limit, offset)
	if err != nil {
		return err
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	return encodeDBJSON(out, dbListOutput{Path: absolute, Table: table, Limit: limit, Offset: offset, Rows: rows})
}

func runDBClear(ctx context.Context, path string, args []string, out io.Writer) error {
	scope, yes := "data", false
	flags := flag.NewFlagSet("db clear", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&scope, "scope", "data", "clear scope: cache, data, or all")
	flags.BoolVar(&yes, "yes", false, "confirm irreversible deletion")
	flags.Usage = func() { _ = writeDBHelp(out, "clear") }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("usage: kagari db clear [-scope cache|data|all] -yes")
	}
	if scope != "cache" && scope != "data" && scope != "all" {
		return errors.New("scope must be cache, data, or all")
	}
	if !yes {
		return errors.New("clearing is irreversible; inspect with `kagari db info` first, then pass -yes")
	}
	absolute, err := absoluteDBPath(path)
	if err != nil {
		return err
	}
	file, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("database file must already exist: %w", err)
	}
	if !file.Mode().IsRegular() {
		return errors.New("database path must be an existing regular file")
	}
	unlock, err := app.Lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	db, err := store.OpenExisting(ctx, path, true)
	if err != nil {
		return err
	}
	defer db.Close()
	deleted, err := db.Clear(ctx, scope)
	if err != nil {
		return err
	}
	return encodeDBJSON(out, dbClearOutput{
		Path:                absolute,
		Scope:               scope,
		Deleted:             deleted,
		TelegramOffsetReset: scope == "all",
	})
}

func absoluteDBPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("database path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("could not resolve database path")
	}
	return abs, nil
}

func encodeDBJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func isDBHelp(arg string) bool { return arg == "-h" || arg == "-help" || arg == "--help" }

func writeDBHelp(out io.Writer, command string) error {
	var usage string
	switch command {
	case "info":
		usage = "usage: kagari db info\n"
	case "list":
		usage = "usage: kagari db list [-limit 20] [-offset 0] TABLE\n"
	case "clear":
		usage = "usage: kagari db clear [-scope cache|data|all] -yes\n"
	default:
		usage = "usage: kagari db {info|list|clear}\n"
	}
	_, err := io.WriteString(out, usage)
	return err
}
