package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"kagari/internal/config"
	"kagari/internal/domain"
)

func TestGenerateDigestMergesAndSendsFrozenSnapshotWithoutSourceBodies(t *testing.T) {
	merged := domain.DigestReview{
		Opening: "这周有个值得追的共同线索喵。",
		Sections: []domain.DigestSection{{Name: "工程", Items: []domain.DigestItem{{
			EntryIDs: []int64{11, 12}, Title: "同一项目的重复进展", Review: "两篇材料都谈到同一项进展，证据分别来自两条原文。",
			Refs: []domain.DigestRef{{JobID: 11, SourceID: "s_11"}, {JobID: 12, SourceID: "s_12"}},
		}}}},
		Closing: "后续再看原文有没有补充喵。",
	}
	encoded, _ := json.Marshal(merged)
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		data, _ := json.Marshal(body)
		requestBody = string(data)
		format := body["text"].(map[string]any)["format"].(map[string]any)
		if format["name"] != "weekly_digest" || format["strict"] != true {
			t.Errorf("digest format=%v", format)
		}
		properties := format["schema"].(map[string]any)["properties"].(map[string]any)
		if _, ok := properties["sections"]; !ok {
			t.Error("digest schema is missing sections")
		}
		if tools, ok := body["tools"].([]any); ok && len(tools) != 0 {
			t.Error("digest request must not expose tools")
		}
		snapshot := findDigestSnapshot(body["input"])
		if snapshot == nil {
			t.Error("serialized user input did not contain the digest snapshot")
		} else {
			if _, ok := snapshot["instruction"]; ok {
				t.Error("system instruction was duplicated in the user input")
			}
			entries := snapshot["entries"].([]any)
			for _, rawEntry := range entries {
				sources := rawEntry.(map[string]any)["sources"].([]any)
				for _, rawSource := range sources {
					if _, ok := rawSource.(map[string]any)["content"]; ok {
						t.Error("digest snapshot included a source body")
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_digest", "object": "response", "created_at": 1, "status": "completed", "model": "test",
			"output": []any{map[string]any{"type": "message", "id": "msg_digest", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": string(encoded), "annotations": []any{}}}}},
			"usage":  map[string]any{"input_tokens": 13, "output_tokens": 8, "total_tokens": 21},
		})
	}))
	defer server.Close()
	e := newDigestTestEngine(t, server.URL)
	input := domain.DigestInput{
		UserID: 7, Start: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		Cutoff: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), AsOf: time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC),
		Timezone: "Asia/Shanghai", Profile: "profile-frozen-once", Instruction: "digest-system-instruction-once",
		Entries: []domain.DigestEntry{
			{JobID: 11, ReceivedAt: time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC), Analysis: domain.Analysis{Title: "进展一", Category: "工程", Summary: []domain.Claim{{Text: "事实一", SourceIDs: []string{"s_11"}}}}, Sources: []domain.DigestSource{{ID: "s_11", Title: "材料一", URL: "https://example.org/1", Status: "ok", Usable: true}}},
			{JobID: 12, ReceivedAt: time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC), Analysis: domain.Analysis{Title: "进展二", Category: "工程", Summary: []domain.Claim{{Text: "事实二", SourceIDs: []string{"s_12"}}}}, Sources: []domain.DigestSource{{ID: "s_12", Title: "材料二", URL: "https://example.org/2", Status: "ok", Usable: true}}},
		},
	}
	review, usage, err := e.GenerateDigest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if review.Sections[0].Items[0].EntryIDs[1] != 12 || len(review.Sections[0].Items[0].Refs) != 2 {
		t.Fatalf("merged output=%+v", review)
	}
	if usage != (domain.Usage{InputTokens: 13, OutputTokens: 8, TotalTokens: 21}) {
		t.Fatalf("usage=%+v", usage)
	}
	if strings.Count(requestBody, "profile-frozen-once") != 1 || strings.Count(requestBody, "digest-system-instruction-once") != 1 {
		t.Fatalf("frozen profile or system instruction was duplicated: %s", requestBody)
	}
}

