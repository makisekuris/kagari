package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kagari/internal/app"
	"kagari/internal/domain"
	"kagari/internal/store"
)

func seedDebugDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kagari.db")
	db, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	target := []domain.DeliveryTarget{{Channel: "telegram", Address: "7"}}
	id, _, err := db.Enqueue(ctx, "analyze", "completed", []byte(`{"user_id":1}`), target)
	if err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimJob(ctx)
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("ClaimJob() = (%+v, %v), want job %d", job, err, id)
	}
	source := domain.Source{RequestedURL: "https://example.test/a", URL: "https://example.test/a", Status: "ok"}
	if err := db.CompletePublication(ctx, id, []byte(`{}`), []domain.Delivery{{Target: target[0], Part: 1, Text: "stored delivery"}}, []domain.Source{source}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Enqueue(ctx, "analyze", "pending", []byte(`{"user_id":2}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AcceptUpdate(ctx, 41, "", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDBInfoAndListJSON(t *testing.T) {
	path := seedDebugDB(t)
	var out bytes.Buffer
	if err := runDB(context.Background(), path, []string{"info"}, &out); err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatalf("info output is not JSON: %v (%q)", err, out.String())
	}
	absolute, _ := filepath.Abs(path)
	if info["path"] != absolute || info["tables"] == nil || info["job_statuses"] == nil || info["delivery_statuses"] == nil || info["next_telegram_offset"] != float64(42) {
		t.Fatalf("unexpected info JSON: %#v", info)
	}

	out.Reset()
	if err := runDB(context.Background(), path, []string{"list", "-limit", "1", "-offset", "0", "jobs"}, &out); err != nil {
		t.Fatal(err)
	}
	var listing dbListOutput
	if err := json.Unmarshal(out.Bytes(), &listing); err != nil {
		t.Fatalf("list output is not JSON: %v (%q)", err, out.String())
	}
	if listing.Path != absolute || listing.Table != "jobs" || listing.Limit != 1 || listing.Offset != 0 || len(listing.Rows) != 1 {
		t.Fatalf("unexpected list JSON: %+v", listing)
	}
	if _, ok := listing.Rows[0]["key"]; !ok {
		t.Fatalf("job row did not include key: %#v", listing.Rows[0])
	}
}

func TestDBRejectsInvalidArgsWithoutCreatingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "info extra", args: []string{"info", "extra"}, want: "usage"},
		{name: "table not allowed", args: []string{"list", "private_table"}, want: "table"},
		{name: "limit range", args: []string{"list", "-limit", "101", "jobs"}, want: "limit"},
		{name: "offset range", args: []string{"list", "-offset", "-1", "jobs"}, want: "offset"},
		{name: "flags after table", args: []string{"list", "jobs", "-limit", "2"}, want: "usage"},
		{name: "missing confirmation", args: []string{"clear", "-scope", "data"}, want: "irreversible"},
		{name: "invalid scope", args: []string{"clear", "-scope", "tables", "-yes"}, want: "scope"},
		{name: "clear extra", args: []string{"clear", "-yes", "extra"}, want: "usage"},
		{name: "confirmed missing database", args: []string{"clear", "-yes"}, want: "must already exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runDB(context.Background(), path, tc.args, &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runDB() error = %v, want message containing %q", err, tc.want)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid command created database: stat error = %v", statErr)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid command wrote output: %q", out.String())
			}
		})
	}
}

func TestDBHelpDoesNotOpenOrCreateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"info", "-h"}, want: "usage: kagari db info"},
		{args: []string{"list", "-h"}, want: "usage: kagari db list"},
		{args: []string{"clear", "-yes", "-h"}, want: "usage: kagari db clear"},
		{args: []string{"-h"}, want: "usage: kagari db"},
	} {
		var out bytes.Buffer
		if err := runDB(context.Background(), path, tc.args, &out); err != nil {
			t.Fatalf("runDB(%v): %v", tc.args, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Fatalf("runDB(%v) help = %q, want %q", tc.args, out.String(), tc.want)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("help %v created database: stat error = %v", tc.args, err)
		}
	}
}

func TestDBClearWithoutConfirmationLeavesExistingDatabaseUntouched(t *testing.T) {
	path := seedDebugDB(t)
	var out bytes.Buffer
	err := runDB(context.Background(), path, []string{"clear", "-scope", "all"}, &out)
	if err == nil || !strings.Contains(err.Error(), "irreversible") || !strings.Contains(err.Error(), "-yes") || !strings.Contains(err.Error(), "db info") {
		t.Fatalf("unconfirmed clear error = %v, want irreversible/info/-yes guidance", err)
	}
	if out.Len() != 0 {
		t.Fatalf("unconfirmed clear wrote output: %q", out.String())
	}
	out.Reset()
	if err := runDB(context.Background(), path, []string{"info"}, &out); err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["next_telegram_offset"] != float64(42) {
		t.Fatalf("unconfirmed clear modified offset: %#v", info["next_telegram_offset"])
	}
	tables := info["tables"].([]any)
	for _, table := range tables {
		entry := table.(map[string]any)
		if entry["name"] == "jobs" && entry["rows"] != float64(2) {
			t.Fatalf("unconfirmed clear modified jobs: %#v", entry)
		}
	}
}

func TestDBClearCacheScopeLeavesTaskData(t *testing.T) {
	path := seedDebugDB(t)
	var out bytes.Buffer
	if err := runDB(context.Background(), path, []string{"clear", "-scope", "cache", "-yes"}, &out); err != nil {
		t.Fatal(err)
	}
	var cleared dbClearOutput
	if err := json.Unmarshal(out.Bytes(), &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Scope != "cache" || cleared.TelegramOffsetReset || cleared.Deleted["source_cache"] != 1 || len(cleared.Deleted) != 1 {
		t.Fatalf("unexpected cache clear result: %+v", cleared)
	}
	out.Reset()
	if err := runDB(context.Background(), path, []string{"info"}, &out); err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	tables, ok := info["tables"].([]any)
	if !ok {
		t.Fatalf("info tables have unexpected type: %#v", info["tables"])
	}
	for _, raw := range tables {
		entry := raw.(map[string]any)
		if entry["name"] == "jobs" && entry["rows"] != float64(2) {
			t.Fatalf("cache clear modified jobs: %#v", entry)
		}
	}
}

func TestDBClearRequiresLockAndPersistsScope(t *testing.T) {
	path := seedDebugDB(t)
	unlock, err := app.Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runDB(context.Background(), path, []string{"info"}, &out); err != nil {
		t.Fatalf("read-only info was blocked by processor lock: %v", err)
	}
	out.Reset()
	if err := runDB(context.Background(), path, []string{"list", "jobs"}, &out); err != nil {
		t.Fatalf("read-only list was blocked by processor lock: %v", err)
	}
	out.Reset()
	err = runDB(context.Background(), path, []string{"clear", "-scope", "data", "-yes"}, &out)
	unlock()
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("clear with service lock error = %v, want lock refusal", err)
	}
	if out.Len() != 0 {
		t.Fatalf("failed clear wrote output: %q", out.String())
	}

	if err := runDB(context.Background(), path, []string{"clear", "-yes"}, &out); err != nil {
		t.Fatal(err)
	}
	var dataClear dbClearOutput
	if err := json.Unmarshal(out.Bytes(), &dataClear); err != nil {
		t.Fatalf("clear output is not JSON: %v (%q)", err, out.String())
	}
	if dataClear.Scope != "data" || dataClear.TelegramOffsetReset || dataClear.Deleted["jobs"] != 2 || dataClear.Deleted["deliveries"] != 1 || dataClear.Deleted["source_cache"] != 1 {
		t.Fatalf("unexpected data clear result: %+v", dataClear)
	}

	out.Reset()
	if err := runDB(context.Background(), path, []string{"info"}, &out); err != nil {
		t.Fatalf("database was not readable after data clear: %v", err)
	}
	var info map[string]any
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["next_telegram_offset"] != float64(42) {
		t.Fatalf("data clear changed Telegram offset: %#v", info["next_telegram_offset"])
	}

	out.Reset()
	if err := runDB(context.Background(), path, []string{"clear", "-scope", "all", "-yes"}, &out); err != nil {
		t.Fatal(err)
	}
	var allClear dbClearOutput
	if err := json.Unmarshal(out.Bytes(), &allClear); err != nil {
		t.Fatal(err)
	}
	if allClear.Scope != "all" || !allClear.TelegramOffsetReset {
		t.Fatalf("unexpected all clear result: %+v", allClear)
	}
	out.Reset()
	if err := runDB(context.Background(), path, []string{"info"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["next_telegram_offset"] != float64(0) {
		t.Fatalf("all clear did not reset Telegram offset: %#v", info["next_telegram_offset"])
	}
}
