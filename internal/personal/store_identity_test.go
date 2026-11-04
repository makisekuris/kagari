package personal

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A stable database identity joins logs across restart, while a fresh database
// must not inherit another database's trace just because both contain event 1.
func TestStoreIdentityPersistsAndSeparatesDatabases(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "personal.db")
	s := testStore(t, path, time.Minute)
	if id, err := s.Identity(ctx); err != nil || id != "" {
		t.Fatalf("inspection initialized identity: %q, %v", id, err)
	}
	id, err := s.EnsureIdentity(ctx)
	if err != nil || id == "" {
		t.Fatalf("initialize identity: %q, %v", id, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := testStore(t, path, time.Minute)
	if got, err := reopened.EnsureIdentity(ctx); err != nil || got != id {
		t.Fatalf("restart identity: %q, %v; want %q", got, err, id)
	}
	other := testStore(t, filepath.Join(t.TempDir(), "personal.db"), time.Minute)
	if got, err := other.EnsureIdentity(ctx); err != nil || got == "" || got == id {
		t.Fatalf("fresh database identity: %q, %v", got, err)
	}
}
