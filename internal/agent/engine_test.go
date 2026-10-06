package agent

import (
	"context"
	"encoding/json"
	"errors"
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

type testPersona string

func (p testPersona) Prompt() string { return string(p) }

func engineConfig(t *testing.T, endpoint string) config.Config {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = endpoint+"/v1", "test-key", "test-model"
	cfg.ProfilePath = t.TempDir() + "/missing-profile"
	cfg.Agent.MaxSources = 8
	cfg.Agent.MaxIterations = 8
	cfg.Agent.Timeout = 5 * time.Second
	return cfg
}

func responseTool(name, url, callID string) []any {
	args, _ := json.Marshal(readRequest{URL: url})
	return []any{map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name, "arguments": string(args)}}
}

func responseText(text string) []any {
	return []any{map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}}}
}

func writeResponse(w http.ResponseWriter, call int, output []any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "resp", "object": "response", "status": "completed", "model": "fixture", "output": output,
		"usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18},
	})
}

func TestResponsesAgentFollowsFailedReadWithOtherSourcesAndMarkdown(t *testing.T) {
	const aURL = "https://example.org/a"
	const bURL = "https://example.org/b"
	const cURL = "https://example.org/c"
	var calls atomic.Int32
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		serialized, _ := json.Marshal(body)
		requests = append(requests, string(serialized))
		if body["text"] != nil {
			t.Errorf("final JSON format unexpectedly constrained: %v", body["text"])
		}
		call := int(calls.Add(1))
		if call > 1 {
			input, _ := json.Marshal(body["input"])
			if !strings.Contains(string(input), "function_call_output") {
				t.Errorf("tool output missing from turn %d", call)
			}
		}
		switch call {
		case 1:
			writeResponse(w, call, responseTool("read_url", aURL, "read-a"))
		case 2:
			input, _ := json.Marshal(body["input"])
			if !strings.Contains(string(input), "HTTP 403") {
				t.Errorf("failed read was not returned to the model: %s", input)
			}
			writeResponse(w, call, responseTool("read_url", bURL, "read-b"))
		case 3:
			input, _ := json.Marshal(body["input"])
			if !strings.Contains(string(input), "B has a link to C") {
				t.Errorf("usable source was not returned to the model: %s", input)
			}
			writeResponse(w, call, responseTool("read_url", cURL, "read-c"))
		case 4:
			writeResponse(w, call, responseText("## Answer\nB and C confirm the result."))
		default:
			t.Errorf("unexpected model call %d", call)
			writeResponse(w, call, responseText("unexpected"))
		}
	}))
	defer server.Close()

	cfg := engineConfig(t, server.URL)
	profile := "Use concise English."
	if err := os.WriteFile(cfg.ProfilePath, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	readCalls := 0
	engine, err := New(context.Background(), cfg, func(_ context.Context, u string) (domain.Source, error) {
		readCalls++
		switch u {
		case aURL:
			return domain.Source{ID: "a", URL: u, RequestedURL: u, Status: "restricted", Reason: "HTTP 403"}, errors.New("forbidden")
		case bURL:
			return domain.Source{ID: "b", URL: u, RequestedURL: u, Status: "ok", Content: "B has a link to C", Links: []domain.Link{{URL: cURL, Text: "C"}}}, nil
		case cURL:
			return domain.Source{ID: "c", URL: u, RequestedURL: u, Status: "ok", Content: "C confirms the random value 4711"}, nil
		default:
			return domain.Source{}, errors.New("unexpected URL")
		}
	}, nil, testPersona("persona fixture"))
	if err != nil {
		t.Fatal(err)
	}
	question := "What does the source say? QUESTION_TOKEN"
	submission, err := engine.Prepare(domain.Submission{Text: question, Note: "NOTE_TOKEN", ForwardedText: "FORWARDED_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Analyze(context.Background(), submission)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || readCalls != 3 {
		t.Fatalf("calls=%d reads=%d", calls.Load(), readCalls)
	}
	if result.Body != "## Answer\nB and C confirm the result." {
		t.Fatalf("body=%q", result.Body)
	}
	if !result.UsageReported || result.Usage.TotalTokens != 72 || result.AnalysisVersion != AnalysisVersion {
		t.Fatalf("metadata=%+v", result)
	}
	if len(result.Sources) != 4 || result.Sources[1].Status != "restricted" || len(result.Readings) != 3 {
		t.Fatalf("partial failure or readings were lost: %+v", result)
	}
	if result.Readings[0].URL != aURL || result.Readings[2].URL != cURL {
		t.Fatalf("unexpected readings: %+v", result.Readings)
	}
	firstInput := requests[0]
	for _, want := range []string{question, "NOTE_TOKEN", "FORWARDED_TOKEN", profile, "persona fixture", "read_url"} {
		if !strings.Contains(firstInput, want) {
			t.Errorf("initial request omitted %q", want)
		}
	}
	if strings.Contains(firstInput, "categories") || strings.Contains(firstInput, "claim_ids") {
		t.Fatalf("input retained final schema contract: %s", firstInput)
	}
}

func TestBrowserIsOpenedLazilyAndCanRetrySameURLWithHTTP(t *testing.T) {
	const target = "https://example.org/page"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		switch calls.Add(1) {
		case 1:
			tools, _ := body["tools"].([]any)
			names := make(map[string]bool)
			for _, raw := range tools {
				definition, _ := raw.(map[string]any)
				name, _ := definition["name"].(string)
				names[name] = true
			}
			if !names["read_url"] || !names["browse_url"] {
				t.Errorf("both HTTP and browser tools must be available: %v", names)
			}
			writeResponse(w, 1, responseTool("browse_url", target, "browse"))
		case 2:
			writeResponse(w, 2, responseTool("read_url", target, "http"))
		case 3:
			writeResponse(w, 3, responseText("The browser was unavailable; HTTP supplied the page."))
		default:
			t.Errorf("unexpected request")
			writeResponse(w, 4, responseText("unexpected"))
		}
	}))
	defer server.Close()
	cfg := engineConfig(t, server.URL)
	cfg.Agent.MaxSources = 2
	engine, err := New(context.Background(), cfg, func(_ context.Context, u string) (domain.Source, error) {
		return domain.Source{ID: "http-page", URL: u, RequestedURL: u, Status: "ok", Content: "read with HTTP"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	engine.OpenBrowser = func(context.Context) (func(context.Context, string) (domain.Source, error), func(), error) {
		opened++
		return nil, nil, errors.New("browser startup failed")
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Read this page"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Analyze(context.Background(), submission)
	if err != nil {
		t.Fatal(err)
	}
	if opened != 1 || calls.Load() != 3 {
		t.Fatalf("browser opens=%d model calls=%d", opened, calls.Load())
	}
	if result.Body == "" || len(result.Readings) != 2 || result.Readings[0].Backend != browserBackend || result.Readings[1].Backend != httpBackend {
		t.Fatalf("same-URL retry was not recorded: %+v", result)
	}
	if len(result.Sources) != 2 || result.Sources[0].Status != "failed" || !result.Sources[1].Usable() {
		t.Fatalf("browser failure was not preserved: %+v", result.Sources)
	}
}

func TestPrepareAllowsTextOnlyAndKeepsCacheIdentityScoped(t *testing.T) {
	engine := &Engine{Config: config.Config{Agent: config.Agent{MaxSources: 4}}}
	if _, err := engine.Prepare(domain.Submission{}); err == nil {
		t.Fatal("empty submission was accepted")
	}
	sub := domain.Submission{UserID: 1, Text: "Question", ReceivedAt: time.Now()}
	first, err := engine.Prepare(sub)
	if err != nil {
		t.Fatal(err)
	}
	sub.ReceivedAt = sub.ReceivedAt.Add(time.Hour)
	same, err := engine.Prepare(sub)
	if err != nil || first.CacheKey != same.CacheKey {
		t.Fatalf("receipt time changed cache key: %v", err)
	}
	sub.UserID = 2
	other, _ := engine.Prepare(sub)
	if first.CacheKey == other.CacheKey {
		t.Fatal("cross-user cache identity reused")
	}
	sub.UserID = 1
	sub.Note = "different preference"
	other, _ = engine.Prepare(sub)
	if first.CacheKey == other.CacheKey {
		t.Fatal("different user guidance reused cache identity")
	}
	sub.Note = ""
	engine.Config.Browser.Enabled = true
	other, _ = engine.Prepare(sub)
	if first.CacheKey == other.CacheKey {
		t.Fatal("browser configuration did not change cache identity")
	}
}

type mutablePersona struct{ text string }

func (p *mutablePersona) Prompt() string { return p.text }

func TestPersonaIsFrozenAndChangesCacheIdentity(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = "https://model.test/v1", "test", "test"
	cfg.ProfilePath = t.TempDir() + "/missing"
	role := &mutablePersona{text: "first-persona"}
	first, err := New(context.Background(), cfg, nil, nil, role)
	if err != nil {
		t.Fatal(err)
	}
	sub := domain.Submission{UserID: 7, Text: "question"}
	before, err := first.Prepare(sub)
	if err != nil {
		t.Fatal(err)
	}
	role.text = "second-persona"
	after, err := first.Prepare(sub)
	if err != nil {
		t.Fatal(err)
	}
	if before.CacheKey != after.CacheKey || !strings.Contains(first.DigestPrompt(), "first-persona") {
		t.Fatal("a mutable persona changed the running engine's frozen prompts")
	}
	second, err := New(context.Background(), cfg, nil, nil, role)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := second.Prepare(sub)
	if err != nil {
		t.Fatal(err)
	}
	if changed.CacheKey == before.CacheKey || !strings.Contains(second.DigestPrompt(), "second-persona") {
		t.Fatal("replacing persona did not change prompts and cache identity")
	}
}

func TestEmptyModelAnswerFailsSafely(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(w, 1, responseText("  \n "))
	}))
	defer server.Close()
	engine, err := New(context.Background(), engineConfig(t, server.URL), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Question"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Analyze(context.Background(), submission)
	if err == nil || err.Error() != "empty analysis output" {
		t.Fatalf("empty answer error=%v", err)
	}
}
