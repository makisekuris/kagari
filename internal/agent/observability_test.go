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
	"time"
	"unicode/utf8"

	"kagari/internal/config"
	"kagari/internal/domain"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestAgentMessagesAndLiveSSE(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			firstLogged := make(chan struct{}, 1)
			logger := zap.New(zapcore.RegisterHooks(core, func(entry zapcore.Entry) error {
				if entry.Message == "agent sse output" {
					select {
					case firstLogged <- struct{}{}:
					default:
					}
				}
				return nil
			}))
			var calls atomic.Int32
			args := `{"url":"https://example.org/article","parent_source_id":"root","question":"读取原文","role":"primary"}`
			analysis := domain.Analysis{Title: "标题", Overview: "事实", Summary: []domain.Claim{{Text: "事实", SourceIDs: []string{"article"}}}, Category: "工程"}
			analysis.Headings = &domain.AnalysisHeadings{Summary: "事实", Discussion: "讨论", Evaluation: "评价", Uncertainties: "限制", Sources: "来源"}
			raw, _ := json.Marshal(analysis)
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
				var item map[string]any
				if call == 1 {
					item = map[string]any{"type": "function_call", "id": "fc_1", "call_id": "read_1", "name": "read_source", "arguments": args, "status": "completed"}
				} else {
					input, _ := json.Marshal(request["input"])
					if !strings.Contains(string(input), `"call_id":"read_1"`) || !strings.Contains(string(input), "function_call_output") {
						t.Error("tool result lost call_id")
					}
					item = map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": string(raw), "annotations": []any{}}}}
				}
				response := map[string]any{"id": fmt.Sprintf("resp_%d", call), "object": "response", "status": "completed", "model": "fixture", "output": []any{item}, "usage": map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18, "input_tokens_details": map[string]any{"cached_tokens": 3}, "output_tokens_details": map[string]any{"reasoning_tokens": 2}}}
				if !streaming {
					if call == 1 {
						response["output"] = []any{map[string]any{"id": "reason_1", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "需要阅读原文"}}}, item}
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(response)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(kind string, fields map[string]any) {
					fields["type"] = kind
					data, _ := json.Marshal(fields)
					_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
					w.(http.Flusher).Flush()
				}
				if call == 1 {
					send("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "reason_1", "type": "reasoning", "summary": []any{}}})
					send("response.reasoning_summary_text.delta", map[string]any{"output_index": 0, "summary_index": 0, "item_id": "reason_1", "delta": "需要阅读原文"})
					added := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "read_1", "name": "read_source", "arguments": "", "status": "in_progress"}
					send("response.output_item.added", map[string]any{"output_index": 1, "item": added})
					send("response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_1", "delta": args[:35]})
					// 服务端等待第一段日志，再发送剩余内容；缓冲到结束才打日志会失败。
					select {
					case <-firstLogged:
					case <-time.After(3 * time.Second):
						t.Error("SSE was not logged before response completion")
					}
					send("response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_1", "delta": args[35:]})
					send("response.output_item.done", map[string]any{"output_index": 1, "item": item})
				} else {
					for _, delta := range []string{string(raw[:20]), string(raw[20:])} {
						send("response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_1", "delta": delta})
					}
				}
				send("response.completed", map[string]any{"response": response})
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
				if u == "https://example.org/root" {
					return domain.Source{ID: "root", URL: u, Status: "ok", Content: "入口", Links: []domain.Link{{URL: "https://example.org/article"}}}, nil
				}
				return domain.Source{ID: "article", URL: u, Status: "ok", Content: strings.Repeat("完整原文 ", 100)}, nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			engine.Log = logger
			result, err := engine.Analyze(context.Background(), domain.Submission{URLs: []string{"https://example.org/root"}})
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || len(result.Readings) != 2 || result.Usage.TotalTokens != 36 || result.Analysis.Title != "标题" {
				t.Fatalf("wrong tool round trip: %+v calls=%d", result, calls.Load())
			}
			for _, name := range []string{"agent reason", "agent toolcall", "agent toolcall result", "agent token usage", "agent analyze finished"} {
				if logs.FilterMessage(name).Len() == 0 {
					t.Errorf("missing info log %s", name)
				}
			}
			toolCall := logs.FilterMessage("agent toolcall").All()[0].ContextMap()
			if toolCall["call_id"] != "read_1" || toolCall["name"] != "read_source" || toolCall["arguments"] != args {
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
			if streaming {
				var text strings.Builder
				for _, entry := range logs.FilterMessage("agent sse output").All() {
					if delta, ok := entry.ContextMap()["delta"].(string); ok {
						text.WriteString(delta)
					}
				}
				if text.String() != string(raw) {
					t.Errorf("stream was not logged in chunks: %q", text.String())
				}
				if logs.FilterMessage("agent message").Len() != 0 {
					t.Error("stream also logged buffered message")
				}
			} else if logs.FilterMessage("agent sse output").Len() != 0 {
				t.Error("non-stream response labeled SSE")
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
	engine := &Engine{Config: cfg, Log: zap.New(core), Read: func(_ context.Context, u string) (domain.Source, error) {
		return domain.Source{URL: u, Status: "restricted", Reason: "URL resolves to a non-public address"}, errors.New("unsafe target")
	}}
	result, err := engine.Analyze(context.Background(), domain.Submission{URLs: []string{"https://github.com/loro-dev/loro?token=secret"}})
	if err == nil || result.Usage.TotalTokens != 0 {
		t.Fatalf("unreadable source was accepted: %+v %v", result, err)
	}
	failed := logs.FilterMessage("agent read_source failed").All()
	if len(failed) != 1 || failed[0].ContextMap()["source_reason"] != "URL resolves to a non-public address" || strings.Contains(failed[0].ContextMap()["url"].(string), "secret") {
		t.Fatalf("missing/unsafe read error log: %+v", failed)
	}
	if logs.FilterMessage("agent analyze finished").Len() != 0 || logs.FilterMessage("agent analyze failed").All()[0].ContextMap()["stage"] != "read_sources" {
		t.Fatal("failed run logged success or wrong stage")
	}
	base, _ := engine.Prepare(domain.Submission{URLs: []string{"https://example.org/root"}})
	engine.Config.Agent.Streaming = true
	stream, _ := engine.Prepare(domain.Submission{URLs: []string{"https://example.org/root"}})
	if base.CacheKey != stream.CacheKey {
		t.Error("stream setting changed analysis identity")
	}
}

func TestModelFailureDiagnosticsExcludeUpstreamBody(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"fixture_secret_body\"}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = fmt.Fprint(w, `{"error":{"message":"fixture_secret_body","type":"insufficient_quota","code":"insufficient_quota"}}`)
				}
			}))
			defer server.Close()
			cfg, _ := config.Load("")
			cfg.Agent.Streaming = streaming
			cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = server.URL+"/v1", "credential_fixture", "fixture"
			cfg.ProfilePath = t.TempDir() + "/missing"
			engine, err := New(context.Background(), cfg, func(_ context.Context, u string) (domain.Source, error) {
				return domain.Source{ID: "root", URL: u, Status: "ok", Content: "原文"}, nil
			}, nil)
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
