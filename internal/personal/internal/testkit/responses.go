package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/components/model"

	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/inputbudget"
)

type Response struct {
	Status string
	Output []any
}

type ResponsesFixture struct {
	T         *testing.T
	Responses []Response
	Requests  []map[string]any
	index     int
}

func (f *ResponsesFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		f.T.Errorf("decode Responses request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.Requests = append(f.Requests, request)
	if f.index >= len(f.Responses) {
		f.T.Errorf("unexpected Responses request %d", f.index+1)
		http.Error(w, "no fixture response", http.StatusInternalServerError)
		return
	}
	response := f.Responses[f.index]
	f.index++
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": fmt.Sprintf("resp_%d", f.index), "object": "response", "status": response.Status,
		"model": "fixture", "output": response.Output,
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
	})
}

func (f *ResponsesFixture) Model(t *testing.T) model.AgenticModel {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	timeout := 4 * time.Second
	maxRetries, store, parallelCalls := 0, false, false
	m, err := agenticopenai.NewResponsesModel(context.Background(), &agenticopenai.ResponsesConfig{
		BaseURL: server.URL + "/v1", APIKey: "test-key", Model: "fixture-model",
		Timeout: &timeout, MaxRetries: &maxRetries, Store: &store, ParallelToolCalls: &parallelCalls,
	})
	if err != nil {
		t.Fatal(err)
	}
	limits, err := inputbudget.NewLimits(262144, 5000)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := inputbudget.NewAgenticModel(m, inputbudget.ModelConfig{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return bounded
}

func ResponseTool(name string, args any, id string) Response {
	encoded, _ := json.Marshal(args)
	return Response{Status: "completed", Output: []any{map[string]any{
		"type": "function_call", "id": "fc_" + id, "call_id": id, "name": name, "arguments": string(encoded),
	}}}
}

func ResponseText(text string) Response {
	return Response{Status: "completed", Output: []any{map[string]any{
		"type": "message", "id": "msg", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text}},
	}}}
}

func ResponseRefusal() Response {
	return Response{Status: "completed", Output: []any{map[string]any{
		"type": "message", "id": "msg", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "refusal", "refusal": "I cannot help with that request."}},
	}}}
}

func RequestInput(request map[string]any) string {
	encoded, _ := json.Marshal(request["input"])
	return string(encoded)
}

func ReadingEngine(fixture *ResponsesFixture, read func(context.Context, string) (domain.Source, error)) *agent.Engine {
	cfg := config.Config{}
	cfg.Agent.MaxSources = 8
	cfg.Agent.MaxIterations = 6
	cfg.Agent.Timeout = 4 * time.Second
	return &agent.Engine{Model: fixture.Model(fixture.T), Config: cfg, Profile: "PROFILE_READING", Read: read}
}
