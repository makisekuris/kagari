package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenMemoryOwnsAndCleansAttachmentDirectory(t *testing.T) {
	db, err := openDatabase(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	dir := db.attachmentDir
	if dir == "" {
		t.Fatal("Open(:memory:) returned no attachment directory")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("attachment directory is unavailable: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temporary attachment directory still exists: %v", err)
	}
}

func TestOpenCreatesParentAndDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "personal.db")
	db, err := openDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("database file = (%v,%v)", info, err)
	}
}
