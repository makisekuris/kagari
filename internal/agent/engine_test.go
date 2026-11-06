package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kagari/internal/browser"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/textpage"
)

type testPersona string

func (p testPersona) Prompt() string { return string(p) }

func memorySourceRecorder(_ context.Context, captureKey string, src domain.Source) (domain.SourceRef, error) {
	return domain.SourceRef{ID: "m_" + captureKey, RunID: 1, URL: src.URL, Title: src.Title, Status: src.Status,
		FetchedAt: src.FetchedAt, SourceTruncated: src.Truncated, TotalRunes: len([]rune(src.Content))}, nil
}

func engineConfig(t *testing.T, endpoint string) config.Config {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = endpoint+"/v1", "test-key", "test-model"
	cfg.Model.ContextWindow = 32768
	cfg.ProfilePath = t.TempDir() + "/missing-profile"
	cfg.Agent.MaxSources = 8
	cfg.Agent.MaxIterations = 8
	cfg.Agent.Timeout = 5 * time.Second
	return cfg
}

func responseTool(name, url, callID string, refresh ...bool) []any {
	request := readRequest{URL: url}
	if len(refresh) > 0 {
		request.Refresh = refresh[0]
	}
	args, _ := json.Marshal(request)
	return []any{map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name, "arguments": string(args)}}
}

func responseText(text string) []any {
	return []any{map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}}}
}

func requireTaskToolMode(t *testing.T, request map[string]any) {
	t.Helper()
	textConfig, _ := request["text"].(map[string]any)
	format, _ := textConfig["format"].(map[string]any)
	tools, _ := request["tools"].([]any)
	if format["type"] == "json_schema" || len(tools) == 0 {
		t.Errorf("task agent must expose tools without forcing JSON output: tools=%d format=%v", len(tools), format["type"])
	}
}

func writeResponse(w http.ResponseWriter, call int, output []any) {
	writeResponseWithStatus(w, "completed", output)
}

func writeResponseWithStatus(w http.ResponseWriter, status string, output []any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "resp", "object": "response", "status": status, "model": "fixture", "output": output,
		"usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18},
	})
}

func sendResponseEvent(w http.ResponseWriter, event string, fields map[string]any) {
	fields["type"] = event
	data, _ := json.Marshal(fields)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	w.(http.Flusher).Flush()
}

func streamTextResponse(w http.ResponseWriter, response map[string]any, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	middle := len(text) / 2
	for _, delta := range []string{text[:middle], text[middle:]} {
		sendResponseEvent(w, "response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_1", "delta": delta})
	}
	sendResponseEvent(w, "response.completed", map[string]any{"response": response})
}

