package modelprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"kagari/internal/config"
)

func TestChatToolCallAndToolResultRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("request = %s %s, want POST /v1/chat/completions", r.Method, r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		call := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			writeChatCompletion(t, w, map[string]any{
				"id": "chatcmpl_1", "object": "chat.completion", "created": 1, "model": "fixture",
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{
					"role": "assistant", "tool_calls": []any{map[string]any{
						"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"query":"news"}`},
					}},
				}, "finish_reason": "tool_calls"}},
			})
			return
		}
		writeChatCompletion(t, w, map[string]any{
			"id": "chatcmpl_2", "object": "chat.completion", "created": 2, "model": "fixture",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Found it."}, "finish_reason": "stop"}},
		})
	}))
	defer server.Close()

	m := newTestModel(t, server.URL+"/v1", config.ModelAPIChatCompletions)
	tool := &schema.ToolInfo{Name: "lookup", Desc: "Look up a query", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
		"query": {Type: schema.String, Required: true},
	})}
	user := schema.UserAgenticMessage("look up the news")
	call, err := m.Generate(context.Background(), []*schema.AgenticMessage{user}, model.WithTools([]*schema.ToolInfo{tool}))
	if err != nil {
		t.Fatal(err)
	}
	if len(call.ContentBlocks) != 1 || call.ContentBlocks[0].FunctionToolCall == nil {
		t.Fatalf("first response blocks = %#v; want one function call", call.ContentBlocks)
	}
	if got := call.ContentBlocks[0].FunctionToolCall.CallID; got != "call_1" {
		t.Fatalf("call ID = %q, want call_1", got)
	}

	toolResult := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.FunctionToolResult{
			CallID: "call_1", Name: "lookup", Content: []*schema.FunctionToolResultContentBlock{{
				Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "one matching article"},
			}},
		}),
	}}
	final, err := m.Generate(context.Background(), []*schema.AgenticMessage{user, call, toolResult}, model.WithTools([]*schema.ToolInfo{tool}), model.WithMaxTokens(222))
	if err != nil {
		t.Fatal(err)
	}
	if got := textAt(final); got != "Found it." {
		t.Fatalf("final text = %q, want Found it.", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	for i, request := range requests {
		wantBudget := float64(500)
		if i == 1 {
			wantBudget = 222
		}
		if request["max_completion_tokens"] != wantBudget {
			t.Errorf("request %d max_completion_tokens = %#v, want %v", i+1, request["max_completion_tokens"], wantBudget)
		}
		if _, ok := request["max_tokens"]; ok {
			t.Errorf("request %d unexpectedly sent max_tokens", i+1)
		}
	}
	messages, ok := requests[1]["messages"].([]any)
	if !ok {
		t.Fatalf("second request messages = %#v", requests[1]["messages"])
	}
	var sawAssistantCall, sawToolResult bool
	for _, item := range messages {
		message, _ := item.(map[string]any)
		switch message["role"] {
		case "assistant":
			calls, _ := message["tool_calls"].([]any)
			for _, raw := range calls {
				toolCall, _ := raw.(map[string]any)
				if toolCall["id"] == "call_1" {
					sawAssistantCall = true
				}
			}
		case "tool":
			if message["tool_call_id"] == "call_1" && message["content"] == "one matching article" {
				sawToolResult = true
			}
		}
	}
	if !sawAssistantCall || !sawToolResult {
		t.Fatalf("second request did not preserve tool round trip: assistant call=%t, tool result=%t; messages=%#v", sawAssistantCall, sawToolResult, messages)
	}
}

func TestChatStrictJSONSchemaAndCompletionBudget(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeChatCompletion(t, w, map[string]any{
			"id": "chatcmpl_schema", "object": "chat.completion", "created": 1, "model": "fixture",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": `{"ok":true}`}, "finish_reason": "stop"}},
		})
	}))
	defer server.Close()

	m := newTestModel(t, server.URL+"/v1", config.ModelAPIChatCompletions)
	outputSchema := map[string]any{
		"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}, "additionalProperties": false,
	}
	_, err := m.Generate(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("return JSON")}, WithJSONSchema("result", outputSchema), model.WithTemperature(0.25), model.WithMaxTokens(333))
	if err != nil {
		t.Fatal(err)
	}
	if request["temperature"] != 0.25 {
		t.Fatalf("temperature = %#v, want 0.25", request["temperature"])
	}
	if request["max_completion_tokens"] != float64(333) {
		t.Fatalf("max_completion_tokens = %#v, want 333", request["max_completion_tokens"])
	}
	if _, ok := request["max_tokens"]; ok {
		t.Fatalf("request unexpectedly sent max_tokens: %#v", request["max_tokens"])
	}
	responseFormat, ok := request["response_format"].(map[string]any)
	if !ok || responseFormat["type"] != "json_schema" {
		t.Fatalf("response_format = %#v", request["response_format"])
	}
	jsonSchema, ok := responseFormat["json_schema"].(map[string]any)
	if !ok || jsonSchema["name"] != "result" || jsonSchema["strict"] != true {
		t.Fatalf("json_schema = %#v", responseFormat["json_schema"])
	}
	if got := jsonSchema["schema"].(map[string]any)["additionalProperties"]; got != false {
		t.Fatalf("schema additionalProperties = %#v, want false", got)
	}
}

