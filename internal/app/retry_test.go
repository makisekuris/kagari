package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/store"
)

func TestProcessJobRetriesModelAndNotifiesOnlyExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failure  string
		failures int
		terminal bool
	}{
		{"http_recovers", "http", 2, false},
		{"http_exhausts", "http", 6, true},
		{"invalid_json_recovers", "json", 1, false},
		{"invalid_json_exhausts", "json", 6, true},
		{"invalid_citation_recovers", "citation", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				failed := requests.Add(1) <= int32(tc.failures)
				if failed && tc.failure == "http" {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":{"message":"fixture_secret_body","type":"forbidden"}}`))
					return
				}
				a := domain.Analysis{Title: "文章", Overview: "事实", Summary: []domain.Claim{{Text: "事实", SourceIDs: []string{"article"}}}, Category: "工程"}
				a.Headings = &domain.AnalysisHeadings{Summary: "事实", Discussion: "讨论", Evaluation: "评价", Uncertainties: "限制", Sources: "来源"}
				if failed && tc.failure == "citation" {
					a.Summary[0].SourceIDs = []string{"unread"}
				}
				raw, _ := json.Marshal(a)
				if failed && tc.failure == "json" {
					raw = []byte(`{"title"c:"fixture_secret_body"}`)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_retry", "object": "response", "status": "completed", "model": "test", "output": []any{map[string]any{"type": "message", "id": "msg_retry", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": string(raw), "annotations": []any{}}}}}})
			}))
			defer server.Close()
			cfg, err := config.Load("")
			if err != nil {
				t.Fatal(err)
			}
			cfg.MaxAttempts = 6
			cfg.ProfilePath = t.TempDir() + "/missing"
			cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = server.URL+"/v1", "fake", "test"
			s, err := store.New(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			e, err := agent.New(ctx, cfg, func(_ context.Context, u string) (domain.Source, error) {
				return domain.Source{ID: "article", URL: u, RequestedURL: u, Status: "ok", Content: "完整正文", FetchedAt: time.Now()}, nil
			}, s.CachedSource)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := e.Prepare(domain.Submission{UserID: 7, ChatID: 7, URLs: []string{"https://example.org/article"}})
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(sub)
			id, _, err := s.Enqueue(ctx, "analyze", "selected", payload, 900)
			if err != nil {
				t.Fatal(err)
			}
			unrelatedID, _, err := s.Enqueue(ctx, "notice", "unrelated", []byte(`{"text":"queued"}`), 0)
			if err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zapcore.DebugLevel)
			worker := &Worker{Store: s, Engine: e, Config: cfg, Log: zap.New(core)}
			waits := 0
			job, processErr := worker.processJob(ctx, id, func(_ context.Context, delay time.Duration) bool {
				waits++
				want := backoff(waits)
				if delay <= want-time.Second || delay > want {
					t.Errorf("retry delay = %s, expected close to %s", delay, want)
				}
				return true
			})
			wantAttempts := tc.failures + 1
			wantState := "completed"
			if tc.terminal {
				wantAttempts = cfg.MaxAttempts
				wantState = "failed"
			}
			if job == nil || job.ID != id || job.Status != wantState || job.Attempts != wantAttempts || (processErr != nil) != tc.terminal || int(requests.Load()) != wantAttempts || waits != wantAttempts-1 {
				t.Fatalf("retry outcome: job=%+v err=%v requests=%d waits=%d", job, processErr, requests.Load(), waits)
			}
			if processErr != nil && strings.Contains(processErr.Error(), "fixture_secret_body") {
				t.Fatal("final CLI error exposed the upstream response body")
			}
			workerWarnings, workerErrors := 0, 0
			for _, entry := range logs.All() {
				if strings.HasPrefix(entry.Message, "agent ") {
					fields := entry.ContextMap()
					if fields["job_id"] != id || fields["attempt"] == nil || fields["max_attempts"] != int64(cfg.MaxAttempts) {
						t.Errorf("agent log lacks attempt correlation: %v", fields)
					}
				}
				if !strings.HasPrefix(entry.Message, "job processing") {
					continue
				}
				if entry.Level == zapcore.WarnLevel {
					workerWarnings++
				}
				if entry.Level == zapcore.ErrorLevel {
					workerErrors++
				}
				if tc.failure == "http" && entry.ContextMap()["http_status"] != int64(http.StatusForbidden) {
					t.Errorf("model status missing from local diagnostic: %+v", entry)
				}
				if entry.ContextMap()["max_attempts"] != int64(cfg.MaxAttempts) || entry.ContextMap()["reason"] == "" {
					t.Errorf("attempt metadata or safe reason missing: %+v", entry)
				}
				if _, raw := entry.ContextMap()["error"]; raw {
					t.Fatal("raw upstream error was logged")
				}
			}
			terminalErrors := 0
			if tc.terminal {
				terminalErrors = 1
			}
			if workerWarnings != waits || workerErrors != terminalErrors {
				t.Fatalf("wrong worker warning/error levels: warns=%d errors=%d all=%+v", workerWarnings, workerErrors, logs.All())
			}
			if tc.terminal && (job.LastError == "" || strings.Contains(job.LastError, "fixture_secret_body")) {
				t.Fatalf("last_error is empty or exposed upstream response: %q", job.LastError)
			}
			unrelated, err := s.Job(ctx, unrelatedID)
			if err != nil || unrelated.Status != "pending" || unrelated.Attempts != 0 {
				t.Fatalf("selected CLI job consumed unrelated work: %+v, %v", unrelated, err)
			}
			if _, err := s.StartJob(ctx, unrelatedID); err != nil {
				t.Fatal(err)
			}
			if tc.terminal {
				notice, err := s.ClaimJob(ctx)
				if err != nil || notice == nil || notice.Kind != "notice" || notice.TargetChatID != 7 {
					t.Fatalf("missing private final notice: %+v, %v", notice, err)
				}
				var command domain.Command
				if err := json.Unmarshal(notice.Payload, &command); err != nil || strings.Contains(command.Text, "fixture_secret_body") || strings.Contains(command.Text, "403") || !strings.Contains(command.Text, "/retry") {
					t.Fatalf("unsafe or incomplete final notice: %+v, %v", command, err)
				}
				if next, err := s.ClaimJob(ctx); err != nil || next != nil {
					t.Fatalf("duplicate final notice: %+v, %v", next, err)
				}
			} else if next, err := s.ClaimJob(ctx); err != nil || next != nil {
				t.Fatalf("recovered task generated a bot warning: %+v, %v", next, err)
			}
		})
	}
}

