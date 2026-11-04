package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// This protects the full JSONL payload contract while keeping structured content off console output.
func TestFileContainsFullLargeRecordAndConsoleOnlySafeMetadata(t *testing.T) {
	var console bytes.Buffer
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "logs"), StoreID: "store", Console: &console, Secrets: []string{"known-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(s.filePath); err != nil || info.Size() != 0 {
		t.Fatalf("startup write probe remained in active log: info=%v, err=%v", info, err)
	}
	large := "private payload marker " + strings.Repeat("x", 96<<10)
	s.Logger.Info("job completed",
		zap.Int("event_id", 17),
		zap.String("payload", large),
		zap.String("text", "private text marker"),
		zap.String("result", "private result marker"),
		zap.String("arguments", "private arguments marker"),
		zap.String("prompt", "private prompt marker"),
		zap.String("response_body", "private response marker"),
		zap.String("api_key", "sensitive-key-value"),
		zap.Any("tool memory key", "ordinary metadata"),
		zap.Any("nested", map[string]any{"detail": "known-secret"}),
	)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := ListFiles(s.dir)
	if err != nil || len(paths) != 1 {
		t.Fatalf("ListFiles = %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 64<<10 {
		t.Fatalf("full record was truncated: %d bytes", len(data))
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &record); err != nil {
		t.Fatalf("log record is not JSON: %v", err)
	}
	if record["schema_version"] != float64(1) || record["store_id"] != "store" || record["level"] != "info" || record["seq"] != float64(1) {
		t.Fatalf("required record metadata missing: %#v", record)
	}
	if record["payload"] != large || record["api_key"] != redactedValue || strings.Contains(string(data), "known-secret") {
		t.Fatal("file record did not preserve payload or redact configured/known secrets")
	}
	for _, forbidden := range []string{"private payload marker", "private text marker", "private result marker", "private arguments marker", "private prompt marker", "private response marker", "sensitive-key-value"} {
		if strings.Contains(console.String(), forbidden) {
			t.Fatalf("console exposed %q: %s", forbidden, console.String())
		}
	}
	if !strings.Contains(console.String(), "event_id=17") || !strings.Contains(console.String(), "job completed") {
		t.Fatalf("console omitted safe summary metadata: %q", console.String())
	}
}

// This protects info+ file capture when an operator raises the console threshold.
func TestFileInfoRecordsIgnoreConsoleThreshold(t *testing.T) {
	var console bytes.Buffer
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "logs"), Console: &console, Level: zapcore.ErrorLevel})
	if err != nil {
		t.Fatal(err)
	}
	s.Logger.Info("file only")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := ListFiles(s.dir)
	if err != nil || len(paths) != 1 {
		t.Fatalf("ListFiles = %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"msg":"file only"`)) || console.Len() != 0 {
		t.Fatalf("file/console threshold mismatch: file=%s console=%q", data, console.String())
	}
}

// This protects age cleanup by creation time, bounded storage, private modes, and ownership scope.
func TestRotationRetentionCapAgeAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldName := logName(time.Now().Add(-48 * time.Hour))
	oldPath := filepath.Join(dir, oldName)
	if err := os.WriteFile(oldPath, []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldPath, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	unrelatedPath := filepath.Join(dir, "kagari-not-owned.jsonl")
	if err := os.WriteFile(unrelatedPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	var console bytes.Buffer
	s, err := Open(Options{Directory: dir, Console: &console, RotateBytes: 300, MaxBytes: 1100, MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired owned file survived startup cleanup: %v", err)
	}
	for i := range 4 {
		s.Logger.Info("rotate", zap.Int("attempt", i), zap.String("payload", strings.Repeat("z", 600)))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory permissions = %v, %v", info, err)
	}
	if data, err := os.ReadFile(unrelatedPath); err != nil || string(data) != "keep" {
		t.Fatalf("unowned file changed: %q, %v", data, err)
	}
	paths, err := ListFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 || len(paths) >= 4 {
		t.Fatalf("rotation/retention file count = %d (%v)", len(paths), paths)
	}
	var total int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("log file mode = %o", info.Mode().Perm())
		}
		total += info.Size()
	}
	if total > 1100 {
		t.Fatalf("retained files exceed MaxBytes: %d", total)
	}
}

// This protects serialized record writes and unique sequence numbers under concurrent loggers.
func TestConcurrentWritesProduceCompleteUniqueRecords(t *testing.T) {
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "logs"), Console: io.Discard, Level: zapcore.FatalLevel})
	if err != nil {
		t.Fatal(err)
	}
	const workers, recordsPerWorker = 12, 50
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range recordsPerWorker {
				s.Logger.Info("concurrent", zap.Int("worker", worker), zap.Int("count", i))
			}
		}(worker)
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := ListFiles(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[uint64]bool, workers*recordsPerWorker)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatalf("partial or invalid record: %v", err)
			}
			seq := uint64(record["seq"].(float64))
			if seen[seq] {
				t.Fatalf("duplicate sequence %d", seq)
			}
			seen[seq] = true
		}
	}
	if len(seen) != workers*recordsPerWorker {
		t.Fatalf("complete records = %d, want %d", len(seen), workers*recordsPerWorker)
	}
}

