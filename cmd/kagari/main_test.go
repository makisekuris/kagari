package main

import (
	"os"
	"strings"
	"testing"
)

func TestRemovedCommandsDoNotCreateStorage(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("KAGARI_STORAGE_PATH", "data/unused.db")
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	for _, command := range []string{"read", "analyze", "digest", "status", "export", "retry", "retry-delivery", "db"} {
		os.Args = []string{"kagari", command}
		if err := run(); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("%s: expected unknown command, got %v", command, err)
		}
	}
	if _, err := os.Stat("data"); !os.IsNotExist(err) {
		t.Fatalf("removed command touched storage: %v", err)
	}
}