func TestChatStreamRejectsTruncatedToolCallAndKeepsText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		streamChunk := func(delta map[string]any, finishReason any) map[string]any {
			return map[string]any{
				"id": "chatcmpl_stream", "object": "chat.completion.chunk", "created": 1, "model": "fixture",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
			}
		}
		chunks := []map[string]any{
			streamChunk(map[string]any{"role": "assistant", "content": "partial stage text"}, nil),
			streamChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"query":`},
			}}}, nil),
			streamChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": `"news"}`},
			}}}, nil),
			streamChunk(map[string]any{}, "length"),
		}
		for _, chunk := range chunks {
			encoded, err := json.Marshal(chunk)
			if err != nil {
				t.Errorf("marshal stream chunk: %v", err)
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	m := newTestModel(t, server.URL+"/v1", config.ModelAPIChatCompletions)
	stream, err := m.Stream(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("lookup")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var chunks []*schema.AgenticMessage
	var streamErr error
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			streamErr = err
			break
		}
		chunks = append(chunks, chunk)
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), `finish_reason "length"`) {
		t.Fatalf("stream error = %v, want length finish reason error", streamErr)
	}
	if got := textAt(chunks...); got != "partial stage text" {
		t.Fatalf("stream text = %q, want partial stage text", got)
	}
	for _, chunk := range chunks {
		for _, block := range chunk.ContentBlocks {
			if block != nil && block.FunctionToolCall != nil {
				t.Fatalf("truncated tool call escaped stream: %#v", block.FunctionToolCall)
			}
		}
	}
}

func TestResponsesStrictJSONSchemaAndRequestInvariants(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("request path = %q, want /v1/responses", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"fixture","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"ok\":true}"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	}))
	defer server.Close()

	m := newTestModel(t, server.URL+"/v1", config.ModelAPIResponses)
	outputSchema := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}, "additionalProperties": false}
	if _, err := m.Generate(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("return JSON")}, WithJSONSchema("result", outputSchema)); err != nil {
		t.Fatal(err)
	}
	if request["store"] != false || request["parallel_tool_calls"] != false {
		t.Fatalf("Responses request invariants: store=%#v parallel_tool_calls=%#v", request["store"], request["parallel_tool_calls"])
	}
	if request["max_output_tokens"] != float64(500) {
		t.Fatalf("max_output_tokens = %#v, want 500", request["max_output_tokens"])
	}
	textConfig, ok := request["text"].(map[string]any)
	if !ok {
		t.Fatalf("text config = %#v", request["text"])
	}
	format, ok := textConfig["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" || format["name"] != "result" || format["strict"] != true {
		t.Fatalf("Responses text format = %#v", textConfig["format"])
	}
	if got := format["schema"].(map[string]any)["additionalProperties"]; got != false {
		t.Fatalf("schema additionalProperties = %#v, want false", got)
	}
}

func newTestModel(t *testing.T, baseURL string, apiType config.ModelAPIType) model.AgenticModel {
	t.Helper()
	m, err := New(context.Background(), config.Model{
		APIType: apiType, BaseURL: baseURL, APIKey: "test-key", Name: "fixture-model", Timeout: 3 * time.Second, MaxOutputTokens: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func writeChatCompletion(t *testing.T, w http.ResponseWriter, response map[string]any) {
	t.Helper()
	response["usage"] = map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func textAt(messages ...*schema.AgenticMessage) string {
	var text strings.Builder
	for _, message := range messages {
		if message == nil {
			continue
		}
		for _, block := range message.ContentBlocks {
			if block != nil && block.AssistantGenText != nil {
				text.WriteString(block.AssistantGenText.Text)
			}
		}
	}
	return text.String()
}