func TestCanceledAnalysisPreservesEvidenceWithoutFinalNotice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxAttempts = 1
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := &agent.Engine{Config: cfg, Read: func(_ context.Context, u string) (domain.Source, error) {
		cancel()
		return domain.Source{ID: "partial", URL: u, Content: "保留的片段", Status: "failed"}, context.Canceled
	}}
	sub, err := e.Prepare(domain.Submission{UserID: 7, ChatID: 7, URLs: []string{"https://example.org/article"}})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(sub)
	id, _, err := s.Enqueue(context.Background(), "analyze", "cancel", payload, 900)
	if err != nil {
		t.Fatal(err)
	}
	worker := &Worker{Store: s, Engine: e, Config: cfg, Log: zap.NewNop()}
	if _, err := worker.ProcessJob(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	job, err := s.Job(context.Background(), id)
	if err != nil || job.Status != "pending" || job.Attempts != 0 || !strings.Contains(string(job.Result), "保留的片段") {
		t.Fatalf("interrupted task lost evidence or retry budget: %+v, %v", job, err)
	}
	if next, err := s.ClaimJob(context.Background()); err != nil || next == nil || next.ID != id {
		t.Fatalf("canceled task was not left resumable: %+v, %v", next, err)
	}
	if next, err := s.ClaimJob(context.Background()); err != nil || next != nil {
		t.Fatalf("user cancellation generated a final notice: %+v, %v", next, err)
	}
}
