package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/digest"
	"kagari/internal/domain"
	"kagari/internal/persona"
	"kagari/internal/store"

	"go.uber.org/zap"
)

type digestTestPersona string

func (p digestTestPersona) Prompt() string { return string(p) }

func digestModelInput(t *testing.T, body map[string]any) domain.DigestInput {
	t.Helper()
	for _, message := range body["input"].([]any) {
		item := message.(map[string]any)
		if item["role"] != "user" {
			continue
		}
		text, _ := item["content"].(string)
		if blocks, ok := item["content"].([]any); ok {
			for _, content := range blocks {
				block := content.(map[string]any)
				if block["type"] == "input_text" {
					text, _ = block["text"].(string)
				}
			}
		}
		if text != "" {
			var input domain.DigestInput
			if err := json.Unmarshal([]byte(text), &input); err != nil {
				t.Fatal(err)
			}
			return input
		}
	}
	t.Fatal("no digest user input")
	return domain.DigestInput{}
}

func digestModelReview(input domain.DigestInput) domain.DigestReview {
	item := domain.DigestItem{Title: "这一周的工程线索喵", Review: "这些材料介绍了同一项工程实践喵。"}
	for _, entry := range input.Entries {
		item.EntryIDs = append(item.EntryIDs, entry.JobID)
		for _, source := range entry.Sources {
			if source.Usable {
				item.Refs = append(item.Refs, domain.DigestRef{JobID: entry.JobID, SourceID: source.ID})
			}
		}
	}
	return domain.DigestReview{Opening: "小菲来回顾这周的阅读喵。", Sections: []domain.DigestSection{{Name: "工程简报喵", Items: []domain.DigestItem{item}}}}
}

func archiveDigestEntry(t *testing.T, s *store.Store, key string, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	request, _ := json.Marshal(domain.Submission{UserID: 7, ReceivedAt: at, CacheKey: key})
	id, _, err := s.Enqueue(ctx, "analyze", key, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(domain.Result{Analysis: domain.Analysis{Title: "工程实践", Overview: "归档概述", Category: "工程", Summary: []domain.Claim{{Text: "归档事实", SourceIDs: []string{"source"}}}}, Sources: []domain.Source{{ID: "source", URL: "https://example.org/" + key, Status: "ok", Content: "全文不应传入周报模型"}}, CreatedAt: at})
	if err := s.CompletePublication(ctx, id, result, nil, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDigestRetryFreezesInputAndRecoveryReplaysOutput(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/archive.db"
	s, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	archiveDigestEntry(t, s, "first", now.Add(-time.Hour))
	archiveDigestEntry(t, s, "second", now.Add(-2*time.Hour))
	var inputs []string
	var modelInputs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		input := digestModelInput(t, body)
		modelInput, _ := json.Marshal(body["input"])
		modelInputs = append(modelInputs, string(modelInput))
		rawInput, _ := json.Marshal(input)
		inputs = append(inputs, string(rawInput))
		if len(inputs) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary","type":"server_error"}}`))
			return
		}
		raw, _ := json.Marshal(digestModelReview(input))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_digest", "object": "response", "status": "completed", "model": "test", "output": []any{map[string]any{"type": "message", "id": "msg_digest", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": string(raw), "annotations": []any{}}}}}, "usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18}})
	}))
	defer server.Close()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProfilePath = t.TempDir() + "/missing"
	cfg.Model.BaseURL, cfg.Model.Name, cfg.Model.APIKey = server.URL+"/v1", "test", "fake"
	initialPersona := persona.Default()
	e, err := agent.New(ctx, cfg, nil, nil, initialPersona)
	if err != nil {
		t.Fatal(err)
	}
	e.Profile = "初始表达偏好"
	worker := &Worker{Store: s, Engine: e, Config: cfg, Log: zap.NewNop()}
	request := domain.DigestRequest{UserID: 7, Start: now.AddDate(0, 0, -7), End: now, Version: domain.DigestVersion}
	targets := []domain.DeliveryTarget{{Channel: "telegram", Address: "900"}}
	id, _, err := worker.EnqueueDigest(ctx, request, targets)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.StartJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(ctx, job); err == nil {
		t.Fatal("first model call should fail")
	}
	failed, _ := s.Job(ctx, id)
	var snapshot digest.Report
	if err := json.Unmarshal(failed.Result, &snapshot); err != nil || snapshot.Input == nil || snapshot.Review != nil {
		t.Fatalf("failed attempt lost input: %+v %v", snapshot, err)
	}
	if !strings.Contains(snapshot.Input.Instruction, initialPersona.Prompt()) {
		t.Fatal("digest snapshot did not freeze the injected persona")
	}
	archiveDigestEntry(t, s, "late", now.Add(-3*time.Hour))
	replacement, err := agent.New(ctx, cfg, nil, nil, digestTestPersona("replacement-persona"))
	if err != nil {
		t.Fatal(err)
	}
	replacement.Profile = "后来改动的偏好"
	worker.Engine = replacement
	job, err = s.StartJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟生成已完成、尚未提交 outbox 时进程退出。
	report, text, err := worker.processDigest(ctx, job)
	if err != nil || len(inputs) != 2 || inputs[0] != inputs[1] || strings.Contains(inputs[0], "全文不应") || len(report.Input.Entries) != 2 || report.Input.Profile != "初始表达偏好" || report.Usage.TotalTokens != 18 || !strings.HasPrefix(text, "本周回顾（1条）") {
		t.Fatalf("snapshot/generation: %+v %q err=%v inputs=%v", report, text, err, inputs)
	}
	if modelInputs[0] != modelInputs[1] || strings.Contains(modelInputs[1], "replacement-persona") {
		t.Fatal("digest retry replaced the frozen persona with the current one")
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	worker.Engine = nil
	worker.Config.Model.APIKey = ""
	job, err = s.StartJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(ctx, job); err != nil {
		t.Fatal(err)
	}
	job, err = s.Job(ctx, id)
	if err != nil || job.Status != "completed" || len(inputs) != 2 {
		t.Fatalf("recovery called model or failed: %+v %v", job, err)
	}
	counts, err := s.DeliveryCounts(ctx, id)
	if err != nil || counts["pending"] != 1 {
		t.Fatalf("outbox: %v %v", counts, err)
	}
	if duplicate, created, err := worker.EnqueueDigest(ctx, request, targets); err != nil || created || duplicate != id {
		t.Fatalf("request lost idempotency: %d %v %v", duplicate, created, err)
	}
	if _, err := worker.ProcessJob(ctx, id); err != nil || len(inputs) != 2 {
		t.Fatalf("completed job did not replay: %v", err)
	}
}

