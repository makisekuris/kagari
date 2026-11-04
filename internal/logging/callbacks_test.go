package logging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestCallbackObserverCorrelatesFullModelPayloadsAcrossContexts(t *testing.T) {
	coreA, logsA := observer.New(zapcore.DebugLevel)
	coreB, logsB := observer.New(zapcore.DebugLevel)
	handler := NewCallbackObserver("think")
	info := &callbacks.RunInfo{Name: "fixture", Component: components.ComponentOfAgenticModel}
	input := &model.AgenticCallbackInput{
		Messages: []*schema.AgenticMessage{schema.UserAgenticMessage("full logical input")},
		Tools:    []*schema.ToolInfo{{Name: "lookup"}},
		Config:   &model.AgenticConfig{Model: "fixture-model", MaxTokens: 19},
	}
	ctxA := handler.OnStart(WithLogger(context.Background(), zap.New(coreA).With(zap.Int64("event_id", 101))), info, input)
	ctxB := handler.OnStart(WithLogger(context.Background(), zap.New(coreB).With(zap.Int64("event_id", 202))), info, input)
	message := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "full model output"})}}
	output := &model.AgenticCallbackOutput{Message: message, TokenUsage: &model.TokenUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}
	handler.OnEnd(ctxB, info, output)
	handler.OnEnd(ctxA, info, output)

	startedA, finishedA := callbackEvent(t, logsA.All(), "model.started"), callbackEvent(t, logsA.All(), "model.finished")
	startedB, finishedB := callbackEvent(t, logsB.All(), "model.started"), callbackEvent(t, logsB.All(), "model.finished")
	fieldsA, fieldsB := startedA.ContextMap(), startedB.ContextMap()
	if fieldsA["event_id"] != int64(101) || fieldsB["event_id"] != int64(202) {
		t.Fatalf("start contexts = %v and %v", fieldsA["event_id"], fieldsB["event_id"])
	}
	if fieldsA["model_call_id"] == fieldsB["model_call_id"] {
		t.Fatalf("model call IDs are not unique: %v", fieldsA["model_call_id"])
	}
	if finishedA.ContextMap()["model_call_id"] != fieldsA["model_call_id"] || finishedB.ContextMap()["model_call_id"] != fieldsB["model_call_id"] {
		t.Fatal("finished model calls lost their start correlation")
	}
	if fieldsA["phase"] != "think" || finishedA.ContextMap()["phase"] != "think" {
		t.Fatalf("phase missing from model lifecycle: start=%v finish=%v", fieldsA["phase"], finishedA.ContextMap()["phase"])
	}
	startPayload, _ := json.Marshal(fieldsA["payload"])
	finishPayload, _ := json.Marshal(finishedA.ContextMap()["payload"])
	if !strings.Contains(string(startPayload), "full logical input") || !strings.Contains(string(startPayload), "fixture-model") || !strings.Contains(string(startPayload), "lookup") {
		t.Fatalf("start payload missing messages/tools/config: %s", startPayload)
	}
	if !strings.Contains(string(finishPayload), "full model output") || finishedA.ContextMap()["usage_reported"] != true || finishedA.ContextMap()["input_tokens"] != int64(3) {
		t.Fatalf("finish payload/usage incomplete: fields=%v payload=%s", finishedA.ContextMap(), finishPayload)
	}
}

func TestCallbackObserverDoesNotReportTypedNilUsage(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	handler := NewCallbackObserver("format")
	info := &callbacks.RunInfo{Name: "fixture", Component: components.ComponentOfAgenticModel}
	ctx := handler.OnStart(WithLogger(context.Background(), zap.New(core)), info, &model.AgenticCallbackInput{})
	handler.OnEnd(ctx, info, &model.AgenticCallbackOutput{Message: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ResponseMeta: &schema.AgenticResponseMeta{}}})
	finished := callbackEvent(t, logs.All(), "model.finished")
	if finished.ContextMap()["usage_reported"] != false {
		t.Fatalf("typed nil usage reported: %v", finished.ContextMap())
	}
}

func TestToolBusinessFailureRecognizesReadErrors(t *testing.T) {
	if !toolBusinessFailure("read_url", &tool.CallbackOutput{Response: `{"error":"HTTP request failed"}`}) {
		t.Fatal("non-empty read_url Error field must be a business failure")
	}
}

