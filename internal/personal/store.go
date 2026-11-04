package personal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const personalSchema = `
CREATE TABLE IF NOT EXISTS personal_state (
	id INTEGER PRIMARY KEY CHECK(id=1),
	next_offset INTEGER NOT NULL CHECK(next_offset>=0)
);
INSERT OR IGNORE INTO personal_state(id,next_offset) VALUES(1,0);
CREATE TABLE IF NOT EXISTS personal_meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS personal_tasks (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	goal TEXT NOT NULL,
	context TEXT NOT NULL,
	revision INTEGER NOT NULL CHECK(revision>0),
	status TEXT NOT NULL CHECK(status IN ('queued','running','awaiting_review','waiting_input','waiting_budget','completed','failed','cancelled')),
	summary TEXT NOT NULL DEFAULT '',
	checkpoint TEXT NOT NULL DEFAULT '',
	budget_ms INTEGER NOT NULL CHECK(budget_ms>0),
	used_ms INTEGER NOT NULL DEFAULT 0 CHECK(used_ms>=0),
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS personal_tasks_list ON personal_tasks(updated_at DESC,id DESC);
CREATE TABLE IF NOT EXISTS personal_runs (
	id INTEGER PRIMARY KEY,
	task_id INTEGER REFERENCES personal_tasks(id),
	revision INTEGER NOT NULL DEFAULT 0,
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	kind TEXT NOT NULL CHECK(kind IN ('task','memory_review')),
	input TEXT NOT NULL,
	status TEXT NOT NULL CHECK(status IN ('queued','running','completed','failed','cancelled')),
	result TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	elapsed_ms INTEGER NOT NULL DEFAULT 0 CHECK(elapsed_ms>=0),
	created_at TEXT NOT NULL,
	CHECK((kind='task' AND task_id IS NOT NULL AND revision>0) OR (kind='memory_review' AND task_id IS NULL AND revision=0))
);
CREATE INDEX IF NOT EXISTS personal_runs_claim ON personal_runs(kind,status,id);
CREATE INDEX IF NOT EXISTS personal_runs_task ON personal_runs(task_id,revision,status);
CREATE UNIQUE INDEX IF NOT EXISTS personal_runs_memory_review ON personal_runs(event_id) WHERE kind='memory_review';
CREATE TABLE IF NOT EXISTS personal_events (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL CHECK(kind IN ('user','material','task')),
	text TEXT NOT NULL,
	task_id INTEGER REFERENCES personal_tasks(id),
	run_id INTEGER REFERENCES personal_runs(id),
	message_id INTEGER,
	reply_to_id INTEGER,
	status TEXT NOT NULL CHECK(status IN ('queued','running','completed')),
	interrupted INTEGER NOT NULL DEFAULT 0 CHECK(interrupted IN (0,1)),
	elapsed_ms INTEGER NOT NULL DEFAULT 0 CHECK(elapsed_ms>=0),
	created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS personal_events_message ON personal_events(message_id) WHERE message_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS personal_events_run ON personal_events(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS personal_events_claim ON personal_events(status,id);
CREATE TABLE IF NOT EXISTS personal_history (
	id INTEGER PRIMARY KEY,
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	role TEXT NOT NULL,
	text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS personal_history_event ON personal_history(event_id,id);
CREATE TABLE IF NOT EXISTS personal_requests (
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	request_key TEXT NOT NULL,
	operation TEXT NOT NULL,
	payload TEXT NOT NULL,
	task_id INTEGER REFERENCES personal_tasks(id),
	run_id INTEGER REFERENCES personal_runs(id),
	memory_history_id INTEGER REFERENCES personal_memory_history(id),
	CHECK((task_id IS NOT NULL AND run_id IS NOT NULL AND memory_history_id IS NULL) OR
		(task_id IS NULL AND run_id IS NULL AND memory_history_id IS NOT NULL)),
	PRIMARY KEY(event_id,request_key)
);
CREATE TABLE IF NOT EXISTS personal_memories (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	version INTEGER NOT NULL CHECK(version>0),
	source_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	deleted INTEGER NOT NULL CHECK(deleted IN (0,1)),
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS personal_memories_source ON personal_memories(source_event_id);
CREATE TABLE IF NOT EXISTS personal_memory_history (
	id INTEGER PRIMARY KEY,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	version INTEGER NOT NULL CHECK(version>0),
	source_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	run_id INTEGER REFERENCES personal_runs(id),
	deleted INTEGER NOT NULL CHECK(deleted IN (0,1)),
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS personal_memory_history_key ON personal_memory_history(key,version DESC);
CREATE INDEX IF NOT EXISTS personal_memory_history_recent ON personal_memory_history(id DESC);
CREATE TABLE IF NOT EXISTS personal_outbox (
	id INTEGER PRIMARY KEY,
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	part INTEGER NOT NULL CHECK(part>0),
	text TEXT NOT NULL,
	status TEXT NOT NULL CHECK(status IN ('pending','sending','sent','failed','uncertain')),
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt_at TEXT NOT NULL,
	receipt INTEGER,
	detail TEXT NOT NULL DEFAULT '',
	UNIQUE(event_id,part)
);
CREATE INDEX IF NOT EXISTS personal_outbox_claim ON personal_outbox(status,next_attempt_at,id);
CREATE UNIQUE INDEX IF NOT EXISTS personal_outbox_receipt ON personal_outbox(receipt) WHERE receipt IS NOT NULL;
`

// Store serializes operations through one SQLite connection, including claims and transactions.
type Store struct {
	db           *sql.DB
	taskBudgetMS int64
}

func NewStore(path string, taskBudget time.Duration) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("personal: database path is required")
	}
	budgetMS := int64(taskBudget / time.Millisecond)
	if budgetMS <= 0 {
		return nil, errors.New("personal: task budget must be at least one millisecond")
	}
	dsn := path
	if path == ":memory:" {
		dsn += "?_foreign_keys=on&_busy_timeout=5000"
	} else if strings.HasPrefix(path, "file:") {
		u, err := url.Parse(path)
		if err != nil {
			return nil, fmt.Errorf("personal: parse database URI: %w", err)
		}
		query := u.Query()
		query.Set("_foreign_keys", "on")
		query.Set("_busy_timeout", "5000")
		u.RawQuery = query.Encode()
		dsn = u.String()
	} else {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("personal: resolve database path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0700); err != nil {
			return nil, fmt.Errorf("personal: create database directory: %w", err)
		}
		file, err := os.OpenFile(absPath, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("personal: create database file: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("personal: close database file: %w", err)
		}
		if err := os.Chmod(absPath, 0600); err != nil {
			return nil, fmt.Errorf("personal: secure database file: %w", err)
		}
		dsn = (&url.URL{Scheme: "file", Path: absPath, RawQuery: "_foreign_keys=on&_busy_timeout=5000"}).String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("personal: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("personal: connect database: %w", err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("personal: begin schema transaction: %w", err)
	}
	if _, err := tx.Exec(personalSchema); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("personal: initialize schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("personal: commit schema: %w", err)
	}
	return &Store{db: db, taskBudgetMS: budgetMS}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func parseTimestamp(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("personal: invalid stored timestamp: %w", err)
	}
	return t, nil
}

func remainingMS(budget, used int64) int64 {
	if used >= budget {
		return 0
	}
	return budget - used
}