func TestDigestEmptyAndInputLimitDoNotCallModel(t *testing.T) {
	for _, empty := range []bool{true, false} {
		ctx := context.Background()
		s, err := store.New(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		cfg, err := config.Load("")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Model.APIKey = ""
		cfg.ProfilePath = t.TempDir() + "/missing"
		cfg.MaxAttempts = 1
		cfg.Weekly.MaxInputChars = 1024
		now := time.Now()
		if !empty {
			archiveDigestEntry(t, s, "article", now.Add(-time.Hour))
		}
		worker := &Worker{Store: s, Config: cfg, Log: zap.NewNop()}
		id, _, err := worker.EnqueueDigest(ctx, domain.DigestRequest{UserID: 7, Start: now.AddDate(0, 0, -7), End: now, Version: domain.DigestVersion}, nil)
		if err != nil {
			t.Fatal(err)
		}
		job, err := worker.ProcessJob(ctx, id)
		if empty {
			var report digest.Report
			if err != nil || json.Unmarshal(job.Result, &report) != nil || !strings.HasPrefix(digest.Render(report), "本周回顾（0条）") {
				t.Fatalf("empty digest requires model: %+v %v", job, err)
			}
		} else if err == nil || job.Status != "failed" || !strings.Contains(job.LastError, "max_input_chars") {
			t.Fatalf("input limit did not precede credential check: %+v %v", job, err)
		}
	}
}

func TestDigestRejectsMissingEntriesWithoutPublishing(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_invalid", "object": "response", "status": "completed", "model": "test", "output": []any{map[string]any{"type": "message", "id": "msg_invalid", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": `{"opening":"","sections":[],"closing":""}`, "annotations": []any{}}}}}})
	}))
	defer server.Close()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.BaseURL, cfg.Model.Name, cfg.Model.APIKey = server.URL+"/v1", "test", "fake"
	cfg.ProfilePath = t.TempDir() + "/missing"
	cfg.MaxAttempts = 1
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	archiveDigestEntry(t, s, "article", now.Add(-time.Hour))
	worker := &Worker{Store: s, Config: cfg, Log: zap.NewNop(), Persona: digestTestPersona("worker-injected-persona")}
	id, _, err := worker.EnqueueDigest(ctx, domain.DigestRequest{UserID: 7, Start: now.AddDate(0, 0, -7), End: now, Version: domain.DigestVersion}, []domain.DeliveryTarget{{Channel: "telegram", Address: "900"}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := worker.ProcessJob(ctx, id)
	if err == nil || job.Status != "failed" || job.LastError != "weekly review failed validation" {
		t.Fatalf("missing entries were published: %+v %v", job, err)
	}
	var report digest.Report
	if err := json.Unmarshal(job.Result, &report); err != nil || report.Input == nil || report.Review != nil {
		t.Fatalf("invalid review persisted as valid output: %+v %v", report, err)
	}
	if !strings.Contains(report.Input.Instruction, "worker-injected-persona") {
		t.Fatal("CLI digest did not use the worker's injected persona")
	}
	counts, err := s.DeliveryCounts(ctx, id)
	if err != nil || len(counts) != 0 {
		t.Fatalf("invalid output entered outbox: %v %v", counts, err)
	}
	notice, err := s.ClaimJob(ctx)
	if err != nil || notice == nil || notice.Kind != "notice" || len(notice.Targets) != 1 || notice.Targets[0].Address != "7" {
		t.Fatalf("failed digest needs a private retry notice: %+v %v", notice, err)
	}
}