func TestToolBusinessFailureUsesOnlyKnownReadReplySources(t *testing.T) {
	tests := []struct {
		name, tool, response string
		want                 bool
	}{
		{name: "restricted read", tool: "read_url", response: `{"source":{"status":"restricted","reason":"HTTP 403"}}`, want: true},
		{name: "empty read", tool: "browse_url", response: `{"source":{"status":"ok","content":"  "}}`, want: true},
		{name: "truncated citation", tool: "read_url", response: `{"source":{"status":"incomplete","truncated":true,"content":"usable excerpt"}}`},
		{name: "untruncated incomplete read", tool: "read_url", response: `{"source":{"status":"incomplete","content":"not complete enough to cite"}}`, want: true},
		{name: "other tool source field", tool: "lookup", response: `{"source":{"status":"restricted"}}`},
		{name: "top level ok false", tool: "lookup", response: `{"ok":false}`, want: true},
		{name: "top level error", tool: "lookup", response: `{"error":"lookup failed"}`, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolBusinessFailure(tt.tool, &tool.CallbackOutput{Response: tt.response}); got != tt.want {
				t.Fatalf("toolBusinessFailure(%q, %s) = %t, want %t", tt.tool, tt.response, got, tt.want)
			}
		})
	}
}

func TestCallbackObserverStreamingPreservesCompleteAndPartialResponses(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		handler := NewCallbackObserver("analyze")
		info := &callbacks.RunInfo{Name: "fixture", Component: components.ComponentOfAgenticModel}
		ctx := handler.OnStart(WithLogger(context.Background(), zap.New(core)), info, &model.AgenticCallbackInput{})
		first := assistantChunk("streamed ")
		last := assistantChunk("response")
		last.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: &schema.TokenUsage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}}
		handler.OnEndWithStreamOutput(ctx, info, callbackOutputStream(nil, &model.AgenticCallbackOutput{Message: first}, &model.AgenticCallbackOutput{Message: last}))
		handler.Wait()
		finished := callbackEvent(t, logs.All(), "model.finished")
		payload, _ := json.Marshal(finished.ContextMap()["payload"])
		if !strings.Contains(string(payload), "streamed response") || finished.ContextMap()["total_tokens"] != int64(7) {
			t.Fatalf("stream output/usage missing: fields=%v payload=%s", finished.ContextMap(), payload)
		}
	})

	t.Run("partial stream error", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		handler := NewCallbackObserver("analyze")
		info := &callbacks.RunInfo{Name: "fixture", Component: components.ComponentOfAgenticModel}
		ctx := handler.OnStart(WithLogger(context.Background(), zap.New(core)), info, &model.AgenticCallbackInput{})
		partial := assistantChunk("partial output")
		handler.OnEndWithStreamOutput(ctx, info, callbackOutputStream(errors.New("wire body secret"), &model.AgenticCallbackOutput{Message: partial}))
		handler.Wait()
		failed := callbackEvent(t, logs.All(), "model.failed")
		payload, _ := json.Marshal(failed.ContextMap()["payload"])
		if !strings.Contains(string(payload), "partial output") || strings.Contains(string(payload), "wire body secret") {
			t.Fatalf("partial output or sanitized failure lost: fields=%v payload=%s", failed.ContextMap(), payload)
		}
		if failed.ContextMap()["usage_reported"] != false {
			t.Fatalf("missing usage must be explicit: %v", failed.ContextMap())
		}
	})
}

func assistantChunk(text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlockChunk(&schema.AssistantGenText{Text: text}, &schema.StreamingMeta{Index: 0})}}
}

func callbackOutputStream(streamErr error, values ...callbacks.CallbackOutput) *schema.StreamReader[callbacks.CallbackOutput] {
	reader, writer := schema.Pipe[callbacks.CallbackOutput](len(values) + 1)
	for _, value := range values {
		writer.Send(value, nil)
	}
	if streamErr != nil {
		writer.Send(nil, streamErr)
	}
	writer.Close()
	return reader
}

func callbackEvent(t *testing.T, entries []observer.LoggedEntry, message string) observer.LoggedEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.Message == message {
			return entry
		}
	}
	t.Fatalf("event %q not found in %v", message, entries)
	return observer.LoggedEntry{}
}
