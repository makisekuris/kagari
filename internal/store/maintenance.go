package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type DatabaseInfo struct {
	Tables             []TableInfo      `json:"tables"`
	JobStatuses        map[string]int64 `json:"job_statuses"`
	DeliveryStatuses   map[string]int64 `json:"delivery_statuses"`
	NextTelegramOffset int64            `json:"next_telegram_offset"`
}

type TableInfo struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

var maintenanceTables = []TableInfo{
	{Name: "jobs"},
	{Name: "deliveries"},
	{Name: "source_cache"},
	{Name: "meta"},
	{Name: "update_state"},
}

// OpenExisting opens only an existing regular database file. It never creates,
// initializes, migrates, chmods, or recovers the database.
func OpenExisting(ctx context.Context, path string, writable bool) (*Store, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, errors.New("store: maintenance requires a regular database path")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve database path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("store: stat database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("store: maintenance path must be a regular file")
	}

	query := url.Values{}
	if writable {
		query.Set("mode", "rw")
	} else {
		query.Set("mode", "ro")
		query.Add("_pragma", "query_only(1)")
	}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	dsn := (&url.URL{Scheme: "file", Path: absPath, RawQuery: query.Encode()}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: connect database: %w", err)
	}
	return &Store{db: db}, nil
}

// DatabaseInfo returns one consistent, read-only snapshot of the known store tables.
func (s *Store) DatabaseInfo(ctx context.Context) (DatabaseInfo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DatabaseInfo{}, err
	}
	defer tx.Rollback()

	info := DatabaseInfo{
		Tables:           append([]TableInfo(nil), maintenanceTables...),
		JobStatuses:      make(map[string]int64),
		DeliveryStatuses: make(map[string]int64),
	}
	for i := range info.Tables {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+info.Tables[i].Name).Scan(&info.Tables[i].Rows); err != nil {
			return DatabaseInfo{}, err
		}
	}
	if err := readStatusCounts(ctx, tx, "jobs", info.JobStatuses); err != nil {
		return DatabaseInfo{}, err
	}
	if err := readStatusCounts(ctx, tx, "deliveries", info.DeliveryStatuses); err != nil {
		return DatabaseInfo{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT next_offset FROM update_state WHERE id=1`).Scan(&info.NextTelegramOffset); err != nil {
		return DatabaseInfo{}, err
	}
	if err := tx.Commit(); err != nil {
		return DatabaseInfo{}, err
	}
	return info, nil
}

func readStatusCounts(ctx context.Context, tx *sql.Tx, table string, counts map[string]int64) error {
	rows, err := tx.QueryContext(ctx, "SELECT status,count(*) FROM "+table+" GROUP BY status ORDER BY status")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return err
		}
		counts[status] = count
	}
	return rows.Err()
}

// TableRows reads a page from one of the five fixed store tables, never arbitrary SQL.
func (s *Store) TableRows(ctx context.Context, table string, limit, offset int) ([]map[string]any, error) {
	if !knownMaintenanceTable(table) {
		return nil, fmt.Errorf("store: unsupported maintenance table %q", table)
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, errors.New("store: table row limit must be 1..100 and offset non-negative")
	}
	query := "SELECT * FROM " + table + " ORDER BY " + maintenanceOrder(table) + " LIMIT ? OFFSET ?"
	rows, err := s.db.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = maintenanceValue(values[i])
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func knownMaintenanceTable(table string) bool {
	for _, info := range maintenanceTables {
		if info.Name == table {
			return true
		}
	}
	return false
}

func maintenanceOrder(table string) string {
	switch table {
	case "jobs", "deliveries":
		return "id DESC"
	case "source_cache":
		return "cached_at DESC,url ASC"
	case "meta":
		return "key ASC"
	default: // update_state
		return "id ASC"
	}
}

func maintenanceValue(value any) any {
	data, ok := value.([]byte)
	if !ok {
		return value // includes SQL NULL, which remains JSON null
	}
	if json.Valid(data) {
		return json.RawMessage(append([]byte(nil), data...))
	}
	return string(data)
}

// Clear removes selected persisted data. Callers must exclude active processors
// and senders first so concurrent work cannot repopulate or race the maintenance action.
func (s *Store) Clear(ctx context.Context, scope string) (map[string]int64, error) {
	var tables []string
	switch scope {
	case "cache":
		tables = []string{"source_cache"}
	case "data":
		tables = []string{"deliveries", "jobs", "source_cache"}
	case "all":
		tables = []string{"deliveries", "jobs", "source_cache", "meta"}
	default:
		return nil, fmt.Errorf("store: unsupported clear scope %q", scope)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	deleted := make(map[string]int64, len(tables))
	for _, table := range tables {
		result, err := tx.ExecContext(ctx, "DELETE FROM "+table)
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		deleted[table] = count
	}
	if scope == "all" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO update_state(id,next_offset) VALUES(1,0)
			ON CONFLICT(id) DO UPDATE SET next_offset=excluded.next_offset`); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return deleted, nil
}
