package logging

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultRotateBytes = int64(50 << 20)
	defaultMaxBytes    = int64(1 << 30)
	defaultMaxAge      = 7 * 24 * time.Hour
)

var (
	errRecordTooLarge = errors.New("record exceeds maximum log storage size")
	errMaxStorage     = errors.New("maximum log storage size reached")
)

type ownedLog struct {
	path      string
	name      string
	size      int64
	createdAt time.Time
}

func (s *Session) openActiveLocked() error {
	if s.file != nil {
		return nil
	}
	for range 3 {
		createdAt := time.Now().UTC()
		name := "kagari-" + createdAt.Format("20060102T150405.000000000Z") + "-" + NewID() + ".jsonl"
		path := filepath.Join(s.dir, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("create log file: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			os.Remove(path)
			return fmt.Errorf("set log file permissions: %w", err)
		}
		s.file, s.filePath, s.fileCreatedAt = file, path, createdAt
		return nil
	}
	return errors.New("could not allocate a unique log file")
}

func (s *Session) closeActiveLocked() error {
	if s.file == nil {
		return nil
	}
	file := s.file
	s.file, s.filePath, s.fileCreatedAt = nil, "", time.Time{}
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(syncErr, closeErr)
}

func (s *Session) verifyActiveWriteLocked() error {
	// Creation alone misses quota/full-disk errors; remove the probe so it does
	// not consume a sequence number or become a historical record.
	probe := []byte("{}\n")
	n, err := s.writeFile(s.file, probe)
	if err == nil && n != len(probe) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = s.file.Sync()
	}
	if err == nil {
		err = s.file.Truncate(0)
	}
	if err == nil {
		err = s.file.Sync()
	}
	return err
}

func (s *Session) ensureRecordCapacityLocked(recordBytes int64) error {
	if recordBytes > s.maxBytes {
		return errRecordTooLarge
	}
	if s.file != nil {
		info, err := s.file.Stat()
		if err != nil {
			return fmt.Errorf("inspect active log file: %w", err)
		}
		// Retention is based on file creation time so appends cannot keep old records forever.
		tooOld := !s.fileCreatedAt.IsZero() && time.Since(s.fileCreatedAt) > s.maxAge
		if info.Size() > 0 && (info.Size()+recordBytes > s.rotateBytes || s.totalBytes+recordBytes > s.maxBytes || tooOld) {
			if err := s.closeActiveLocked(); err != nil {
				return fmt.Errorf("close rotated log file: %w", err)
			}
		}
	}
	if err := s.pruneLocked(recordBytes); err != nil {
		return err
	}
	if err := s.openActiveLocked(); err != nil {
		return err
	}
	if s.totalBytes+recordBytes > s.maxBytes {
		return errMaxStorage
	}
	return nil
}

func (s *Session) cleanupLocked() error {
	if s.file != nil {
		_, err := s.file.Stat()
		if err != nil {
			return fmt.Errorf("inspect active log file: %w", err)
		}
		if !s.fileCreatedAt.IsZero() && time.Since(s.fileCreatedAt) > s.maxAge {
			if err := s.closeActiveLocked(); err != nil {
				return fmt.Errorf("close expired log file: %w", err)
			}
		}
	}
	return s.pruneLocked(0)
}

func (s *Session) pruneLocked(reserve int64) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	files := make([]ownedLog, 0, len(entries))
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		createdAt, owned := logFileCreatedAt(name)
		if !owned {
			continue
		}
		path := filepath.Join(s.dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect log file: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		f := ownedLog{path: path, name: name, size: info.Size(), createdAt: createdAt}
		files = append(files, f)
		total += f.size
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].createdAt.Equal(files[j].createdAt) {
			return files[i].name < files[j].name
		}
		return files[i].createdAt.Before(files[j].createdAt)
	})
	activePath := s.filePath
	for _, file := range files {
		if file.path == activePath || time.Since(file.createdAt) <= s.maxAge {
			continue
		}
		if err := os.Remove(file.path); err != nil {
			return fmt.Errorf("remove expired log file: %w", err)
		}
		total -= file.size
	}
	for _, file := range files {
		if total+reserve <= s.maxBytes {
			break
		}
		if file.path == activePath {
			continue
		}
		if _, err := os.Lstat(file.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect log file for retention: %w", err)
		}
		if err := os.Remove(file.path); err != nil {
			return fmt.Errorf("remove old log file: %w", err)
		}
		total -= file.size
	}
	s.totalBytes = total
	if total+reserve > s.maxBytes {
		return errMaxStorage
	}
	return nil
}

func logFileCreatedAt(name string) (time.Time, bool) {
	// The timestamp and ID must match our generated format before retention can delete a file.
	if !strings.HasPrefix(name, "kagari-") || !strings.HasSuffix(name, ".jsonl") {
		return time.Time{}, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, "kagari-"), ".jsonl")
	parts := strings.Split(body, "-")
	if len(parts) != 2 || len(parts[1]) != 26 {
		return time.Time{}, false
	}
	for _, r := range parts[1] {
		if !((r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7')) {
			return time.Time{}, false
		}
	}
	createdAt, err := time.Parse("20060102T150405.000000000Z", parts[0])
	return createdAt, err == nil
}

func ListFiles(directory string) ([]string, error) {
	return listFiles(directory, os.Lstat)
}

func listFiles(directory string, stat func(string) (os.FileInfo, error)) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read log directory: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, ok := logFileCreatedAt(entry.Name()); !ok {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect log file: %w", err)
		}
		if info.Mode().IsRegular() {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}
