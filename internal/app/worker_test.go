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
	const finalAnalysis = "## 原文简报喵\n原文事实\n\n## taffy锐评\n适合当前需求"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		call := requests.Add(1)
		var output []any
		switch call {
		case 1:
			tools, ok := body["tools"].([]any)
			if !ok || len(tools) == 0 {
				t.Errorf("analysis request did not expose read_url: %v", body["tools"])
			}
			output = []any{map[string]any{"type": "function_call", "id": "fc_app_read", "call_id": "call_app_read", "name": "read_url", "arguments": `{"url":"https://example.org/article"}`}}
		case 2:
			input, _ := json.Marshal(body["input"])
			if !strings.Contains(string(input), "function_call_output") || !strings.Contains(string(input), "正文应被保存到本地") {
				t.Errorf("analysis final turn lacks the read result: %s", input)
			}
			output = []any{map[string]any{"type": "message", "id": "msg_analysis", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": finalAnalysis, "annotations": []any{}}}}}
		case 3:
			if tools, ok := body["tools"].([]any); ok && len(tools) != 0 {
				t.Errorf("weekly digest exposed tools: %v", tools)
			}
			input := digestModelInput(t, body)
			if len(input.Entries) != 1 || input.Entries[0].Body != finalAnalysis || len(input.Entries[0].Sources) != 1 || !input.Entries[0].Sources[0].Usable {
				t.Errorf("weekly digest input omitted archived analysis or source metadata: %+v", input.Entries)
			}
			output = []any{map[string]any{"type": "message", "id": "msg_digest", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "## 本周阅读\n这些材料介绍了同一项工程实践喵。", "annotations": []any{}}}}}
		default:
			t.Errorf("unexpected model request %d", call)
			output = []any{map[string]any{"type": "message", "id": "msg_unexpected", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "unexpected", "annotations": []any{}}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_app", "object": "response", "status": "completed", "model": "test", "output": output})
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
	var reads atomic.Int32
	e, err := agent.New(ctx, cfg, func(_ context.Context, u string) (domain.Source, error) {
		reads.Add(1)
		return domain.Source{ID: "article", URL: u, RequestedURL: u, Status: "ok", Content: "正文应被保存到本地", FetchedAt: time.Now()}, nil
	}, s.CachedSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: s, Engine: e, Config: cfg, Log: zap.NewNop()}
	now := time.Now().UTC()
	var firstID int64
	for i := 0; i < 2; i++ {
		sub, err := e.Prepare(domain.Submission{UserID: 7, ChatID: 7, URLs: []string{"https://example.org/article"}, ReceivedAt: now.Add(time.Duration(i) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(sub)
		id, _, err := s.Enqueue(ctx, "analyze", string(rune('a'+i)), raw, 7)
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
	if requests.Load() != 2 || reads.Load() != 1 {
		t.Fatalf("analysis did not preserve its read and reuse the cached result: model turns=%d reads=%d", requests.Load(), reads.Load())
	}
	entries, err := s.Entries(ctx, 7, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Result.Body != finalAnalysis || len(entries[0].Result.Sources) != 1 || entries[0].Result.Sources[0].Content != "正文应被保存到本地" {
		t.Fatal("archive dedup/evidence missing")
	}
	request := domain.DigestRequest{Version: domain.DigestVersion, UserID: 7, Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	id, created, err := w.EnqueueDigest(ctx, request, 7)
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
	if report.Input == nil || len(report.Input.Entries) != 1 || report.Body == "" || requests.Load() != 3 {
		t.Fatalf("digest generation %+v", report)
	}
	duplicate, created, err := w.EnqueueDigest(ctx, request, 7)
	if err != nil || created || duplicate != id {
		t.Fatal("digest period duplicated")
	}
	if text := w.command(ctx, domain.Command{UserID: 8, ChatID: 8, Text: "/status 1"}); !strings.Contains(text, "不属于你") {
		t.Fatal("cross-user status access")
	}
	sendCtx, cancel := context.WithCancel(ctx)
	if err := deliver(sendCtx, w, distribution.Dispatcher{"telegram": telegram.Adapter(func(_ context.Context, _ int64, text string) (int64, error) {
		if !strings.Contains(text, "原文简报喵") || !strings.Contains(text, "taffy锐评") || strings.Contains(text, "AI 摘要：") || strings.Contains(text, "Agent 评价：") {
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
		if job.Kind != "digest" || job.TargetChatID != 7 || !reflect.DeepEqual(job.Targets, telegram.Targets([]int64{7, -100123})) {
			t.Fatal("wrong scheduled job")
		}
	}
	if count != 3 {
		t.Fatalf("scheduled jobs=%d", count)
	}
}
