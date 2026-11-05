package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"kagari/internal/domain"
	"kagari/internal/logging"
)

// Exercise the actual provider adapter: Eino emits both graph and component
// callbacks, so handler-only tests cannot detect duplicate request lifecycles.
func TestTaskStageCallbacksHaveOneCorrelatedLifecycle(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeResponse(w, requests, responseText("complete analysis body"))
	}))
	defer server.Close()

	cfg := engineConfig(t, server.URL)
	engine, err := New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.Log = zap.New(core)
	submission, err := engine.Prepare(domain.Submission{Text: "full question"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := logging.WithLogger(context.Background(), engine.Log.With(zap.Int64("event_id", 31), zap.Int64("task_id", 41), zap.Int64("run_id", 51), zap.Int64("revision", 2)))
	result, err := engine.RunTaskStage(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	if result.Body != "complete analysis body" || requests != 1 {
		t.Fatalf("result=%+v model_requests=%d", result, requests)
	}
	entries := logs.All()
	started, finished := findAgentEvent(t, entries, "model.started"), findAgentEvent(t, entries, "model.finished")
	if started.ContextMap()["model_call_id"] != finished.ContextMap()["model_call_id"] || started.ContextMap()["phase"] != "task_stage" {
		t.Fatalf("model lifecycle correlation missing: start=%v finish=%v", started.ContextMap(), finished.ContextMap())
	}
	for key, expected := range map[string]int64{"event_id": 31, "task_id": 41, "run_id": 51, "revision": 2} {
		if started.ContextMap()[key] != expected {
			t.Fatalf("model event %s = %v, want %d", key, started.ContextMap()[key], expected)
		}
	}
	if findAgentEventOptional(entries, "agent message") != nil {
		t.Fatal("task stage logged duplicate model content")
	}
}

func TestTaskStageProviderFailureHasOneCorrelatedLifecycle(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"raw response body secret","type":"server_error","code":"fixture_error"}}`))
	}))
	defer server.Close()

	cfg := engineConfig(t, server.URL)
	engine, err := New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.Log = zap.New(core)
	submission, err := engine.Prepare(domain.Submission{Text: "fail once"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := logging.WithLogger(context.Background(), engine.Log.With(zap.Int64("event_id", 61)))
	if _, err := engine.RunTaskStage(ctx, submission); err == nil {
		t.Fatal("RunTaskStage unexpectedly succeeded")
	}
	if requests.Load() != 1 {
		t.Fatalf("model requests=%d, want one", requests.Load())
	}
	entries := logs.All()
	starts, failures := agentEvents(entries, "model.started"), agentEvents(entries, "model.failed")
	if len(starts) != 1 || len(failures) != 1 {
		t.Fatalf("provider error lifecycle = starts %d failures %d; entries=%v", len(starts), len(failures), entries)
	}
	if starts[0].ContextMap()["model_call_id"] != failures[0].ContextMap()["model_call_id"] || failures[0].ContextMap()["event_id"] != int64(61) {
		t.Fatalf("provider failure correlation = start %v, failure %v", starts[0].ContextMap(), failures[0].ContextMap())
	}
	encoded, _ := json.Marshal(failures[0].ContextMap())
	if strings.Contains(string(encoded), "raw response body secret") {
		t.Fatalf("raw provider error body leaked: %s", encoded)
	}
}

func TestTaskStageStreamingUsesOneModelLifecycle(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	var requests atomic.Int32
	const body = "complete streamed response"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		output := responseText(body)
		response := map[string]any{"id": "resp_stream", "object": "response", "status": "completed", "model": "fixture", "output": output, "usage": map[string]any{"input_tokens": 4, "output_tokens": 3, "total_tokens": 7}}
		text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		streamTextResponse(w, response, text)
	}))
	defer server.Close()

	cfg := engineConfig(t, server.URL)
	cfg.Agent.Streaming = true
	engine, err := New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.Log = zap.New(core)
	submission, err := engine.Prepare(domain.Submission{Text: "stream once"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := logging.WithLogger(context.Background(), engine.Log.With(zap.Int64("event_id", 62)))
	result, err := engine.RunTaskStage(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	if result.Body != body || requests.Load() != 1 {
		t.Fatalf("result body=%q requests=%d", result.Body, requests.Load())
	}
	entries := logs.All()
	starts, finishes := agentEvents(entries, "model.started"), agentEvents(entries, "model.finished")
	if len(starts) != 1 || len(finishes) != 1 || starts[0].ContextMap()["model_call_id"] != finishes[0].ContextMap()["model_call_id"] {
		t.Fatalf("stream callback lifecycle duplicated or uncorrelated: starts=%v finishes=%v", starts, finishes)
	}
	payload, _ := json.Marshal(finishes[0].ContextMap()["payload"])
	if !strings.Contains(string(payload), body) || finishes[0].ContextMap()["usage_reported"] != true {
		t.Fatalf("full stream output or usage missing: fields=%v payload=%s", finishes[0].ContextMap(), payload)
	}
	if findAgentEventOptional(entries, "agent sse output") != nil || findAgentEventOptional(entries, "agent message") != nil {
		t.Fatal("streaming task stage emitted duplicate content logs")
	}
}

// This Eino/OpenAI roundtrip checks the real tool callback contract and cached call IDs.
func TestReadToolOutcomesUseSourceUsabilityAndKeepCachedToolOutput(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	const (
		restrictedURL = "https://example.org/blocked"
		emptyURL      = "https://example.org/empty"
		partialURL    = "https://example.org/partial"
	)
	var modelRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		call := int(modelRequests.Add(1))
		switch call {
		case 1:
			writeResponse(w, call, responseTool("read_url", restrictedURL, "restricted-first"))
		case 2:
			assertReadToolOutput(t, request, "restricted-first", "restricted", "", "HTTP 403", false)
			writeResponse(w, call, responseTool("read_url", restrictedURL, "restricted-cached"))
		case 3:
			assertReadToolOutput(t, request, "restricted-cached", "restricted", "", "HTTP 403", false)
			writeResponse(w, call, responseTool("read_url", emptyURL, "empty-body"))
		case 4:
			assertReadToolOutput(t, request, "empty-body", "ok", "  ", "", false)
			writeResponse(w, call, responseTool("read_url", partialURL, "truncated-content"))
		case 5:
			assertReadToolOutput(t, request, "truncated-content", "incomplete", "usable excerpt", "", true)
			writeResponse(w, call, responseText("The source was unavailable; a partial excerpt was available."))
		default:
			t.Errorf("unexpected model request %d", call)
			writeResponse(w, call, responseText("unexpected"))
		}
	}))
	defer server.Close()

	readCalls := map[string]int{}
	cfg := engineConfig(t, server.URL)
	engine, err := New(context.Background(), cfg, func(_ context.Context, url string) (domain.Source, error) {
		readCalls[url]++
		switch url {
		case restrictedURL:
			return domain.Source{ID: "restricted", URL: url, RequestedURL: url, Status: "restricted", Reason: "HTTP 403"}, errors.New("HTTP 403")
		case emptyURL:
			return domain.Source{ID: "empty", URL: url, RequestedURL: url, Status: "ok", Content: "  "}, nil
		case partialURL:
			return domain.Source{ID: "partial", URL: url, RequestedURL: url, Status: "incomplete", Truncated: true, Content: "usable excerpt"}, nil
		default:
			return domain.Source{}, errors.New("unexpected URL")
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.Log = zap.New(core)
	submission, err := engine.Prepare(domain.Submission{Text: "Read these pages"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.RunTaskStage(logging.WithLogger(context.Background(), engine.Log), submission)
	if err != nil {
		t.Fatal(err)
	}
	if result.Body != "The source was unavailable; a partial excerpt was available." || modelRequests.Load() != 5 {
		t.Fatalf("result=%+v model_requests=%d", result, modelRequests.Load())
	}
	if readCalls[restrictedURL] != 1 || readCalls[emptyURL] != 1 || readCalls[partialURL] != 1 {
		t.Fatalf("reader calls=%v; cached failure should not trigger another read", readCalls)
	}
	starts, finishes := agentEvents(logs.All(), "tool.started"), agentEvents(logs.All(), "tool.finished")
	wantIDs := []string{"restricted-first", "restricted-cached", "empty-body", "truncated-content"}
	wantOutcomes := []string{"business_failure", "business_failure", "business_failure", "success"}
	if len(starts) != len(wantIDs) || len(finishes) != len(wantIDs) {
		t.Fatalf("tool lifecycle counts: started=%d finished=%d", len(starts), len(finishes))
	}
	for i, id := range wantIDs {
		if starts[i].ContextMap()["tool_call_id"] != id || finishes[i].ContextMap()["tool_call_id"] != id || finishes[i].ContextMap()["tool_name"] != "read_url" || finishes[i].ContextMap()["outcome"] != wantOutcomes[i] {
			t.Errorf("tool event %d start=%v finish=%v", i, starts[i].ContextMap(), finishes[i].ContextMap())
		}
	}
}

func assertReadToolOutput(t *testing.T, request map[string]any, callID, status, content, reason string, truncated bool) {
	t.Helper()
	input, ok := request["input"].([]any)
	if !ok {
		t.Errorf("model request input has type %T", request["input"])
		return
	}
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call_output" || item["call_id"] != callID {
			continue
		}
		parts, _ := item["output"].([]any)
		if len(parts) == 0 {
			t.Errorf("tool output %q has no content: %v", callID, item)
			return
		}
		part, _ := parts[0].(map[string]any)
		text, _ := part["text"].(string)
		var reply struct {
			Source struct {
				Status    string `json:"status"`
				Content   string `json:"content"`
				Reason    string `json:"reason"`
				Truncated bool   `json:"truncated"`
			} `json:"source"`
		}
		if err := json.Unmarshal([]byte(text), &reply); err != nil {
			t.Errorf("decode tool output %q: %v; output=%s", callID, err, text)
			return
		}
		if reply.Source.Status != status || reply.Source.Content != content || reply.Source.Reason != reason || reply.Source.Truncated != truncated {
			t.Errorf("tool output %q source changed: %+v", callID, reply.Source)
		}
		return
	}
	t.Errorf("model request omitted tool output %q", callID)
}

// A provider SSE error must preserve prior output chunks in the real adapter callback.
func TestTaskStageStreamingErrorRetainsPartialOutput(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		sendResponseEvent(w, "response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_1", "delta": "partial output before failure"})
		sendResponseEvent(w, "error", map[string]any{"code": "fixture_error", "message": "upstream failed"})
	}))
	defer server.Close()
	cfg := engineConfig(t, server.URL)
	cfg.Agent.Streaming = true
	engine, err := New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.Log = zap.New(core)
	submission, err := engine.Prepare(domain.Submission{Text: "Stream this"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunTaskStage(logging.WithLogger(context.Background(), engine.Log), submission); err == nil {
		t.Fatal("streaming provider error was swallowed")
	}
	starts, failures := logs.FilterMessage("model.started").All(), logs.FilterMessage("model.failed").All()
	finishes := logs.FilterMessage("model.finished").All()
	if requests.Load() != 1 || len(starts) != 1 || len(failures) != 1 || len(finishes) != 0 {
		t.Fatalf("requests=%d starts=%d failures=%d finishes=%d", requests.Load(), len(starts), len(failures), len(finishes))
	}
	if starts[0].ContextMap()["model_call_id"] != failures[0].ContextMap()["model_call_id"] {
		t.Fatalf("stream failure lost model call ID: start=%v failure=%v", starts[0].ContextMap(), failures[0].ContextMap())
	}
	payload, _ := json.Marshal(failures[0].ContextMap()["payload"])
	if !strings.Contains(string(payload), "partial output before failure") {
		t.Fatalf("partial output missing: %s", payload)
	}
}

func findAgentEvent(t *testing.T, entries []observer.LoggedEntry, message string) observer.LoggedEntry {
	t.Helper()
	if entry := findAgentEventOptional(entries, message); entry != nil {
		return *entry
	}
	t.Fatalf("event %q not found: %v", message, entries)
	return observer.LoggedEntry{}
}

func findAgentEventOptional(entries []observer.LoggedEntry, message string) *observer.LoggedEntry {
	for _, entry := range entries {
		if entry.Message == message {
			return &entry
		}
	}
	return nil
}

func agentEvents(entries []observer.LoggedEntry, message string) []observer.LoggedEntry {
	var found []observer.LoggedEntry
	for _, entry := range entries {
		if entry.Message == message {
			found = append(found, entry)
		}
	}
	return found
}