func TestTaskAgentFollowsFailedReadWithOtherSourcesAndMarkdown(t *testing.T) {
	const aURL = "https://example.org/a"
	const bURL = "https://example.org/b"
	const cURL = "https://example.org/c"
	var calls atomic.Int32
	var requests []string
	forwardedRecorded := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		serialized, _ := json.Marshal(body)
		requests = append(requests, string(serialized))
		requireTaskToolMode(t, body)
		call := int(calls.Add(1))
		if call == 1 && !forwardedRecorded {
			t.Error("forwarded text was not persisted before the first model request")
		}
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
	}, testPersona("persona fixture"))
	if err != nil {
		t.Fatal(err)
	}
	var observedPages []domain.SourceRef
	engine.OnSourcePage = func(ref domain.SourceRef, _ textpage.Page) {
		observedPages = append(observedPages, ref)
	}
	question := "What does the source say? QUESTION_TOKEN"
	submission, err := engine.Prepare(domain.Submission{Text: question, Note: "NOTE_TOKEN", ForwardedText: "FORWARDED_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	var captureKeys []string
	record := func(ctx context.Context, captureKey string, src domain.Source) (domain.SourceRef, error) {
		captureKeys = append(captureKeys, captureKey)
		if src.Kind == "submitted_text" {
			forwardedRecorded = src.Content == "FORWARDED_TOKEN" && src.Status == "supplied"
		}
		return memorySourceRecorder(ctx, captureKey, src)
	}
	result, err := engine.RunTaskStage(context.Background(), submission, record)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || readCalls != 3 {
		t.Fatalf("calls=%d reads=%d", calls.Load(), readCalls)
	}
	if result.Body != "## Answer\nB and C confirm the result." {
		t.Fatalf("body=%q", result.Body)
	}
	if !result.UsageReported || result.Usage.TotalTokens != 72 {
		t.Fatalf("metadata=%+v", result)
	}
	if len(result.Sources) != 4 || result.Sources[1].Status != "restricted" || len(result.Readings) != 3 {
		t.Fatalf("partial failure or readings were lost: %+v", result)
	}
	if len(result.SourceRefs) != 4 || len(captureKeys) != 4 || uniqueCount(captureKeys) != len(captureKeys) {
		t.Fatalf("persisted source references/capture keys missing or duplicated: refs=%+v keys=%v", result.SourceRefs, captureKeys)
	}
	if len(observedPages) != 4 {
		t.Fatalf("observed %d of 4 first pages", len(observedPages))
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
	if strings.Count(firstInput, "FORWARDED_TOKEN") != 1 {
		t.Fatalf("forwarded body was injected more than once: %s", firstInput)
	}
	if strings.Contains(firstInput, "categories") || strings.Contains(firstInput, "claim_ids") {
		t.Fatalf("input retained final schema contract: %s", firstInput)
	}
}

func uniqueCount(values []string) int {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	return len(seen)
}

func TestTaskStageStreamingAndNonStreaming(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			const bodyText = "  exact body\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if (request["stream"] == true) != streaming {
					t.Errorf("stream=%v, want %t", request["stream"], streaming)
				}
				requireTaskToolMode(t, request)
				output := responseText(bodyText)
				response := map[string]any{"id": "resp", "object": "response", "status": "completed", "model": "fixture", "output": output, "usage": map[string]any{"input_tokens": 4, "output_tokens": 3, "total_tokens": 7}}
				if !streaming {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(response)
					return
				}
				streamTextResponse(w, response, bodyText)
			}))
			defer server.Close()

			cfg := engineConfig(t, server.URL)
			cfg.Agent.Streaming = streaming
			engine, err := New(context.Background(), cfg, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			submission, err := engine.Prepare(domain.Submission{Text: "Question"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := engine.RunTaskStage(context.Background(), submission, memorySourceRecorder)
			if err != nil {
				t.Fatal(err)
			}
			if result.Body != bodyText || result.Usage.TotalTokens != 7 {
				t.Fatalf("body=%q usage=%+v", result.Body, result.Usage)
			}
		})
	}
}

func TestTaskStageStreamingToolRoundTrip(t *testing.T) {
	const target = "https://example.org/source"
	const bodyText = "## Evidence\nThe source supports the claim."
	args, _ := json.Marshal(readRequest{URL: target})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requireTaskToolMode(t, request)
		if request["stream"] != true {
			t.Errorf("stream=%v, want true", request["stream"])
		}
		call := calls.Add(1)
		if call == 1 {
			argsText := string(args)
			item := map[string]any{"type": "function_call", "id": "fc_read", "call_id": "read_1", "name": "read_url", "arguments": argsText, "status": "completed"}
			response := map[string]any{"id": "resp_1", "object": "response", "status": "completed", "model": "fixture", "output": []any{item}, "usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18}}
			w.Header().Set("Content-Type", "text/event-stream")
			sendResponseEvent(w, "response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_read", "call_id": "read_1", "name": "read_url", "arguments": "", "status": "in_progress"}})
			sendResponseEvent(w, "response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "fc_read", "delta": argsText})
			sendResponseEvent(w, "response.output_item.done", map[string]any{"output_index": 0, "item": item})
			sendResponseEvent(w, "response.completed", map[string]any{"response": response})
			return
		}
		input, _ := json.Marshal(request["input"])
		if !strings.Contains(string(input), `"call_id":"read_1"`) || !strings.Contains(string(input), "function_call_output") {
			t.Errorf("tool result lost call ID or output: %s", input)
		}
		output := responseText(bodyText)
		text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		response := map[string]any{"id": "resp_2", "object": "response", "status": "completed", "model": "fixture", "output": output, "usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18}}
		streamTextResponse(w, response, text)
	}))
	defer server.Close()
	cfg := engineConfig(t, server.URL)
	cfg.Agent.Streaming = true
	engine, err := New(context.Background(), cfg, func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{ID: "source", URL: url, RequestedURL: url, Status: "ok", Content: "source content"}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Summarize this source", URLs: []string{target}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.RunTaskStage(context.Background(), submission, memorySourceRecorder)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || result.Body != bodyText || len(result.Readings) != 1 || result.Usage.TotalTokens != 36 {
		t.Fatalf("calls=%d result=%+v", calls.Load(), result)
	}
}

