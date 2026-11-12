package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type database struct {
	*sql.DB
	attachmentDir string
	temporaryDir  bool
}

func openDatabase(ctx context.Context, path string) (*database, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("personal: database path is required")
	}
	dsn, attachmentDir, temporaryDir, err := writableDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		cleanup(attachmentDir, temporaryDir)
		return nil, fmt.Errorf("personal: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	result := &database{DB: db, attachmentDir: attachmentDir, temporaryDir: temporaryDir}
	if err := result.PingContext(ctx); err != nil {
		_ = result.Close()
		return nil, fmt.Errorf("personal: connect database: %w", err)
	}
	return result, nil
}

func openExistingDatabase(ctx context.Context, path string) (*database, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, errors.New("personal inspection requires an existing database file")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("personal database must be a regular file")
	}
	query := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(5000)"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: query.Encode()}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	result := &database{DB: db, attachmentDir: filepath.Dir(abs)}
	if err := result.PingContext(ctx); err != nil {
		_ = result.Close()
		return nil, err
	}
	return result, nil
}

func writableDSN(path string) (dsn, attachmentDir string, temporaryDir bool, err error) {
	if path == ":memory:" {
		attachmentDir, err = os.MkdirTemp("", "kagari-personal-")
		if err != nil {
			return "", "", false, fmt.Errorf("personal: create temporary attachment directory: %w", err)
		}
		return path + "?_foreign_keys=on&_busy_timeout=5000", attachmentDir, true, nil
	}
	if strings.HasPrefix(path, "file:") {
		u, parseErr := url.Parse(path)
		if parseErr != nil {
			return "", "", false, fmt.Errorf("personal: parse database URI: %w", parseErr)
		}
		query := u.Query()
		query.Set("_foreign_keys", "on")
		query.Set("_busy_timeout", "5000")
		u.RawQuery = query.Encode()
		if u.Path != "" && u.Path != ":memory:" {
			attachmentDir = filepath.Dir(u.Path)
		}
		return u.String(), attachmentDir, false, nil
	}
	absPath, absErr := filepath.Abs(path)
	if absErr != nil {
		return "", "", false, fmt.Errorf("personal: resolve database path: %w", absErr)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0700); err != nil {
		return "", "", false, fmt.Errorf("personal: create database directory: %w", err)
	}
	file, err := os.OpenFile(absPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", "", false, fmt.Errorf("personal: create database file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", "", false, fmt.Errorf("personal: close database file: %w", err)
	}
	if err := os.Chmod(absPath, 0600); err != nil {
		return "", "", false, fmt.Errorf("personal: secure database file: %w", err)
	}
	return (&url.URL{Scheme: "file", Path: absPath, RawQuery: "_foreign_keys=on&_busy_timeout=5000"}).String(), filepath.Dir(absPath), false, nil
}

func cleanup(path string, temporary bool) {
	if temporary {
		_ = os.RemoveAll(path)
	}
}

// Close closes the connection and removes only the temporary attachment root
// created for an in-memory database.
func (d *database) Close() error {
	err := d.DB.Close()
	if d.temporaryDir {
		if cleanupErr := os.RemoveAll(d.attachmentDir); err == nil {
			err = cleanupErr
		}
	}
	return err
}
