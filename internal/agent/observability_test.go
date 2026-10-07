package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"kagari/internal/config"
	"kagari/internal/domain"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestAgentMessagesAndFinalLogs(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			logger := zap.New(core)
			var calls atomic.Int32
			args := `{"url":"https://example.org/article"}`
			raw := "## 答案\n原文支持这个结论。"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if (request["stream"] == true) != streaming {
					t.Errorf("request stream=%v, want %t", request["stream"], streaming)
				}
				call := calls.Add(1)
				if call > 1 {
					input, _ := json.Marshal(request["input"])
					if !strings.Contains(string(input), `"call_id":"read_1"`) || !strings.Contains(string(input), "function_call_output") {
						t.Error("tool result lost call_id")
					}
				}
				response := map[string]any{
					"id": fmt.Sprintf("resp_%d", call), "object": "response", "status": "completed", "model": "fixture",
					"usage": map[string]any{
						"input_tokens": 11, "output_tokens": 7, "total_tokens": 18,
						"input_tokens_details":  map[string]any{"cached_tokens": 3},
						"output_tokens_details": map[string]any{"reasoning_tokens": 2},
					},
				}
				if call == 1 {
					item := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "read_1", "name": "read_url", "arguments": args, "status": "completed"}
					reason := map[string]any{"id": "reason_1", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "需要阅读原文"}}}
					response["output"] = []any{reason, item}
					if streaming {
						writeSSEHeader(w)
						sendSSE(t, w, "response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "reason_1", "type": "reasoning", "summary": []any{}}})
						sendSSE(t, w, "response.reasoning_summary_text.delta", map[string]any{"output_index": 0, "summary_index": 0, "item_id": "reason_1", "delta": "需要阅读原文"})
						sendSSE(t, w, "response.output_item.added", map[string]any{"output_index": 1, "item": functionItem("fc_1", "read_1", args, "in_progress")})
						sendSSE(t, w, "response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_1", "delta": args[:20]})
						sendSSE(t, w, "response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_1", "delta": args[20:]})
						sendSSE(t, w, "response.output_item.done", map[string]any{"output_index": 1, "item": item})
						sendSSE(t, w, "response.completed", map[string]any{"response": response})
						return
					}
				} else {
					response["output"] = responseText(raw)
				}
				if streaming {
					writeSSEHeader(w)
					cut := strings.Index(raw, "原文")
					for _, delta := range []string{raw[:cut], raw[cut:]} {
						sendSSE(t, w, "response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_1", "delta": delta})
					}
					sendSSE(t, w, "response.completed", map[string]any{"response": response})
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			cfg, err := config.Load("")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Agent.Streaming = streaming
			cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = server.URL+"/v1", "credential_fixture", "fixture"
			cfg.ProfilePath = t.TempDir() + "/missing"
			engine, err := New(context.Background(), cfg, func(_ context.Context, u string) (domain.Source, error) {
				return domain.Source{ID: "article", URL: u, RequestedURL: u, Status: "ok", Content: strings.Repeat("完整原文 ", 100)}, nil
			}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			engine.Log = logger
			result, err := engine.Analyze(context.Background(), domain.Submission{Text: "读取这篇文章", URLs: []string{"https://example.org/article"}})
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || len(result.Readings) != 1 || result.Usage.TotalTokens != 36 || result.Body != raw {
				t.Fatalf("wrong tool round trip: %+v calls=%d", result, calls.Load())
			}
			for _, name := range []string{"agent toolcall", "agent toolcall result", "agent token usage", "agent analyze finished"} {
				if logs.FilterMessage(name).Len() == 0 {
					t.Errorf("missing info log %s", name)
				}
			}
			if logs.FilterMessage("agent sse output").Len() != 0 {
				t.Error("per-chunk SSE logging is disabled")
			}
			if !streaming && logs.FilterMessage("agent reason").Len() == 0 {
				t.Error("non-stream response omitted reasoning logs")
			}
			toolCall := logs.FilterMessage("agent toolcall").All()[0].ContextMap()
			if toolCall["call_id"] != "read_1" || toolCall["name"] != "read_url" || toolCall["arguments"] != args {
				t.Errorf("partial/missing tool call: %v", toolCall)
			}
			for _, entry := range logs.FilterMessage("agent toolcall result").All() {
				preview := entry.ContextMap()["result"].(string)
				if utf8.RuneCountInString(preview) > 200 || strings.Count(preview, "\n") > 3 {
					t.Errorf("tool result exceeds preview limit: %q", preview)
				}
				if entry.ContextMap()["call_id"] != "read_1" {
					t.Errorf("tool result lost ID: %v", entry.ContextMap())
				}
			}
			if !streaming && logs.FilterMessage("agent message").Len() == 0 {
				t.Error("non-stream response omitted final message log")
			}
			for _, entry := range logs.FilterMessage("agent token usage").All() {
				usage := entry.ContextMap()
				if usage["input_tokens"] != int64(11) || usage["output_tokens"] != int64(7) || usage["total_tokens"] != int64(18) || usage["cached_input_tokens"] != int64(3) || usage["reasoning_tokens"] != int64(2) {
					t.Errorf("token usage details were lost: %v", usage)
				}
			}
		})
	}
}