func TestTaskStageRefusalKeepsReadingsAndUsageWithoutPublishingRawOutput(t *testing.T) {
	const target = "https://example.org/source"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requireTaskToolMode(t, request)
		if calls.Add(1) == 1 {
			writeResponse(w, 1, responseTool("read_url", target, "read-source"))
			return
		}
		writeResponse(w, 2, []any{map[string]any{"type": "message", "id": "msg_refusal", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "refusal", "refusal": "upstream private refusal reason"}}}})
	}))
	defer server.Close()
	engine, err := New(context.Background(), engineConfig(t, server.URL), func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{ID: "source", URL: url, RequestedURL: url, Status: "ok", Content: "source content"}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Read and summarize", URLs: []string{target}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.RunTaskStage(context.Background(), submission, memorySourceRecorder)
	if err != errModelResponseRefused {
		t.Fatalf("error=%v", err)
	}
	if result.Body != "" || len(result.Readings) != 1 || result.Usage.TotalTokens != 36 || !result.UsageReported {
		t.Fatalf("refusal result lost metadata or exposed output: %+v", result)
	}
	if strings.Contains(err.Error(), "upstream private refusal reason") {
		t.Fatal("upstream refusal reason leaked in error")
	}
}

func TestTaskStageRejectsIncompleteResponseWithOutput(t *testing.T) {
	for _, tc := range []struct{ status, wantError string }{
		{status: "failed", wantError: "model response failed"},
		{status: "incomplete", wantError: "model response incomplete"},
		{status: "cancelled", wantError: "model response incomplete"},
		{status: "in_progress", wantError: "model response incomplete"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				requireTaskToolMode(t, request)
				writeResponseWithStatus(w, tc.status, responseText("parseable final JSON"))
			}))
			defer server.Close()
			engine, err := New(context.Background(), engineConfig(t, server.URL), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			submission, err := engine.Prepare(domain.Submission{Text: "Question"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := engine.RunTaskStage(context.Background(), submission, memorySourceRecorder)
			if err == nil || err.Error() != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if result.Body != "" || result.Usage.TotalTokens != 18 || !result.UsageReported {
				t.Fatalf("incomplete result lost metadata or exposed output: %+v", result)
			}
		})
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
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	engine.OpenBrowser = func(context.Context) (browser.Browse, func(), error) {
		opened++
		return nil, nil, errors.New("browser startup failed")
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Read this page"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.RunTaskStage(context.Background(), submission, memorySourceRecorder)
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

func TestPrepareValidatesAndNormalizesURLs(t *testing.T) {
	engine := &Engine{Config: config.Config{Agent: config.Agent{MaxSources: 2}}}
	if _, err := engine.Prepare(domain.Submission{}); err == nil {
		t.Fatal("empty submission was accepted")
	}
	prepared, err := engine.Prepare(domain.Submission{Text: "Question", URLs: []string{
		"HTTPS://EXAMPLE.COM/path#fragment", "https://example.com/path", "https://example.net/guide",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.URLs) != 2 || prepared.URLs[0] != "https://example.com/path" || prepared.URLs[1] != "https://example.net/guide" {
		t.Fatalf("URLs were not normalized and deduplicated: %v", prepared.URLs)
	}
	if _, err := engine.Prepare(domain.Submission{Text: "Question", URLs: []string{"javascript:alert(1)"}}); err == nil {
		t.Fatal("non-HTTP URL was accepted")
	}
	engine.Config.Agent.MaxSources = 1
	if _, err := engine.Prepare(domain.Submission{Text: "Question", URLs: []string{"https://example.com/a", "https://example.com/b"}}); err == nil {
		t.Fatal("URL limit was not enforced")
	}
}

type mutablePersona struct{ text string }

func (p *mutablePersona) Prompt() string { return p.text }

func TestPersonaIsFrozenAtEngineConstruction(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = "https://model.test/v1", "test", "test"
	cfg.Model.ContextWindow = 32768
	cfg.ProfilePath = t.TempDir() + "/missing"
	role := &mutablePersona{text: "first-persona"}
	first, err := New(context.Background(), cfg, nil, role)
	if err != nil {
		t.Fatal(err)
	}
	role.text = "second-persona"
	if !strings.Contains(first.taskPrompt(), "first-persona") || strings.Contains(first.taskPrompt(), "second-persona") {
		t.Fatal("a mutable persona changed the running engine's frozen prompt")
	}
	second, err := New(context.Background(), cfg, nil, role)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.taskPrompt(), "second-persona") {
		t.Fatal("new engine did not use the replacement persona")
	}
}

func TestBlankTaskStageResponseFailsSafely(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(w, 1, responseText("  \n "))
	}))
	defer server.Close()
	engine, err := New(context.Background(), engineConfig(t, server.URL), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Question"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.RunTaskStage(context.Background(), submission, memorySourceRecorder)
	if err == nil || err.Error() != "task stage returned an empty response" {
		t.Fatalf("empty answer error=%v", err)
	}
	if result.Body != "" || result.Usage.TotalTokens != 18 || !result.UsageReported {
		t.Fatalf("empty output lost usage: %+v", result)
	}
}

func TestRunTaskStageRequiresSourceRecorder(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeResponse(w, 1, responseText("unexpected"))
	}))
	defer server.Close()
	engine, err := New(context.Background(), engineConfig(t, server.URL), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunTaskStage(context.Background(), domain.Submission{Text: "Question"}, nil); err == nil || err.Error() != "source recorder is required" {
		t.Fatalf("missing source recorder error=%v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("model was called without a source recorder: %d", requests.Load())
	}
}

func TestSourcePersistenceFailureStopsTaskStage(t *testing.T) {
	const target = "https://example.org/source"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			writeResponse(w, 1, responseTool("read_url", target, "read-source"))
			return
		}
		writeResponse(w, 2, responseText("must not continue after save failure"))
	}))
	defer server.Close()
	engine, err := New(context.Background(), engineConfig(t, server.URL), func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{URL: url, Status: "ok", Content: "collected"}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Read this source"})
	if err != nil {
		t.Fatal(err)
	}
	record := func(context.Context, string, domain.Source) (domain.SourceRef, error) {
		return domain.SourceRef{}, errors.New("disk full")
	}
	if _, err := engine.RunTaskStage(context.Background(), submission, record); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("source persistence error was not propagated: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("stage continued to another model turn after persistence failed: requests=%d", requests.Load())
	}
}

