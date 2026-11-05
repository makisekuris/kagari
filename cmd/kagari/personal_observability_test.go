package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kagari/internal/config"

	"go.uber.org/zap"
)

// An unusable log destination must stop startup before model construction or
// Telegram polling; otherwise the service could silently operate without history.
func TestPersonalRejectsUnusableLogsBeforeWorkers(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.Name, cfg.Model.APIKey, cfg.Model.BaseURL = "fixture", "test-key", "http://127.0.0.1:1/v1"
	cfg.Telegram.Token, cfg.Telegram.AllowedUserIDs = "123:fixture", []int64{123}
	cfg.Storage.Path = filepath.Join(t.TempDir(), "personal.db")
	cfg.Logging.Directory = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cfg.Logging.Directory, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	err = runPersonal(context.Background(), cfg, nil, zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "initialize service logs") {
		t.Fatalf("startup did not fail at the logging boundary: %v", err)
	}
	data, err := os.ReadFile(cfg.Logging.Directory)
	if err != nil || string(data) != "keep" {
		t.Fatal("startup changed the non-directory log target")
	}
}
