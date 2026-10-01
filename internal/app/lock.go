package app

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// The service and CLI analysis share one durable queue. Only one processor may
// recover or claim it at a time; inspection and explicit retries remain available.
func Lock(path string) (func(), error) {
	if path == ":memory:" {
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another processor holds the database lock: %w", err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
