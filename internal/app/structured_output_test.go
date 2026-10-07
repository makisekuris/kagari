package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_chat", "object": "response", "status": "completed", "model": "test",
			"output": []any{map[string]any{"type": "message", "id": "msg_chat", "role": "assistant", "status": "completed", "content": []any{
				map[string]any{"type": "output_text", "text": `{"kind":"chat","body":"plain reply"}`, "annotations": []any{}},
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
	firstID := run("chat-first", sub, targets)
	job, err := s.Job(ctx, firstID)
	if err != nil || job == nil || len(job.Targets) != 2 || job.Targets[0] != targets[0] || job.Targets[1] != targets[1] {
		t.Fatalf("saved job = (%+v, %v), want original frozen targets", job, err)
	}
	var saved domain.Result
	if err := json.Unmarshal(job.Result, &saved); err != nil || saved.Kind != domain.ResultKindChat || saved.Body != "plain reply" || saved.AnalysisVersion != agent.AnalysisVersion || saved.Usage.TotalTokens != 5 {
		t.Fatalf("saved chat result = (%+v, %v), want structured task result with metadata", saved, err)
	}
	counts, err := s.DeliveryTargetCounts(ctx, firstID)
	if err != nil || len(counts) != 1 || counts[0].Target != targets[0] {
		t.Fatalf("chat delivery targets = (%+v, %v), want original private chat only", counts, err)
	}
	delivery, err := s.ClaimDelivery(ctx)
	if err != nil || delivery == nil || delivery.JobID != firstID || delivery.Text != "plain reply" {
		t.Fatalf("chat delivery = (%+v, %v), want plain body without analysis/task framing", delivery, err)
	}

	sub.ReceivedAt = now.Add(time.Minute)
	run("chat-duplicate", sub, targets)
	cli := domain.Submission{UserID: 7, Text: "cli chat", CacheKey: "cli-chat", ReceivedAt: now.Add(2 * time.Minute)}
	cliID := run("chat-cli", cli, targets)
	if requests.Load() != 3 {
		t.Fatalf("model requests = %d, want one for each chat result", requests.Load())
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
