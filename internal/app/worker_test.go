package app

import (
	"context"
	"encoding/json"
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
	"kagari/internal/digest"
	"kagari/internal/distribution"
	"kagari/internal/domain"
	"kagari/internal/store"
	"kagari/internal/telegram"
)

func TestAnalyzeArchiveCacheDigestAndUncertainDelivery(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		a := domain.Analysis{Title: "文章标题", Overview: "原文事实", Summary: []domain.Claim{{Text: "原文事实", SourceIDs: []string{"article"}}}, Discussion: []domain.Claim{}, Evaluation: []domain.Claim{{Text: "适合当前需求", SourceIDs: []string{"article"}}}, Category: "工程", Tags: []string{"Go"}, Uncertainties: []string{}}
		a.Headings = &domain.AnalysisHeadings{Summary: "原文简报喵", Discussion: "讨论", Evaluation: "taffy锐评", Uncertainties: "限制", Sources: "来源"}
		var output any = a
		properties := body["text"].(map[string]any)["format"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)
		if properties["sections"] != nil {
			input := digestModelInput(t, body)
			output = digestModelReview(input)
		}
		raw, _ := json.Marshal(output)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_app", "object": "response", "status": "completed", "model": "test", "output": []any{map[string]any{"type": "message", "id": "msg_app", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": string(raw), "annotations": []any{}}}}}})
	}))
	defer server.Close()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProfilePath = t.TempDir() + "/missing"
	cfg.Model.BaseURL = server.URL + "/v1"
	cfg.Model.APIKey = "fake"
	cfg.Model.Name = "test"
	s, err := store.New(t.TempDir() + "/archive.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e, err := agent.New(ctx, cfg, func(_ context.Context, u string) (domain.Source, error) {
		return domain.Source{ID: "article", URL: u, RequestedURL: u, Status: "ok", Content: "正文应被保存到本地", FetchedAt: time.Now()}, nil
	}, s.CachedSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: s, Engine: e, Config: cfg, Log: zap.NewNop()}
	now := time.Now().UTC()
	legacy, err := e.Prepare(domain.Submission{UserID: 7, ChatID: 7, URLs: []string{"https://example.org/article"}, ReceivedAt: now.Add(-48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	legacyPayload, _ := json.Marshal(legacy)
	legacyID, _, err := s.Enqueue(ctx, "analyze", "legacy", legacyPayload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJob(ctx, legacyID); err != nil {
		t.Fatal(err)
	}
	legacyResult, _ := json.Marshal(domain.Result{Analysis: domain.Analysis{Title: "旧答案引用了用户提示词"}, AnalysisVersion: "source-boundaries-v1", CreatedAt: now})
	if err := s.CompletePublication(ctx, legacyID, legacyResult, nil, nil); err != nil {
		t.Fatal(err)
	}
	var firstID int64
	for i := 0; i < 2; i++ {
		sub, err := e.Prepare(domain.Submission{UserID: 7, ChatID: 7, URLs: []string{"https://example.org/article"}, ReceivedAt: now.Add(time.Duration(i) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(sub)
		id, _, err := s.Enqueue(ctx, "analyze", string(rune('a'+i)), raw, telegram.Targets([]int64{7}))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = id
		}
		job, err := s.StartJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Process(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("identical submission did not reuse analysis")
	}
	entries, err := s.Entries(ctx, 7, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Result.Sources[0].Content != "正文应被保存到本地" {
		t.Fatal("archive dedup/evidence missing")
	}
	if headings := entries[0].Result.Analysis.Headings; headings == nil || headings.Evaluation != "taffy锐评" {
		t.Fatalf("archive lost generated headings: %+v", headings)
	}
	request := domain.DigestRequest{UserID: 7, Start: now.Add(-time.Hour), End: now.Add(time.Hour), Version: domain.DigestVersion}
	targets := telegram.Targets([]int64{7})
	id, created, err := w.EnqueueDigest(ctx, request, targets)
	if err != nil || !created {
		t.Fatal(err)
	}
	job, err := s.StartJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Process(ctx, job); err != nil {
		t.Fatal(err)
	}
	job, _ = s.Job(ctx, id)
	var report digest.Report
	if err := json.Unmarshal(job.Result, &report); err != nil {
		t.Fatal(err)
	}
	if report.Input == nil || len(report.Input.Entries) != 1 || report.Review == nil || digest.Count(*report.Review) != 1 || requests.Load() != 2 {
		t.Fatalf("digest generation %+v", report)
	}
	duplicate, created, err := w.EnqueueDigest(ctx, request, targets)
	if err != nil || created || duplicate != id {
		t.Fatal("digest period duplicated")
	}
	if text := w.command(ctx, domain.Command{UserID: 8, ChatID: 8, Text: "/status 1"}); !strings.Contains(text, "不属于你") {
		t.Fatal("cross-user status access")
	}
	sendCtx, cancel := context.WithCancel(ctx)
	if err := deliver(sendCtx, w, distribution.Dispatcher{"telegram": telegram.Adapter(func(_ context.Context, _ int64, text string) (int64, error) {
		if !strings.Contains(text, "原文简报喵：") || !strings.Contains(text, "taffy锐评：") || strings.Contains(text, "AI 摘要：") || strings.Contains(text, "Agent 评价：") {
			t.Errorf("delivery replaced model headings: %s", text)
		}
		cancel()
		return 0, &telegram.SendError{Reason: "timeout", Uncertain: true}
	})}); err != nil {
		t.Fatal(err)
	}
	counts, err := s.DeliveryCounts(ctx, firstID)
	if err != nil || counts["uncertain"] != 1 {
		t.Fatalf("delivery outcome lost: %v %v", counts, err)
	}
	if err := s.RetryDeliveries(ctx, firstID); err != nil {
		t.Fatal(err)
	}
	counts, _ = s.DeliveryCounts(ctx, firstID)
	if counts["pending"] == 0 {
		t.Fatal("manual resend not queued")
	}
}

func TestScheduleRestartCatchupIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg, _ := config.Load("")
	cfg.Weekly.Enabled = true
	cfg.Weekly.Timezone = "UTC"
	cfg.Telegram.AllowedUserIDs = []int64{7}
	cfg.Telegram.TargetChatIDs = []int64{7, -100123}
	w := &Worker{Store: s, Config: cfg, Log: zap.NewNop()}
	if err := s.SetMeta(ctx, "weekly_cursor", "2026-09-14T08:00:00Z"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	if err := schedule(ctx, w, now); err != nil {
		t.Fatal(err)
	}
	if err := schedule(ctx, w, now); err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		job, err := s.ClaimJob(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
		count++
		if job.Kind != "digest" || len(job.Targets) != 2 || !reflect.DeepEqual(job.Targets, telegram.Targets([]int64{7, -100123})) {
			t.Fatal("wrong scheduled job")
		}
	}
	if count != 3 {
		t.Fatalf("scheduled jobs=%d", count)
	}
}