// This protects business-safe writes, tail rollback, and recovery diagnostics after runtime failure.
func TestRuntimePartialWriteFailureRollsBackAndRecovers(t *testing.T) {
	var console bytes.Buffer
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "logs"), Console: &console})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.writeFile = func(file *os.File, record []byte) (int, error) {
		calls++
		if calls <= 2 {
			n, err := file.Write(record[:min(13, len(record))])
			if err != nil {
				return n, err
			}
			return n, errors.New("simulated private payload write failure")
		}
		return file.Write(record)
	}
	s.Logger.Info("failed event", zap.String("payload", "private payload"))
	s.Logger.Info("still failing")
	if strings.Contains(console.String(), "logging_recovered=true") || strings.Count(console.String(), "file write failed") != 1 {
		t.Fatal("continuing failures falsely reported recovery or bypassed diagnostic rate limiting")
	}
	s.Logger.Info("recovered event")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := ListFiles(s.dir)
	if err != nil || len(paths) != 1 {
		t.Fatalf("ListFiles = %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	if len(lines) != 1 {
		t.Fatalf("partial line remained after failed write: %q", data)
	}
	var record map[string]any
	if err := json.Unmarshal(lines[0], &record); err != nil {
		t.Fatalf("recovered line is invalid JSON: %v", err)
	}
	if record["msg"] != "recovered event" || record["logging_recovered"] != true || record["logging_lost_records"] != float64(2) {
		t.Fatalf("recovery record missing diagnostics: %#v", record)
	}
	if !strings.Contains(console.String(), "file write failed; reason=io_error") || !strings.Contains(console.String(), "recovered after 2 lost records") || strings.Contains(console.String(), "private payload") || strings.Contains(console.String(), "simulated") {
		t.Fatalf("unsafe/missing runtime diagnostics: %q", console.String())
	}
}

// This protects actionable but payload-free failure reasons at the logging boundary.
func TestClassifyLogErrorUsesSafeReasons(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errRecordTooLarge, "record_too_large"},
		{errMaxStorage, "storage_cap_reached"},
		{os.ErrPermission, "permission_denied"},
		{fmt.Errorf("wrapped: %w", syscall.ENOSPC), "no_space"},
		{errors.New("private disk error"), "io_error"},
	} {
		if got := classifyLogError(tc.err); got != tc.want {
			t.Errorf("classifyLogError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// This protects the distinction between startup errors returned to callers and runtime errors absorbed by the logger.
func TestOpenReturnsStartupFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Directory: path, Console: io.Discard}); err == nil {
		t.Fatal("Open should return startup path failures")
	}
}

// File creation can succeed while the first write fails, so Open must verify an actual write before returning.
func TestOpenRejectsStartupWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		written  int
		writeErr error
		wantErr  error
	}{
		{name: "disk full", writeErr: syscall.ENOSPC, wantErr: syscall.ENOSPC},
		{name: "partial write error", written: 1, writeErr: syscall.EIO, wantErr: syscall.EIO},
		{name: "short write", written: 1, wantErr: io.ErrShortWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "logs")
			var opened *os.File
			writeFile := func(file *os.File, probe []byte) (int, error) {
				opened = file
				n, err := file.Write(probe[:tc.written])
				if err != nil {
					return n, err
				}
				return n, tc.writeErr
			}
			session, err := open(Options{Directory: dir, Console: io.Discard}, writeFile)
			if session != nil {
				defer session.Close()
			}
			if !errors.Is(err, tc.wantErr) || session != nil {
				t.Fatalf("Open = (%v, %v), want a startup write error", session, err)
			}
			if opened == nil {
				t.Fatal("startup write probe was not attempted")
			}
			if _, err := opened.Stat(); err == nil {
				t.Fatal("failed startup probe left its file open")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed startup probe left files behind: entries=%v, err=%v", entries, err)
			}
		})
	}
}

// This protects valid-file enumeration from foreign names and symlinks, plus context fallback behavior.
func TestListFilesAndContextHelpers(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, logName(time.Now().Add(-time.Minute)))
	second := filepath.Join(dir, logName(time.Now()))
	for _, path := range []string{second, first} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "kagari-foreign.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, filepath.Join(dir, logName(time.Now().Add(time.Hour)))); err != nil {
		t.Fatal(err)
	}
	paths, err := ListFiles(dir)
	if err != nil || len(paths) != 2 || paths[0] != first || paths[1] != second {
		t.Fatalf("ListFiles = %v, %v", paths, err)
	}
	logger := zap.NewNop()
	if got := FromContext(context.Background()); got == nil || got.Core().Enabled(zapcore.InfoLevel) {
		t.Fatalf("missing context logger should be nop: %v", got)
	}
	if got := FromContext(WithLogger(context.Background(), logger)); got != logger {
		t.Fatalf("context logger = %p, want %p", got, logger)
	}
	if DefaultDirectory(filepath.Join("data", "kagari.db")) != filepath.Join("data", "kagari.logs") {
		t.Fatalf("DefaultDirectory = %q", DefaultDirectory(filepath.Join("data", "kagari.db")))
	}
	if len(NewID()) < 26 || NewID() == NewID() {
		t.Fatal("NewID did not produce distinct cryptographic IDs")
	}
}

// This protects history reads from a log file disappearing during concurrent rotation.
func TestListFilesSkipsFilesDeletedDuringEnumeration(t *testing.T) {
	dir := t.TempDir()
	deleted := filepath.Join(dir, logName(time.Now().Add(-time.Minute)))
	kept := filepath.Join(dir, logName(time.Now()))
	for _, path := range []string{deleted, kept} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := listFiles(dir, func(path string) (os.FileInfo, error) {
		if path == deleted {
			return nil, os.ErrNotExist
		}
		return os.Lstat(path)
	})
	if err != nil || len(paths) != 1 || paths[0] != kept {
		t.Fatalf("listFiles = %v, %v", paths, err)
	}
}

func logName(createdAt time.Time) string {
	return fmt.Sprintf("kagari-%s-%s.jsonl", createdAt.UTC().Format("20060102T150405.000000000Z"), NewID())
}
