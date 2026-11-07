package main

import (
	"path/filepath"
	"testing"
)

func TestLockFileExcludesOtherHoldersUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "personal.db")
	unlock, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unlock() }()
	if release, err := lockFile(path); err == nil {
		release()
		t.Fatal("second lock succeeded")
	}
	unlock()
	unlock = func() {}
	release, err := lockFile(path)
	if err != nil {
		t.Fatalf("released lock cannot be acquired: %v", err)
	}
	release()
}