func TestGenerateDigestReturnsUsageWhenResponseIsIncomplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_incomplete", "object": "response", "created_at": 1, "status": "incomplete", "model": "test",
			"output": []any{map[string]any{"type": "message", "id": "msg_incomplete", "role": "assistant", "status": "incomplete", "content": []any{map[string]any{"type": "output_text", "text": `{"opening":"partial","sections":[],"closing":""}`, "annotations": []any{}}}}},
			"usage":  map[string]any{"input_tokens": 5, "output_tokens": 2, "total_tokens": 7},
		})
	}))
	defer server.Close()
	e := newDigestTestEngine(t, server.URL)
	_, usage, err := e.GenerateDigest(context.Background(), domain.DigestInput{})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err=%v", err)
	}
	if usage.TotalTokens != 7 {
		t.Fatalf("partial usage=%+v", usage)
	}
}

func TestGenerateDigestConsumesStreamingResponse(t *testing.T) {
	raw := `{"opening":"","sections":[],"closing":""}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request["stream"] != true {
			t.Errorf("stream=%v, want true", request["stream"])
		}
		requestJSON, _ := json.Marshal(request)
		if strings.Count(string(requestJSON), "injected-digest-persona") != 1 || strings.Contains(string(requestJSON), "永雏塔菲") {
			t.Error("digest request did not use the injected persona")
		}
		message := map[string]any{"type": "message", "id": "msg_stream", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": raw, "annotations": []any{}}}}
		response := map[string]any{"id": "resp_stream", "object": "response", "status": "completed", "model": "test", "output": []any{message}, "usage": map[string]any{"input_tokens": 4, "output_tokens": 3, "total_tokens": 7}}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(kind string, fields map[string]any) {
			fields["type"] = kind
			data, _ := json.Marshal(fields)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
			w.(http.Flusher).Flush()
		}
		for _, delta := range []string{raw[:18], raw[18:]} {
			send("response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": "msg_stream", "delta": delta})
		}
		send("response.completed", map[string]any{"response": response})
	}))
	defer server.Close()
	e := newDigestTestEngineWithStreaming(t, server.URL, true)
	_, usage, err := e.GenerateDigest(context.Background(), domain.DigestInput{})
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalTokens != 7 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestConsumeEventsHandlesStreamingMessages(t *testing.T) {
	e := &Engine{}
	msg := &schema.AgenticMessage{
		Role:          schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeAssistantGenText, AssistantGenText: &schema.AssistantGenText{Text: `{"opening":"streamed"}`}}},
		ResponseMeta:  &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}},
	}
	it, gen := adk.NewAsyncIteratorPair[*adk.TypedAgentEvent[*schema.AgenticMessage]]()
	gen.Send(adk.EventFromAgenticMessage(nil, schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), schema.AgenticRoleTypeAssistant))
	gen.Close()
	output, usage, reported, err := e.consumeEvents(it, 1)
	if err != nil || output != `{"opening":"streamed"}` || !reported || usage.TotalTokens != 5 {
		t.Fatalf("consume=(%q, %+v, %v, %v)", output, usage, reported, err)
	}
}

func newDigestTestEngine(t *testing.T, baseURL string) *Engine {
	return newDigestTestEngineWithStreaming(t, baseURL, false)
}

func newDigestTestEngineWithStreaming(t *testing.T, baseURL string, streaming bool) *Engine {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agent.Streaming = streaming
	cfg.Model.BaseURL, cfg.Model.APIKey, cfg.Model.Name = baseURL+"/v1", "test", "test"
	e, err := New(context.Background(), cfg, nil, nil, testPersona("injected-digest-persona"))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func findDigestSnapshot(value any) map[string]any {
	switch value := value.(type) {
	case string:
		var decoded map[string]any
		if json.Unmarshal([]byte(value), &decoded) == nil {
			if _, ok := decoded["entries"]; ok {
				return decoded
			}
		}
	case []any:
		for _, item := range value {
			if snapshot := findDigestSnapshot(item); snapshot != nil {
				return snapshot
			}
		}
	case map[string]any:
		for _, item := range value {
			if snapshot := findDigestSnapshot(item); snapshot != nil {
				return snapshot
			}
		}
	}
	return nil
}
