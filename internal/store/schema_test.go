package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestNewRejectsUnsupportedSchemaWithoutPartialInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incompatible.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE jobs(id INTEGER PRIMARY KEY,kind TEXT,payload BLOB,status TEXT,next_attempt_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := New(path)
	if err == nil {
		opened.Close()
		t.Fatal("unsupported schema accepted")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='deliveries'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed initialization left a partial schema")
	}
}
