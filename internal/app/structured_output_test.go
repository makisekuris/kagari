package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/store"
	"kagari/internal/telegram"
)

func TestChatResultUsesSubmissionChatAndSkipsAnalysisCacheAndArchive(t *testing.T) {
	ctx := context.Background()
	const chatBody = "**plain reply** <b>& literal</b>"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_chat", "object": "response", "status": "completed", "model": "test",
			"output": []any{map[string]any{"type": "message", "id": "msg_chat", "role": "assistant", "status": "completed", "content": []any{
				map[string]any{"type": "output_text", "text": `{"kind":"chat","body":"**plain reply** <b>& literal</b>"}`, "annotations": []any{}},
			}}},
			"usage": map[string]any{"input_tokens": 2, "output_tokens": 3, "total_tokens": 5},
		})
	}))
	defer server.Close()

	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProfilePath = t.TempDir() + "/missing-profile"
	cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = server.URL+"/v1", "test-key", "test-model"
	cfg.Reader.CacheTTL = time.Hour
	e, err := agent.New(ctx, cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := &Worker{Store: s, Engine: e, Config: cfg, Log: zap.NewNop()}
	targets := telegram.Targets([]int64{71, -100123})
	now := time.Now().UTC()
	run := func(key string, sub domain.Submission, frozen []domain.DeliveryTarget) int64 {
		t.Helper()
		raw, err := json.Marshal(sub)
		if err != nil {
			t.Fatal(err)
		}
		id, created, err := s.Enqueue(ctx, "analyze", key, raw, frozen)
		if err != nil || !created {
			t.Fatalf("Enqueue() = (%d, %v, %v)", id, created, err)
		}
		job, err := s.StartJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Process(ctx, job); err != nil {
			t.Fatal(err)
		}
		return id
	}

	sub := domain.Submission{UserID: 7, ChatID: 71, Text: "hello", CacheKey: "same-chat", ReceivedAt: now}
	legacy := sub
	legacy.ReceivedAt = now.Add(-time.Hour)
	legacyPayload, _ := json.Marshal(legacy)
	legacyID, _, err := s.Enqueue(ctx, "analyze", "legacy-cache", legacyPayload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJob(ctx, legacyID); err != nil {
		t.Fatal(err)
	}
	legacyResult, _ := json.Marshal(domain.Result{Body: "old analysis", AnalysisVersion: "old-version", CreatedAt: now})
	if err := s.CompletePublication(ctx, legacyID, legacyResult, nil, nil); err != nil {
		t.Fatal(err)
	}
	firstID := run("chat-first", sub, targets)
	job, err := s.Job(ctx, firstID)
	if err != nil || job == nil || len(job.Targets) != 2 || job.Targets[0] != targets[0] || job.Targets[1] != targets[1] {
		t.Fatalf("saved job = (%+v, %v), want original frozen targets", job, err)
	}
	var saved domain.Result
	if err := json.Unmarshal(job.Result, &saved); err != nil || saved.Kind != domain.ResultKindChat || saved.Body != chatBody || saved.AnalysisVersion != agent.AnalysisVersion || saved.Usage.TotalTokens != 5 {
		t.Fatalf("saved chat result = (%+v, %v), want structured task result with metadata", saved, err)
	}
	counts, err := s.DeliveryTargetCounts(ctx, firstID)
	if err != nil || len(counts) != 1 || counts[0].Target != targets[0] {
		t.Fatalf("chat delivery targets = (%+v, %v), want original private chat only", counts, err)
	}
	delivery, err := s.ClaimDelivery(ctx)
	if err != nil || delivery == nil || delivery.JobID != firstID || delivery.Text != chatBody {
		t.Fatalf("chat delivery = (%+v, %v), want plain body without analysis/task framing", delivery, err)
	}
	if delivery.Format != domain.ContentPlainText {
		t.Fatalf("chat format = %q, want plain text", delivery.Format)
	}

	sub.ReceivedAt = now.Add(time.Minute)
	run("chat-duplicate", sub, targets)
	cli := domain.Submission{UserID: 7, Text: "cli chat", CacheKey: "cli-chat", ReceivedAt: now.Add(2 * time.Minute)}
	cliID := run("chat-cli", cli, targets)
	if requests.Load() != 3 {
		t.Fatalf("model requests = %d, want one for each chat result without reusing the old-version cache", requests.Load())
	}
	deliveryCounts, err := s.DeliveryCounts(ctx, cliID)
	if err != nil || len(deliveryCounts) != 0 {
		t.Fatalf("CLI chat delivery counts = (%v, %v), want no deliveries with zero ChatID", deliveryCounts, err)
	}
	entries, err := s.Entries(ctx, 7, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil || len(entries) != 0 {
		t.Fatalf("chat Entries() = (%+v, %v), want empty archive", entries, err)
	}
	if entry, err := s.ArchiveEntry(ctx, 7, firstID); err != nil || entry != nil {
		t.Fatalf("chat ArchiveEntry() = (%+v, %v), want nil", entry, err)
	}

	analysis := domain.Submission{UserID: 7, ChatID: 71, Text: "analysis", CacheKey: "analysis", ReceivedAt: now.Add(3 * time.Minute)}
	raw, _ := json.Marshal(analysis)
	analysisID, _, err := s.Enqueue(ctx, "analyze", "analysis", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJob(ctx, analysisID); err != nil {
		t.Fatal(err)
	}
	analysisResult, _ := json.Marshal(domain.Result{Kind: domain.ResultKindAnalysis, Body: "analysis archive"})
	if err := s.CompletePublication(ctx, analysisID, analysisResult, nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Entries(ctx, 7, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil || len(entries) != 1 || entries[0].JobID != analysisID || entries[0].Result.Body != "analysis archive" {
		t.Fatalf("analysis archive = (%+v, %v), want analysis unaffected", entries, err)
	}
}

func TestStatusDoesNotTreatInvalidKindsAsLegacyAnalysis(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := &Worker{Store: s}
	for _, kind := range []string{`null`, `""`, `"other"`, `123`} {
		id, _, err := s.Enqueue(ctx, "analyze", kind, []byte(`{}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartJob(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := s.CompletePublication(ctx, id, []byte(`{"kind":`+kind+`,"body":"saved"}`), nil, nil); err != nil {
			t.Fatal(err)
		}
		status, err := w.Status(ctx, id)
		if err != nil || !strings.Contains(status, "结果类型：unknown") {
			t.Fatalf("status with kind %s = (%q, %v), want unknown", kind, status, err)
		}
	}
}

func TestResultRoutingKeepsTargetsAcrossRetries(t *testing.T) {
	for _, kind := range []domain.ResultKind{domain.ResultKindAnalysis, domain.ResultKindChat} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if requests.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":{"message":"temporary failure","type":"server_error"}}`))
					return
				}
				body := "reply body"
				if kind == domain.ResultKindAnalysis {
					body = "## 标题\n\n**reply body**"
				}
				raw, _ := json.Marshal(map[string]any{"kind": kind, "body": body})
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_retry", "object": "response", "status": "completed", "model": "test",
					"output": []any{map[string]any{"type": "message", "id": "msg_retry", "role": "assistant", "status": "completed", "content": []any{
						map[string]any{"type": "output_text", "text": string(raw), "annotations": []any{}},
					}}},
				})
			}))
			defer server.Close()
			cfg, err := config.Load("")
			if err != nil {
				t.Fatal(err)
			}
			cfg.MaxAttempts = 1
			cfg.ProfilePath = t.TempDir() + "/missing-profile"
			cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = server.URL+"/v1", "test-key", "test-model"
			e, err := agent.New(ctx, cfg, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			s, err := store.New(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			w := &Worker{Store: s, Engine: e, Config: cfg, Log: zap.NewNop()}
			targets := telegram.Targets([]int64{71, -100123})
			raw, _ := json.Marshal(domain.Submission{UserID: 7, ChatID: 71, Text: "question", CacheKey: "retry", ReceivedAt: time.Now()})
			id, _, err := s.Enqueue(ctx, "analyze", "retry", raw, targets)
			if err != nil {
				t.Fatal(err)
			}
			if job, err := w.ProcessJob(ctx, id); err == nil || job == nil || job.Status != "failed" {
				t.Fatalf("first attempt = (%+v, %v), want failed task", job, err)
			}
			status, err := w.Status(ctx, id)
			if err != nil || strings.Contains(status, "结果类型") {
				t.Fatalf("failed task status = (%q, %v), want no completed result kind", status, err)
			}
			w.Config.Telegram.TargetChatIDs = []int64{999, -100999}
			if err := s.RetryJob(ctx, id); err != nil {
				t.Fatal(err)
			}
			job, err := w.ProcessJob(ctx, id)
			if err != nil || job == nil || job.Status != "completed" || !reflect.DeepEqual(job.Targets, targets) {
				t.Fatalf("processing retry = (%+v, %v), want completed task with frozen targets", job, err)
			}
			wantTargets, wantText := targets, fmt.Sprintf("任务 #%d\n## 标题\n\n**reply body**", id)
			if kind == domain.ResultKindChat {
				wantTargets, wantText = targets[:1], "reply body"
			}
			counts, err := s.DeliveryTargetCounts(ctx, id)
			if err != nil || len(counts) != len(wantTargets) {
				t.Fatalf("delivery targets = (%+v, %v), want %+v", counts, err, wantTargets)
			}
			for _, count := range counts {
				if count.Target != targets[0] && (kind == domain.ResultKindChat || count.Target != targets[1]) {
					t.Fatalf("unexpected delivery target: %+v", count)
				}
			}
			status, err = w.Status(ctx, id)
			if err != nil || !strings.Contains(status, "结果类型："+string(kind)) {
				t.Fatalf("completed status = (%q, %v), want result kind %s", status, err, kind)
			}
			first, err := s.ClaimDelivery(ctx)
			if err != nil || first == nil || first.JobID != id || first.Target != targets[0] || first.Text != wantText {
				t.Fatalf("first delivery = (%+v, %v)", first, err)
			}
			wantFormat := domain.ContentPlainText
			if kind == domain.ResultKindAnalysis {
				wantFormat = domain.ContentMarkdown
			}
			if first.Format != wantFormat {
				t.Fatalf("format for %s = %q, want %q", kind, first.Format, wantFormat)
			}
			if err := s.FailDelivery(ctx, first.ID, "outcome unknown", 1, time.Now(), true); err != nil {
				t.Fatal(err)
			}
			if err := s.RetryDeliveries(ctx, id); err != nil {
				t.Fatal(err)
			}
			retry, err := s.ClaimDelivery(ctx)
			if err != nil || retry == nil || retry.ID != first.ID || retry.Target != first.Target || retry.Text != first.Text || retry.Format != first.Format || requests.Load() != 2 {
				t.Fatalf("delivery retry = (%+v, %v), model calls = %d, want persisted target/body without reanalysis", retry, err, requests.Load())
			}
		})
	}
}
