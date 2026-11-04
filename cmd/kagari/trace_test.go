package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kagari/internal/config"
	"kagari/internal/logging"
	"kagari/internal/personal"

	"go.uber.org/zap"
)

// A real model context easily exceeds Scanner's default token limit. Exercise
// the persisted format and query together, including a crash-truncated tail.
func TestTraceReadsLargeCallsAndReportsMissingTail(t *testing.T) {
	ctx := context.Background()
	var cfg config.Config
	cfg.Storage.Path = filepath.Join(t.TempDir(), "personal.db")
	cfg.Model.APIKey = "configured-secret"
	s, err := personal.NewStore(cfg.Storage.Path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AcceptInput(ctx, 1, &personal.Input{MessageID: 10, Text: "private chat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	directory := logging.DefaultDirectory(cfg.Storage.Path)
	session, err := logging.Open(logging.Options{Directory: directory, StoreID: storeID, Console: io.Discard, Secrets: []string{cfg.Model.APIKey}})
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("large-context-", 6000)
	log := session.Logger.With(zap.Int64("event_id", id), zap.String("model_call_id", "call-1"))
	log.Info("model.started", zap.Any("payload", map[string]any{"input": large, "token": "unknown-credential", "note": cfg.Model.APIKey}))
	log.Info("model.finished", zap.Any("payload", "model answer"))
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := logging.ListFiles(directory)
	if err != nil || len(files) != 1 {
		t.Fatalf("log files: %v, %v", files, err)
	}
	f, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"interrupted":`)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.Storage.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, full := range []bool{false, true} {
		args := []string{"--event", "1"}
		if full {
			args = append(args, "--full")
		}
		var out bytes.Buffer
		if err := inspectTrace(ctx, cfg, args, &out); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Records  []map[string]any `json:"records"`
			Warnings []string         `json:"warnings"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("invalid trace JSON: %v", err)
		}
		if len(result.Records) != 2 || !strings.Contains(strings.Join(result.Warnings, " "), "invalid log record") {
			t.Fatalf("trace evidence: %d records, %v", len(result.Records), result.Warnings)
		}
		if strings.Contains(out.String(), cfg.Model.APIKey) || strings.Contains(out.String(), "unknown-credential") {
			t.Fatal("trace exposed a redacted credential")
		}
		if strings.Contains(out.String(), large) != full {
			t.Fatalf("full=%v did not control payload output", full)
		}
	}
	after, err := os.ReadFile(cfg.Storage.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("trace changed the database")
	}
}

// Inspection errors must never bootstrap a new database or initialize logging.
func TestObservationCLIRejectsInvalidInputWithoutCreatingFiles(t *testing.T) {
	ctx := context.Background()
	var cfg config.Config
	cfg.Storage.Path = filepath.Join(t.TempDir(), "missing.db")
	for _, args := range [][]string{nil, {"--event", "0"}, {"--event", "1", "--task", "2"}, {"--task", "-1"}} {
		if err := inspectTrace(ctx, cfg, args, io.Discard); err == nil {
			t.Fatalf("invalid trace accepted: %v", args)
		}
	}
	if err := inspectEvents(ctx, cfg, nil, io.Discard); err == nil {
		t.Fatal("missing DB accepted")
	}
	if _, err := os.Stat(cfg.Storage.Path); !os.IsNotExist(err) {
		t.Fatal("inspection created database")
	}
	if _, err := os.Stat(logging.DefaultDirectory(cfg.Storage.Path)); !os.IsNotExist(err) {
		t.Fatal("inspection created logs")
	}
}

// A replacement DB can reuse numeric event IDs. Neither another store's log nor
// this service's startup record proves the queried event has retained history.
func TestTraceSeparatesStoreIdentityAndMissingEventHistory(t *testing.T) {
	ctx := context.Background()
	var cfg config.Config
	cfg.Storage.Path = filepath.Join(t.TempDir(), "personal.db")
	s, err := personal.NewStore(cfg.Storage.Path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInput(ctx, 1, &personal.Input{MessageID: 10, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pollingMessages := []string{"telegram.polling.ready", "telegram.polling.failed", "telegram.polling.recovered", "telegram.polling.rejected"}
	for _, identity := range []string{storeID, "previous-database"} {
		session, err := logging.Open(logging.Options{Directory: logging.DefaultDirectory(cfg.Storage.Path), StoreID: identity, Console: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		if identity == storeID {
			session.Logger.Info("service.starting")
			if err := s.Recover(logging.WithLogger(ctx, session.Logger)); err != nil {
				t.Fatal(err)
			}
			session.Logger.Info("telegram.input.accepted", zap.Int64("event_id", 999))
		} else {
			session.Logger.Info("model.started", zap.Int64("event_id", 1))
			session.Logger.Info("store.outcome", zap.String("operation", "store.recover"))
		}
		for _, msg := range pollingMessages {
			session.Logger.Info(msg)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := inspectTrace(ctx, cfg, []string{"--event", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "previous-database") || strings.Contains(out.String(), "model.started") {
		t.Fatal("trace associated a different database's record")
	}
	if !strings.Contains(out.String(), "service.starting") || !strings.Contains(out.String(), "No matching historical records") {
		t.Fatal("service startup masked missing event history")
	}
	for _, msg := range append(pollingMessages, "store.recover") {
		if !strings.Contains(out.String(), msg) {
			t.Errorf("trace omitted retained service evidence: %s", msg)
		}
	}
	if strings.Contains(out.String(), "telegram.input.accepted") {
		t.Fatal("trace included an unrelated input")
	}
}

// One user turn can create multiple tasks. Exercise persisted logs and the CLI
// so a sibling's unfinished call cannot contaminate the selected task's history.
func TestTraceSelectsTaskBeforeSharedSourceEvent(t *testing.T) {
	ctx := context.Background()
	var cfg config.Config
	cfg.Storage.Path = filepath.Join(t.TempDir(), "personal.db")
	s, err := personal.NewStore(cfg.Storage.Path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session, err := logging.Open(logging.Options{Directory: logging.DefaultDirectory(cfg.Storage.Path), StoreID: storeID, Console: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx = logging.WithLogger(ctx, session.Logger)
	eventID, err := s.AcceptInput(ctx, 1, &personal.Input{MessageID: 10, Text: "Start two tasks"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimEvent(ctx); err != nil {
		t.Fatal(err)
	}
	a, aRun, err := s.CreateTask(ctx, eventID, "a", "Task A", personal.TaskInput{Goal: "A"})
	if err != nil {
		t.Fatal(err)
	}
	b, bRun, err := s.CreateTask(ctx, eventID, "b", "Task B", personal.TaskInput{Goal: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEvent(ctx, eventID, personal.TurnOutput{Text: "Both queued"}, []string{"Both queued"}); err != nil {
		t.Fatal(err)
	}
	session.Logger.Warn("telegram.polling.failed", zap.Int("api_code", 409))
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		id     string
		taskID int64
		runID  int64
		ended  bool
	}{
		{"conversation", 0, 0, true},
		{"task-a", a.ID, aRun.ID, true},
		{"task-b", b.ID, bRun.ID, false},
		{"task-b-run-only", 0, bRun.ID, false},
	} {
		log := session.Logger.With(zap.Int64("event_id", eventID), zap.Int64("task_id", call.taskID), zap.Int64("run_id", call.runID), zap.String("model_call_id", call.id))
		log.Info("model.started")
		if call.ended {
			log.Info("model.finished")
		}
	}
	for _, selector := range []struct {
		flag string
		id   int64
	}{
		{"--task", a.ID},
		{"--event", eventID},
	} {
		t.Run(selector.flag, func(t *testing.T) {
			var out bytes.Buffer
			if err := inspectTrace(ctx, cfg, []string{selector.flag, strconv.FormatInt(selector.id, 10)}, &out); err != nil {
				t.Fatal(err)
			}
			var trace struct {
				Records  []map[string]any `json:"records"`
				Warnings []string         `json:"warnings"`
			}
			if err := json.Unmarshal(out.Bytes(), &trace); err != nil {
				t.Fatal(err)
			}
			calls := make(map[string]int)
			for _, record := range trace.Records {
				if id, ok := record["model_call_id"].(string); ok {
					calls[id]++
				}
				if selector.flag == "--task" && (record["task_id"] == float64(b.ID) || record["run_id"] == float64(bRun.ID)) {
					t.Errorf("task A trace contains task B record: %v", record)
				}
			}
			if calls["conversation"] != 2 || calls["task-a"] != 2 {
				t.Errorf("missing or duplicated related calls: %v", calls)
			}
			wantSibling := 0
			if selector.flag == "--event" {
				wantSibling = 1
			}
			if calls["task-b"] != wantSibling || calls["task-b-run-only"] != wantSibling {
				t.Errorf("sibling calls: %v; want %d records each", calls, wantSibling)
			}
			if unfinished := strings.Contains(strings.Join(trace.Warnings, " "), "start without a recorded finish"); unfinished != (selector.flag == "--event") {
				t.Errorf("unfinished call warning has wrong scope: %v", trace.Warnings)
			}
			if !strings.Contains(out.String(), "telegram.polling.failed") || !strings.Contains(out.String(), "store.recover") {
				t.Error("trace omitted polling or recovery evidence")
			}
		})
	}
}
