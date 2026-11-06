package inputbudget

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestNewLimitsUsesDocumentedIntegerFormula(t *testing.T) {
	limits, err := NewLimits(32768, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if limits.Reserve != 1639 || limits.Hard != 26129 || limits.SummarizeAt != 20903 || limits.Target != 15677 {
		t.Fatalf("limits = %+v", limits)
	}
	max, err := NewLimits(math.MaxInt64, 1)
	if err != nil || max.Hard <= 0 || max.Hard >= math.MaxInt64 || max.SummarizeAt > max.Hard || max.Target > max.SummarizeAt {
		t.Fatalf("maximum window overflowed: %+v, %v", max, err)
	}
	for _, pair := range [][2]int64{{0, 1}, {10000, 0}, {10000, 10000}, {7000, 2000}} {
		if _, err := NewLimits(pair[0], pair[1]); !errors.Is(err, ErrInvalidLimits) {
			t.Errorf("NewLimits(%d,%d) error = %v", pair[0], pair[1], err)
		}
	}
}

func TestEstimateAgenticInputCountsMessagesToolsAndSchema(t *testing.T) {
	messages := []*schema.AgenticMessage{schema.UserAgenticMessage("你好🙂")}
	tools := []*schema.ToolInfo{{Name: "lookup", Desc: "find a value"}}
	schemaJSON := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	toolJSON, err := json.Marshal(struct {
		Tools []*schema.ToolInfo `json:"tools,omitempty"`
	}{tools})
	if err != nil {
		t.Fatal(err)
	}
	messageJSON, _ := json.Marshal(messages)
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(schemaJSON)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	canonicalSchema, _ := json.Marshal(decoded)
	estimate, err := EstimateAgenticInput(messages, tools, nil, nil, schemaJSON, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(len(messageJSON) + len(toolJSON) + len(canonicalSchema))
	if estimate.TotalBytes != want || estimate.CountBasis != "utf8_bytes" || estimate.ToolsBytes != int64(len(toolJSON)) || estimate.OutputSchemaBytes != int64(len(canonicalSchema)) {
		t.Fatalf("estimate = %+v, want total %d", estimate, want)
	}
	if estimate.RequiredBytes != int64(len(messageJSON)) || estimate.HistoryBytes != 0 {
		t.Fatalf("required/history bytes = %d/%d", estimate.RequiredBytes, estimate.HistoryBytes)
	}
}

func TestEstimateRejectsUnsupportedMultimodalContent(t *testing.T) {
	msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.UserInputImage{URL: "https://example.com/image.png"}),
	}}
	_, err := EstimateAgenticInput([]*schema.AgenticMessage{msg}, nil, nil, nil, nil, 0)
	var unsupported *UnsupportedContentError
	if !errors.As(err, &unsupported) || unsupported.ContentType != schema.ContentBlockTypeUserInputImage {
		t.Fatalf("unsupported content error = %v", err)
	}
}

func TestAgenticModelChecksGenerateAndStreamWithCallToolsAndReportsUsage(t *testing.T) {
	limits, _ := NewLimits(10000, 2000)
	base := &fakeAgenticModel{response: assistantMessage("ok", &schema.TokenUsage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11})}
	var records []ModelRecord
	wrapper, err := NewAgenticModel(base, ModelConfig{Limits: limits, OutputSchema: json.RawMessage(`{"type":"object"}`), Observe: func(r ModelRecord) { records = append(records, r) }})
	if err != nil {
		t.Fatal(err)
	}
	tool := &schema.ToolInfo{Name: "lookup", Desc: strings.Repeat("x", 40)}
	if _, err := wrapper.Generate(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("hello")}, model.WithTools([]*schema.ToolInfo{tool})); err != nil {
		t.Fatal(err)
	}
	if base.generateCalls != 1 || base.lastMaxTokens != 2000 || len(records) != 1 || records[0].Estimate.ToolsBytes == 0 || records[0].Estimate.OutputSchemaBytes == 0 || records[0].InputTokens != 9 {
		t.Fatalf("generate calls=%d records=%+v", base.generateCalls, records)
	}
	stream, err := wrapper.Stream(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("hello")}, model.WithTools([]*schema.ToolInfo{tool}))
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = stream.Recv()
		if errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if err != nil {
			break
		}
	}
	if base.streamCalls != 1 || base.lastMaxTokens != 2000 || len(records) != 2 || records[1].InputTokens != 9 {
		t.Fatalf("stream calls=%d records=%+v", base.streamCalls, records)
	}
}

