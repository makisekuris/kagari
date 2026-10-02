package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kagari/internal/config"
	"kagari/internal/domain"
)

func TestResponsesToolRoundTrip(t *testing.T) {
	var calls atomic.Int32
	var serializedInput string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		format := body["text"].(map[string]any)["format"].(map[string]any)
		if format["type"] != "json_schema" || format["strict"] != true {
			t.Errorf("missing strict format: %v", format)
		}
		properties := format["schema"].(map[string]any)["properties"].(map[string]any)
		if _, ok := properties["relevance"]; ok {
			t.Error("new analysis schema still requests relevance")
		}
		headings, ok := properties["headings"].(map[string]any)
		if !ok {
			t.Error("analysis schema is missing model-generated headings")
			return
		}
		for _, name := range []string{"summary", "discussion", "evaluation", "uncertainties", "sources"} {
			field := headings["properties"].(map[string]any)[name].(map[string]any)
			if field["type"] != "string" || field["enum"] != nil || field["const"] != nil {
				t.Errorf("heading %s must allow model-generated text: %v", name, field)
			}
		}
		if body["store"] != false {
			t.Error("remote storage should be disabled")
		}
		var output []any
		if calls.Add(1) == 1 {
			output = []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_article", "name": "read_source", "arguments": `{"url":"https://example.org/article","parent_source_id":"s_root","question":"核对作者原文","role":"primary"}`}}
		} else {
			input, _ := json.Marshal(body["input"])
			serializedInput = string(input)
			found := false
			for _, item := range body["input"].([]any) {
				obj := item.(map[string]any)
				if obj["type"] == "function_call_output" && obj["call_id"] == "call_article" {
					found = true
				}
			}
			if !found {
				t.Error("function output missing its call_id")
			}
			a := domain.Analysis{Title: "标题", Overview: "原文要点", Summary: []domain.Claim{{Text: "有依据的事实", SourceIDs: []string{"s_article"}}}, Discussion: []domain.Claim{}, Evaluation: []domain.Claim{}, Category: "工程", Tags: []string{"Go"}, Uncertainties: []string{}}
			a.Headings = &domain.AnalysisHeadings{Summary: "相关简报喵", Discussion: "讨论里的声音喵", Evaluation: "taffy锐评", Uncertainties: "待查线索喵", Sources: "原文与线索喵"}
			raw, _ := json.Marshal(a)
			output = []any{map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": string(raw), "annotations": []any{}}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_test", "object": "response", "created_at": 1, "status": "completed", "model": "test", "output": output, "usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18}})
	}))
	defer server.Close()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	question := strings.Repeat("question-only-instruction ", 4)
	notes := strings.Repeat("notes-only-context ", 4)
	profile := strings.Repeat("profile-only-preference ", 4)
	forwarded := "第三方讨论内容 https://example.org/discussion"
	cfg.ProfilePath = t.TempDir() + "/profile.md"
	if err := os.WriteFile(cfg.ProfilePath, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Model.BaseURL = server.URL + "/v1"
	cfg.Model.APIKey = "test"
	cfg.Model.Name = "test"
	reads := 0
	e, err := New(context.Background(), cfg, func(_ context.Context, u string) (domain.Source, error) {
		reads++
		if u == "https://example.org/discussion" {
			return domain.Source{ID: "s_root", URL: u, RequestedURL: u, Content: "讨论引用了作者原文", Status: "ok", Links: []domain.Link{{URL: "https://example.org/article"}}}, nil
		}
		return domain.Source{ID: "s_article", URL: u, RequestedURL: u, Content: "原文的事实说明", Status: "ok"}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.Prepare(domain.Submission{Text: question, Note: notes, ForwardedText: forwarded})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Analyze(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 2 || len(result.Readings) != 2 || result.Readings[1].ParentID != "s_root" {
		t.Fatalf("reading graph: %+v", result)
	}
	if result.Usage.TotalTokens != 36 {
		t.Errorf("usage=%+v", result.Usage)
	}
	if calls.Load() != 2 {
		t.Errorf("requests=%d", calls.Load())
	}
	for _, value := range []string{question, notes, profile} {
		if strings.Count(serializedInput, value) != 1 {
			t.Errorf("model input should carry user guidance once, count=%d", strings.Count(serializedInput, value))
		}
	}
	for _, field := range []string{`\"instruction\"`, `\"notes\"`, `\"profile\"`, `\"sources\"`} {
		if !strings.Contains(serializedInput, field) {
			t.Errorf("model input missing explicit field %s", field)
		}
	}
	if result.AnalysisVersion != AnalysisVersion {
		t.Errorf("analysis version = %q, want %q", result.AnalysisVersion, AnalysisVersion)
	}
	if result.Analysis.Headings == nil || result.Analysis.Headings.Summary != "相关简报喵" {
		t.Fatalf("model-generated headings were lost: %+v", result.Analysis.Headings)
	}
	foundForwarded := false
	for _, source := range result.Sources {
		if strings.Contains(source.Content, question) || strings.Contains(source.Content, notes) || strings.Contains(source.Content, profile) {
			t.Fatalf("user guidance became source evidence: %+v", source)
		}
		if source.ReadMethod == "forwarded_text" {
			foundForwarded = source.Status == "supplied" && source.Content == forwarded
		}
	}
	if !foundForwarded {
		t.Fatalf("forwarded text was not preserved as supplied source: %+v", result.Sources)
	}
}

func TestReadingPolicyAndEvidenceValidation(t *testing.T) {
	cfg, _ := config.Load("")
	cfg.Agent.MaxSources = 2
	cfg.Agent.MaxSupplemental = 1
	cfg.Agent.MaxDepth = 1
	e := &Engine{Config: cfg, Read: func(_ context.Context, u string) (domain.Source, error) {
		return domain.Source{ID: u, URL: u, Content: "正文", Status: "ok", Links: []domain.Link{{URL: "https://example.org/child"}, {URL: "https://example.org/other"}}}, nil
	}}
	s := newSession(e)
	s.allowed["https://example.org/root"] = true
	root, err := s.read(context.Background(), readRequest{URL: "https://example.org/root", Role: "entry"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(context.Background(), readRequest{URL: "https://invented.org/", ParentID: root.ID, Role: "primary", Question: "原文"}); err == nil {
		t.Fatal("accepted invented URL")
	}
	child, err := s.read(context.Background(), readRequest{URL: "https://example.org/child", ParentID: root.ID, Role: "context", Question: "补充背景"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(context.Background(), readRequest{URL: "https://example.org/other", ParentID: root.ID, Role: "context", Question: "再读"}); err == nil {
		t.Fatal("budget not enforced")
	}
	if _, err := s.read(context.Background(), readRequest{URL: "https://example.org/other", ParentID: child.ID, Role: "primary", Question: "再读"}); err == nil {
		t.Fatal("depth not enforced")
	}
	a := domain.Analysis{Title: "标题", Overview: "概述", Category: "工程", Summary: []domain.Claim{{Text: "事实", SourceIDs: []string{"never-read"}}}}
	a.Headings = &domain.AnalysisHeadings{Summary: "事实", Discussion: "讨论", Evaluation: "评价", Uncertainties: "限制", Sources: "来源"}
	if err := validate(a, s.sources, cfg.Agent.Categories); err == nil || !strings.Contains(err.Error(), "unread source") {
		t.Fatal("unread source accepted")
	}
	if err := decode(`{"title":"x","surprise":true}`, &a); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := decode(`{} {}`, &a); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	// Stable cache identity ignores receipt time but changes with user and reading preferences.
	sub := domain.Submission{UserID: 1, URLs: []string{"https://example.org/root"}, ReceivedAt: time.Now()}
	first, _ := e.Prepare(sub)
	sub.ReceivedAt = sub.ReceivedAt.Add(time.Hour)
	same, _ := e.Prepare(sub)
	if first.CacheKey != same.CacheKey {
		t.Fatal("receipt timestamp changed cache identity")
	}
	sub.UserID = 2
	other, _ := e.Prepare(sub)
	if first.CacheKey == other.CacheKey {
		t.Fatal("cross-user identity reused")
	}
	sub.UserID = 1
	sub.ForwardedText = "third-party discussion"
	forwarded, _ := e.Prepare(sub)
	if first.CacheKey == forwarded.CacheKey {
		t.Fatal("forwarded source identity did not change cache key")
	}
	var legacy domain.Analysis
	if err := decode(`{"relevance":"legacy preference"}`, &legacy); err != nil || legacy.Relevance != "legacy preference" {
		t.Fatalf("legacy relevance decode = (%q, %v)", legacy.Relevance, err)
	}
}

func TestAnalysisHeadingsValidation(t *testing.T) {
	a := domain.Analysis{
		Headings: &domain.AnalysisHeadings{Summary: "任意摘要标题喵", Discussion: "讨论", Evaluation: "评价", Uncertainties: "限制", Sources: "来源"},
		Title:    "文章", Overview: "事实", Category: "工程",
		Summary: []domain.Claim{{Text: "事实", SourceIDs: []string{"source"}}},
	}
	sources := []domain.Source{{ID: "source", Content: "事实", Status: "ok"}}
	if err := validate(a, sources, []string{"工程"}); err != nil {
		t.Fatal(err)
	}
	for _, heading := range []string{"", "  ", "：", "第一行\n第二行", "第一行\r第二行", strings.Repeat("长", 81)} {
		a.Headings.Summary = heading
		if err := validate(a, sources, []string{"工程"}); err == nil {
			t.Fatalf("invalid heading %q was accepted", heading)
		}
	}
	a.Headings = nil
	if err := validate(a, sources, []string{"工程"}); err == nil {
		t.Fatal("new model result without headings was accepted")
	}
}