func TestReadURLRefreshReturnsNewSavedObservation(t *testing.T) {
	const target = "https://example.org/source"
	var modelRequests atomic.Int32
	var firstID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch modelRequests.Add(1) {
		case 1:
			writeResponse(w, 1, responseTool("read_url", target, "first"))
		case 2:
			first := sourceReplyFromInput(t, request, "first")
			firstID = first.Source.ID
			writeResponse(w, 2, responseTool("read_url", target, "cached"))
		case 3:
			cached := sourceReplyFromInput(t, request, "cached")
			if cached.Source.ID != firstID {
				t.Errorf("cached URL changed material ID: first=%s cached=%s", firstID, cached.Source.ID)
			}
			writeResponse(w, 3, responseTool("read_url", target, "refresh", true))
		case 4:
			refreshed := sourceReplyFromInput(t, request, "refresh")
			if refreshed.Source.ID == firstID || refreshed.Page.Text != "observation 2" {
				t.Errorf("refresh did not return a new observation: first=%s refreshed=%+v", firstID, refreshed)
			}
			writeResponse(w, 4, responseText("completed"))
		default:
			t.Errorf("unexpected model call %d", modelRequests.Load())
			writeResponse(w, int(modelRequests.Load()), responseText("unexpected"))
		}
	}))
	defer server.Close()
	var readCalls atomic.Int32
	engine, err := New(context.Background(), engineConfig(t, server.URL), func(_ context.Context, url string) (domain.Source, error) {
		call := readCalls.Add(1)
		return domain.Source{URL: url, Status: "ok", Content: fmt.Sprintf("observation %d", call)}, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Read this source"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.RunTaskStage(context.Background(), submission, memorySourceRecorder)
	if err != nil {
		t.Fatal(err)
	}
	if modelRequests.Load() != 4 || readCalls.Load() != 2 || len(result.Sources) != 2 || len(result.SourceRefs) != 2 || len(result.Readings) != 2 {
		t.Fatalf("refresh/cache accounting: model_requests=%d read_calls=%d result=%+v", modelRequests.Load(), readCalls.Load(), result)
	}
}

func sourceReplyFromInput(t *testing.T, request map[string]any, callID string) readReply {
	t.Helper()
	input, _ := request["input"].([]any)
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call_output" || item["call_id"] != callID {
			continue
		}
		parts, _ := item["output"].([]any)
		if len(parts) == 0 {
			t.Fatalf("tool output %q has no content", callID)
		}
		part, _ := parts[0].(map[string]any)
		text, _ := part["text"].(string)
		var reply readReply
		if err := json.Unmarshal([]byte(text), &reply); err != nil {
			t.Fatalf("decode tool output %q: %v; output=%s", callID, err, text)
		}
		return reply
	}
	t.Fatalf("model input omitted tool output %q", callID)
	return readReply{}
}