func TestAgenticModelRejectsGenerateAndStreamBeforeBaseCall(t *testing.T) {
	limits, _ := NewLimits(10000, 2000)
	base := &fakeAgenticModel{response: assistantMessage("unused", nil)}
	wrapper, err := NewAgenticModel(base, ModelConfig{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	large := []*schema.AgenticMessage{schema.UserAgenticMessage(strings.Repeat("x", int(limits.Hard)))}
	_, err = wrapper.Generate(context.Background(), large)
	var limitErr *ContextLimitError
	if !errors.As(err, &limitErr) || limitErr.Kind != ErrorHistoryExceedsLimit || base.generateCalls != 0 {
		t.Fatalf("Generate err=%v, calls=%d", err, base.generateCalls)
	}
	tool := &schema.ToolInfo{Name: "large", Desc: strings.Repeat("y", int(limits.Hard))}
	_, err = wrapper.Stream(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("small")}, model.WithTools([]*schema.ToolInfo{tool}))
	if !errors.As(err, &limitErr) || limitErr.Kind != ErrorToolsSchemaExceedsLimit || base.streamCalls != 0 {
		t.Fatalf("Stream err=%v, calls=%d", err, base.streamCalls)
	}
}

func TestAgenticModelRecomputesLimitForOutputOverride(t *testing.T) {
	limits, _ := NewLimits(10000, 2000)
	base := &fakeAgenticModel{response: assistantMessage("unused", nil)}
	wrapper, err := NewAgenticModel(base, ModelConfig{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrapper.Generate(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage(strings.Repeat("x", 5000))}, model.WithMaxTokens(4000))
	var limitErr *ContextLimitError
	if !errors.As(err, &limitErr) || limitErr.Limit != 4976 || base.generateCalls != 0 {
		t.Fatalf("output override was not reflected in limit: err=%v calls=%d", err, base.generateCalls)
	}
}

func TestAgenticHandlerReducesOnlyOlderCompleteToolRounds(t *testing.T) {
	limits, _ := NewLimits(10000, 2000)
	var persisted int
	handler, err := NewAgenticHandler(context.Background(), HandlerConfig{
		Limits: limits,
		PersistAndReplace: func(_ context.Context, round ToolRound) ([]*schema.AgenticMessage, error) {
			persisted++
			if !round.Complete || len(round.Messages) != 2 {
				t.Fatalf("incomplete round passed to reducer: %+v", round)
			}
			return []*schema.AgenticMessage{schema.UserAgenticMessage("read result saved as material m_1")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, _ := handler.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		schema.SystemAgenticMessage("system"), schema.UserAgenticMessage("initial request"),
		toolCall("old", "read_url"), toolResult("old", "read_url", strings.Repeat("old evidence ", 520)),
		schema.UserAgenticMessage("intermediate"),
		toolCall("latest", "read_url"), toolResult("latest", "read_url", "latest result"),
	}}
	_, got, err := handler.BeforeModelRewriteState(ctx, state, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil {
		t.Fatal(err)
	}
	if persisted != 1 || len(got.Messages) != 6 || got.Messages[0] != state.Messages[0] || got.Messages[1] != state.Messages[1] {
		t.Fatalf("messages=%d persisted=%d", len(got.Messages), persisted)
	}
	if got.Messages[len(got.Messages)-2].ContentBlocks[0].FunctionToolCall == nil || got.Messages[len(got.Messages)-1].ContentBlocks[0].FunctionToolResult == nil {
		t.Fatal("latest tool call/result group was not preserved")
	}
	_, repeated, err := handler.BeforeModelRewriteState(ctx, got, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil || persisted != 1 || len(repeated.Messages) != len(got.Messages) {
		t.Fatalf("repeat: persisted=%d messages=%d err=%v", persisted, len(repeated.Messages), err)
	}
}

func TestAgenticHandlerDoesNotArchiveItsReplacementAgain(t *testing.T) {
	limits, err := NewLimits(12000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	var persisted int
	handler, err := NewAgenticHandler(context.Background(), HandlerConfig{
		Limits: limits,
		PersistAndReplace: func(_ context.Context, round ToolRound) ([]*schema.AgenticMessage, error) {
			persisted++
			return []*schema.AgenticMessage{round.Messages[0], toolResult("old", "lookup", "saved evidence reference")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		schema.SystemAgenticMessage("system"), schema.UserAgenticMessage("initial request"),
		toolCall("old", "lookup"), toolResult("old", "lookup", strings.Repeat("result ", 400)),
		schema.UserAgenticMessage(strings.Repeat("history ", 650)),
		toolCall("latest", "lookup"), toolResult("latest", "lookup", "latest result"),
		schema.UserAgenticMessage("latest request"),
	}}
	ctx, _, err := handler.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	estimate, err := handler.estimate(state.Messages, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reduced, after, err := handler.reduceToolRounds(ctx, state, estimate)
	if err != nil {
		t.Fatal(err)
	}
	if after.TotalBytes <= limits.Target {
		t.Fatal("fixture must still exceed the reduction target after saving the first result")
	}
	if persisted != 1 {
		t.Fatalf("archived a reference again: persistence calls=%d", persisted)
	}
	if _, _, err := handler.reduceToolRounds(ctx, &reduced, after); err != nil || persisted != 1 {
		t.Fatalf("later pass re-archived the same reference: calls=%d err=%v", persisted, err)
	}
}

func TestAgenticHandlerDoesNotReduceDuplicateToolCallIDs(t *testing.T) {
	limits, err := NewLimits(10000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	var persisted int
	handler, err := NewAgenticHandler(context.Background(), HandlerConfig{
		Limits: limits,
		PersistAndReplace: func(context.Context, ToolRound) ([]*schema.AgenticMessage, error) {
			persisted++
			return []*schema.AgenticMessage{schema.UserAgenticMessage("saved")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicateCalls := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.FunctionToolCall{CallID: "duplicate", Name: "first", Arguments: `{}`}),
		schema.NewContentBlock(&schema.FunctionToolCall{CallID: "duplicate", Name: "second", Arguments: `{}`}),
	}}
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		schema.SystemAgenticMessage("system"), schema.UserAgenticMessage("initial request"), duplicateCalls,
		toolResult("duplicate", "first", strings.Repeat("large result ", 500)),
		toolCall("latest", "lookup"), toolResult("latest", "lookup", "latest result"), schema.UserAgenticMessage("latest request"),
	}}
	ctx, _, err := handler.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	reduced, _, err := handler.reduceToolRounds(ctx, state, Estimate{TotalBytes: limits.Target + 1})
	if err != nil {
		t.Fatal(err)
	}
	if persisted != 0 || !sameMessages(reduced.Messages, state.Messages) {
		t.Fatalf("duplicate call IDs were reduced: persisted=%d", persisted)
	}
}

func TestAgenticHandlerBoundsEinoSummaryAndKeepsMemoryAndInitialInput(t *testing.T) {
	limits, _ := NewLimits(10000, 2000)
	summaryModel := &fakeAgenticModel{response: assistantMessage("short, supported summary", nil)}
	handler, err := NewAgenticHandler(context.Background(), HandlerConfig{Limits: limits, SummaryModel: summaryModel, ValidateSummary: noOpSummaryValidator})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, _ := handler.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
	memory := schema.UserAgenticMessage("private preference value")
	memory.Extra = map[string]any{"context_memory": true}
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		schema.SystemAgenticMessage("system"), schema.UserAgenticMessage("initial request"),
		schema.UserAgenticMessage(strings.Repeat("old history ", 220)), memory,
		assistantMessage(strings.Repeat("recent answer ", 3), nil), schema.UserAgenticMessage("latest request"),
	}}
	// Keep the estimate above S while leaving the required initial request and recent suffix verbatim.
	state.Messages[2] = schema.UserAgenticMessage(strings.Repeat("old history ", 450))
	_, got, err := handler.BeforeModelRewriteState(ctx, state, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil {
		t.Fatal(err)
	}
	if summaryModel.generateCalls != 1 {
		t.Fatalf("summary calls = %d", summaryModel.generateCalls)
	}
	if summaryEstimate, err := EstimateAgenticInput(summaryModel.lastInput, nil, nil, nil, nil, 0); err != nil || summaryEstimate.TotalBytes > handler.summaryLimits.Hard {
		t.Fatalf("summary input exceeded its bound: estimate=%+v err=%v", summaryEstimate, err)
	}
	for _, msg := range summaryModel.lastInput {
		if strings.Contains(textContent(msg), "private preference value") {
			t.Fatal("tagged memory was copied into the summary input")
		}
	}
	if got.Messages[0] != state.Messages[0] || got.Messages[1] != state.Messages[1] || got.Messages[len(got.Messages)-3] != memory {
		t.Fatal("summary did not preserve system, initial input, or tagged memory")
	}
	if estimate, err := handler.estimate(got.Messages, nil, nil); err != nil || estimate.TotalBytes >= limits.SummarizeAt {
		t.Fatalf("summary did not reduce under trigger: estimate=%+v err=%v", estimate, err)
	}
}

func TestAgenticHandlerSummaryFailureFallsBackOnceWhenUnderHardLimit(t *testing.T) {
	limits, _ := NewLimits(10000, 2000)
	base := &fakeAgenticModel{generateErr: errors.New("summary unavailable")}
	handler, err := NewAgenticHandler(context.Background(), HandlerConfig{Limits: limits, SummaryModel: base, ValidateSummary: noOpSummaryValidator})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, _ := handler.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		schema.SystemAgenticMessage("system"), schema.UserAgenticMessage("initial request"),
		schema.UserAgenticMessage(strings.Repeat("old history ", 450)),
		assistantMessage(strings.Repeat("recent answer ", 3), nil), schema.UserAgenticMessage("latest request"),
	}}
	_, got, err := handler.BeforeModelRewriteState(ctx, state, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil || base.generateCalls != 1 || !sameMessages(got.Messages, state.Messages) {
		t.Fatalf("failed summary fallback calls=%d, err=%v", base.generateCalls, err)
	}
	_, _, err = handler.BeforeModelRewriteState(ctx, state, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil || base.generateCalls != 1 {
		t.Fatalf("same prefix retried summary: calls=%d err=%v", base.generateCalls, err)
	}
}

func TestAgenticHandlerRejectsUnparseableSummaryReferences(t *testing.T) {
	limits, _ := NewLimits(10000, 2000)
	base := &fakeAgenticModel{response: assistantMessage("source m_bad", nil)}
	handler, err := NewAgenticHandler(context.Background(), HandlerConfig{
		Limits: limits, SummaryModel: base,
		ValidateSummary: func(context.Context, string) error { return errors.New("invalid material reference") },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, _ := handler.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		schema.SystemAgenticMessage("system"), schema.UserAgenticMessage("initial request"),
		schema.UserAgenticMessage(strings.Repeat("old history ", 450)),
		assistantMessage("recent answer", nil), schema.UserAgenticMessage("latest request"),
	}}
	_, got, err := handler.BeforeModelRewriteState(ctx, state, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil || !sameMessages(got.Messages, state.Messages) {
		t.Fatalf("invalid summary was applied: err=%v", err)
	}
}

func TestAgenticHandlerRejectsOverLimitWhenSummaryFails(t *testing.T) {
	limits, _ := NewLimits(10000, 2000)
	base := &fakeAgenticModel{generateErr: errors.New("summary unavailable")}
	handler, err := NewAgenticHandler(context.Background(), HandlerConfig{Limits: limits, SummaryModel: base, ValidateSummary: noOpSummaryValidator})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, _ := handler.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		schema.SystemAgenticMessage("system"), schema.UserAgenticMessage("initial request"),
		schema.UserAgenticMessage(strings.Repeat("old history ", 300)),
		assistantMessage(strings.Repeat("old answer ", 300), nil),
		schema.UserAgenticMessage(strings.Repeat("old note ", 300)),
		assistantMessage("recent answer", nil), schema.UserAgenticMessage("latest request"),
	}}
	_, _, err = handler.BeforeModelRewriteState(ctx, state, &adk.TypedModelContext[*schema.AgenticMessage]{})
	var limitErr *ContextLimitError
	if !errors.As(err, &limitErr) || limitErr.Kind != ErrorHistoryExceedsLimit || base.generateCalls != 1 {
		t.Fatalf("over-limit summary failure: calls=%d err=%v", base.generateCalls, err)
	}
}

type fakeAgenticModel struct {
	generateCalls int
	streamCalls   int
	lastInput     []*schema.AgenticMessage
	lastMaxTokens int
	response      *schema.AgenticMessage
	generateErr   error
}

func (m *fakeAgenticModel) Generate(_ context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.generateCalls++
	m.lastInput = input
	if options := model.GetCommonOptions(nil, opts...); options.MaxTokens != nil {
		m.lastMaxTokens = *options.MaxTokens
	}
	return m.response, m.generateErr
}

func (m *fakeAgenticModel) Stream(_ context.Context, _ []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.streamCalls++
	if options := model.GetCommonOptions(nil, opts...); options.MaxTokens != nil {
		m.lastMaxTokens = *options.MaxTokens
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{m.response}), nil
}

func assistantMessage(text string, usage *schema.TokenUsage) *schema.AgenticMessage {
	msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: text})}}
	if usage != nil {
		msg.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: usage}
	}
	return msg
}

func toolCall(id, name string) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: id, Name: name, Arguments: `{}`})}}
}

func toolResult(id, name, text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: id, Name: name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: text}}}})}}
}

func textContent(msg *schema.AgenticMessage) string {
	var result strings.Builder
	if msg == nil {
		return ""
	}
	for _, block := range msg.ContentBlocks {
		if block != nil && block.UserInputText != nil {
			result.WriteString(block.UserInputText.Text)
		}
		if block != nil && block.AssistantGenText != nil {
			result.WriteString(block.AssistantGenText.Text)
		}
	}
	return result.String()
}

func noOpSummaryValidator(context.Context, string) error { return nil }