func TestReadFailureAndPreviewLogs(t *testing.T) {
	for _, text := range []string{"one two three four five six", "1\n2\n3\n4", "1\n2\n3\n4\n5", strings.Repeat("中", 201)} {
		preview := logText(text)
		if utf8.RuneCountInString(preview) > 200 || strings.Count(preview, "\n") > 3 {
			t.Errorf("invalid preview %q", preview)
		}
		if utf8.RuneCountInString(text) <= 200 && strings.Count(text, "\n") < 4 && preview != text {
			t.Errorf("unnecessarily truncated %q", preview)
		}
	}
	core, logs := observer.New(zapcore.InfoLevel)
	cfg, _ := config.Load("")
	engine := &Engine{Config: cfg, Log: zap.New(core)}
	session := newSession(engine)
	source, err := session.read(context.Background(), "https://github.com/loro-dev/loro?token=secret", httpBackend, func(_ context.Context, u string) (domain.Source, error) {
		return domain.Source{URL: u, Status: "restricted", Reason: "URL resolves to a non-public address"}, errors.New("unsafe target")
	})
	if err != nil || source.Status != "restricted" || len(session.readings) != 1 {
		t.Fatalf("failed read was not preserved as an observation: %+v %+v %v", source, session.readings, err)
	}
	failed := logs.FilterMessage("agent read_url failed").All()
	if len(failed) != 1 || failed[0].ContextMap()["source_reason"] != "URL resolves to a non-public address" || strings.Contains(failed[0].ContextMap()["url"].(string), "secret") {
		t.Fatalf("missing/unsafe read error log: %+v", failed)
	}
	base, _ := engine.Prepare(domain.Submission{Text: "question"})
	engine.Config.Agent.Streaming = true
	stream, _ := engine.Prepare(domain.Submission{Text: "question"})
	if base.CacheKey != stream.CacheKey {
		t.Error("stream setting changed analysis identity")
	}
}

func TestModelFailureDiagnosticsExcludeUpstreamBody(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if (request["stream"] == true) != streaming {
					t.Errorf("request stream=%v, want %t", request["stream"], streaming)
				}
				if streaming {
					writeSSEHeader(w)
					_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"fixture_secret_body\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = fmt.Fprint(w, `{"error":{"message":"fixture_secret_body","type":"insufficient_quota","code":"insufficient_quota"}}`)
			}))
			defer server.Close()
			cfg, _ := config.Load("")
			cfg.Agent.Streaming = streaming
			cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = server.URL+"/v1", "credential_fixture", "fixture"
			cfg.ProfilePath = t.TempDir() + "/missing"
			engine, err := New(context.Background(), cfg, func(_ context.Context, u string) (domain.Source, error) {
				return domain.Source{ID: "root", URL: u, Status: "ok", Content: "原文"}, nil
			}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zapcore.InfoLevel)
			engine.Log = zap.New(core)
			_, err = engine.Analyze(context.Background(), domain.Submission{URLs: []string{"https://example.org/root"}})
			if err == nil {
				t.Fatal("failed model response was accepted")
			}
			failures := logs.FilterMessage("agent analyze failed").All()
			if len(failures) != 1 || failures[0].ContextMap()["stage"] != "model" {
				t.Fatalf("missing model failure diagnostics: %+v", failures)
			}
			if !streaming && failures[0].ContextMap()["http_status"] != int64(429) {
				t.Errorf("quota status missing: %v", failures[0].ContextMap())
			}
			if streaming && failures[0].ContextMap()["reason"] != "model stream reported an error" {
				t.Errorf("SSE reason missing: %v", failures[0].ContextMap())
			}
			for _, entry := range logs.All() {
				if strings.Contains(fmt.Sprint(entry.ContextMap()), "fixture_secret_body") {
					t.Errorf("upstream error body logged: %v", entry.ContextMap())
				}
			}
			if logs.FilterMessage("agent analyze finished").Len() > 0 {
				t.Error("model failure logged success")
			}
		})
	}
}
