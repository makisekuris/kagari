package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
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

func responseAnalysis(kind domain.ResultKind, body string) []any {
	encoded, _ := json.Marshal(struct {
		Kind domain.ResultKind `json:"kind"`
		Body string            `json:"body"`
	}{Kind: kind, Body: body})
	return responseText(string(encoded))
}

func requireAnalysisSchema(t *testing.T, request map[string]any) {
	t.Helper()
	textConfig, _ := request["text"].(map[string]any)
	format, _ := textConfig["format"].(map[string]any)
	schema, _ := format["schema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	kind, _ := properties["kind"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "analysis_output" || format["strict"] != true ||
		schema["type"] != "object" || schema["additionalProperties"] != false ||
		!reflect.DeepEqual(schema["required"], []any{"kind", "body"}) ||
		!reflect.DeepEqual(kind["enum"], []any{"analysis", "chat"}) {
		t.Errorf("analysis request omitted the strict output schema: %v", request["text"])
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
		requireAnalysisSchema(t, body)
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
			writeResponse(w, call, responseAnalysis(domain.ResultKindAnalysis, "## Answer\nB and C confirm the result."))
		default:
			t.Errorf("unexpected model call %d", call)
			writeResponse(w, call, responseAnalysis(domain.ResultKindAnalysis, "unexpected"))
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
	if result.Kind != domain.ResultKindAnalysis || result.Body != "## Answer\nB and C confirm the result." {
		t.Fatalf("kind=%q body=%q", result.Kind, result.Body)
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

func TestAnalyzeStructuredOutputStreamingAndNonStreaming(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, kind := range []domain.ResultKind{domain.ResultKindAnalysis, domain.ResultKindChat} {
			t.Run(fmt.Sprintf("streaming=%t/%s", streaming, kind), func(t *testing.T) {
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
					requireAnalysisSchema(t, request)
					output := responseAnalysis(kind, bodyText)
					response := map[string]any{"id": "resp", "object": "response", "status": "completed", "model": "fixture", "output": output, "usage": map[string]any{"input_tokens": 4, "output_tokens": 3, "total_tokens": 7}}
					if !streaming {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(response)
						return
					}
					text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
					streamTextResponse(w, response, text)
				}))
				defer server.Close()

				cfg := engineConfig(t, server.URL)
				cfg.Agent.Streaming = streaming
				engine, err := New(context.Background(), cfg, nil, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				submission, err := engine.Prepare(domain.Submission{Text: "Question"})
				if err != nil {
					t.Fatal(err)
				}
				result, err := engine.Analyze(context.Background(), submission)
				if err != nil {
					t.Fatal(err)
				}
				if result.Kind != kind || result.Body != bodyText || result.Usage.TotalTokens != 7 {
					t.Fatalf("kind=%q body=%q usage=%+v", result.Kind, result.Body, result.Usage)
				}
			})
		}
	}
}

func TestAnalyzeStreamingToolRoundTripUsesStructuredOutput(t *testing.T) {
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
		requireAnalysisSchema(t, request)
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
		output := responseAnalysis(domain.ResultKindAnalysis, bodyText)
		text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		response := map[string]any{"id": "resp_2", "object": "response", "status": "completed", "model": "fixture", "output": output, "usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18}}
		streamTextResponse(w, response, text)
	}))
	defer server.Close()
	cfg := engineConfig(t, server.URL)
	cfg.Agent.Streaming = true
	engine, err := New(context.Background(), cfg, func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{ID: "source", URL: url, RequestedURL: url, Status: "ok", Content: "source content"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Summarize this source", URLs: []string{target}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Analyze(context.Background(), submission)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || result.Kind != domain.ResultKindAnalysis || result.Body != bodyText || len(result.Readings) != 1 || result.Usage.TotalTokens != 36 {
		t.Fatalf("calls=%d result=%+v", calls.Load(), result)
	}
}

func TestAnalyzeRefusalKeepsReadingsAndUsageWithoutPublishingRawOutput(t *testing.T) {
	const target = "https://example.org/source"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requireAnalysisSchema(t, request)
		if calls.Add(1) == 1 {
			writeResponse(w, 1, responseTool("read_url", target, "read-source"))
			return
		}
		writeResponse(w, 2, []any{map[string]any{"type": "message", "id": "msg_refusal", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "refusal", "refusal": "upstream private refusal reason"}}}})
	}))
	defer server.Close()
	engine, err := New(context.Background(), engineConfig(t, server.URL), func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{ID: "source", URL: url, RequestedURL: url, Status: "ok", Content: "source content"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := engine.Prepare(domain.Submission{Text: "Read and summarize", URLs: []string{target}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Analyze(context.Background(), submission)
	if err != errModelResponseRefused {
		t.Fatalf("error=%v", err)
	}
	if result.Kind != "" || result.Body != "" || len(result.Readings) != 1 || result.Usage.TotalTokens != 36 || !result.UsageReported {
		t.Fatalf("refusal result lost metadata or exposed output: %+v", result)
	}
	if strings.Contains(err.Error(), "upstream private refusal reason") {
		t.Fatal("upstream refusal reason leaked in error")
	}
}

func TestAnalyzeRejectsIncompleteResponseWithParseableJSON(t *testing.T) {
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
				requireAnalysisSchema(t, request)
				writeResponseWithStatus(w, tc.status, responseAnalysis(domain.ResultKindAnalysis, "parseable final JSON"))
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
			result, err := engine.Analyze(context.Background(), submission)
			if err == nil || err.Error() != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if result.Kind != "" || result.Body != "" || result.Usage.TotalTokens != 18 || !result.UsageReported {
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
			writeResponse(w, 3, responseAnalysis(domain.ResultKindChat, "The browser was unavailable; HTTP supplied the page."))
		default:
			t.Errorf("unexpected request")
			writeResponse(w, 4, responseAnalysis(domain.ResultKindAnalysis, "unexpected"))
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
	if result.Kind != domain.ResultKindChat || result.Body == "" || len(result.Readings) != 2 || result.Readings[0].Backend != browserBackend || result.Readings[1].Backend != httpBackend {
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

func TestBlankStructuredBodyFailsSafely(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(w, 1, responseAnalysis(domain.ResultKindAnalysis, "  \n "))
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
	result, err := engine.Analyze(context.Background(), submission)
	if err == nil || err != errInvalidAnalysisOutput {
		t.Fatalf("empty answer error=%v", err)
	}
	if result.Kind != "" || result.Body != "" || result.Usage.TotalTokens != 18 || !result.UsageReported {
		t.Fatalf("invalid output lost usage or exposed raw JSON: %+v", result)
	}
}
