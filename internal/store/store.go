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
	"time"

	"kagari/internal/domain"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	key TEXT NOT NULL,
	payload BLOB NOT NULL,
	result BLOB,
	target_chat_id INTEGER NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('pending','processing','completed','failed')),
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	next_attempt_at TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE (kind, key)
);
CREATE INDEX IF NOT EXISTS jobs_claim ON jobs(status, next_attempt_at, id);
CREATE INDEX IF NOT EXISTS jobs_analyze_user ON jobs(json_extract(payload,'$.user_id')) WHERE kind='analyze';
CREATE TABLE IF NOT EXISTS deliveries (
	id INTEGER PRIMARY KEY,
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	chat_id INTEGER NOT NULL,
	part INTEGER NOT NULL,
	text TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('pending','sending','sent','failed','uncertain')),
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	next_attempt_at TEXT NOT NULL,
	message_id INTEGER,
	UNIQUE (job_id, part)
);
CREATE INDEX IF NOT EXISTS deliveries_claim ON deliveries(status, next_attempt_at, id);
CREATE INDEX IF NOT EXISTS deliveries_order ON deliveries(job_id, part, status);
CREATE TABLE IF NOT EXISTS source_cache (
	url TEXT PRIMARY KEY,
	source BLOB NOT NULL,
	cached_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS update_state (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	next_offset INTEGER NOT NULL
);
INSERT OR IGNORE INTO update_state(id, next_offset) VALUES (1, 0);
`

// Store uses one SQLite connection so connection-scoped pragmas apply to every
// operation and transactions cannot interleave on the same connection.
// ponytail: this serializes reads too; add connections if measured read throughput needs it.
type Store struct {
	db *sql.DB
}

func New(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: database path is required")
	}
	dsn := path
	if path == ":memory:" {
		dsn += "?_foreign_keys=on&_busy_timeout=5000"
	} else if strings.HasPrefix(path, "file:") {
		u, err := url.Parse(path)
		if err != nil {
			return nil, fmt.Errorf("store: parse database URI: %w", err)
		}
		query := u.Query()
		query.Set("_foreign_keys", "on")
		query.Set("_busy_timeout", "5000")
		u.RawQuery = query.Encode()
		dsn = u.String()
	} else {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("store: resolve database path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0700); err != nil {
			return nil, fmt.Errorf("store: create database directory: %w", err)
		}
		file, err := os.OpenFile(absPath, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("store: create database file: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("store: close database file: %w", err)
		}
		if err := os.Chmod(absPath, 0600); err != nil {
			return nil, fmt.Errorf("store: secure database file: %w", err)
		}
		dsn = (&url.URL{Scheme: "file", Path: absPath, RawQuery: "_foreign_keys=on&_busy_timeout=5000"}).String()
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: connect database: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: initialize schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func validJSON(raw []byte) error {
	if !json.Valid(raw) {
		return errors.New("store: payload must be valid JSON")
	}
	return nil
}

func enqueue(ctx context.Context, tx *sql.Tx, kind, key string, payload []byte, targetChatID int64, now time.Time) (int64, bool, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO jobs(kind,key,payload,target_chat_id,status,next_attempt_at,created_at)
		VALUES(?,?,?,?,'pending',?,?) ON CONFLICT(kind,key) DO NOTHING`,
		kind, key, payload, targetChatID, timestamp(now), timestamp(now))
	if err != nil {
		return 0, false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE kind=? AND key=?`, kind, key).Scan(&id); err != nil {
		return 0, false, err
	}
	return id, count == 1, nil
}

func scanJob(row interface{ Scan(...any) error }) (*domain.Job, error) {
	var job domain.Job
	var payload, result []byte
	if err := row.Scan(&job.ID, &job.Kind, &job.Key, &payload, &result, &job.TargetChatID, &job.Status, &job.Attempts, &job.LastError); err != nil {
		return nil, err
	}
	job.Payload = append(json.RawMessage(nil), payload...)
	job.Result = append(json.RawMessage(nil), result...)
	return &job, nil
}

const jobColumns = `id,kind,key,payload,result,target_chat_id,status,attempts,last_error`
