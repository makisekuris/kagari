package agentctx

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"kagari/internal/channel"
	"kagari/internal/inputbudget"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/textpage"
)

type contextDeliveryReply struct {
	ActionID   string                          `json:"action_id,omitempty"`
	OK         bool                            `json:"ok"`
	Error      string                          `json:"error,omitempty"`
	Task       *personal.Task                  `json:"task,omitempty"`
	Delivery   *personal.DeliveryIntent        `json:"delivery,omitempty"`
	Deliveries *personal.DeliveryPage          `json:"deliveries,omitempty"`
	Control    *personal.DeliveryControlResult `json:"control,omitempty"`
}

func contextDeliveryIntent(id int64, purpose personal.DeliveryPurpose, state personal.DeliveryIntentState) personal.DeliveryIntent {
	created := time.Date(2026, time.November, 13, 8, 30, 0, 0, time.UTC)
	receipt := &channel.Receipt{Channel: "telegram", Account: "owner", Conversation: "chat-1", MessageID: fmt.Sprintf("receipt-%d", id)}
	return personal.DeliveryIntent{
		ID: id, EventID: id + 1000, Purpose: purpose,
		Draft:  personal.DeliveryDraft{Text: "FULL ORIGINAL DRAFT " + strings.Repeat("result text ", 240), Attachments: []string{"asset-second", "asset-first"}},
		Scopes: []personal.DeliveryScope{{TaskID: 41, Revision: 3, RunID: 73}}, State: state,
		CreatedAt: created,
		Progress: &personal.DeliveryProgress{
			AcceptedItems: 1,
			FullyAccepted: false,
			Items: []personal.DeliveryItem{
				{OutboxID: id*10 + 1, DeliveryPart: 1, Text: "accepted chunk " + strings.Repeat("sent words ", 40), Status: "sent", Receipt: receipt, Attempts: 1, NextAttemptAt: created.Add(time.Second)},
				{OutboxID: id*10 + 2, DeliveryPart: 2, AttachmentID: "asset-second", Text: "pending file text " + strings.Repeat("pending words ", 40), Status: "pending", Detail: "waiting for channel", Attempts: 2, NextAttemptAt: created.Add(time.Minute)},
				{OutboxID: id*10 + 3, DeliveryPart: 3, AttachmentID: "asset-first", Text: "unknown result text " + strings.Repeat("uncertain words ", 40), Status: "uncertain", Detail: "remote outcome unknown", Attempts: 1, NextAttemptAt: created.Add(2 * time.Minute)},
				{OutboxID: id*10 + 4, DeliveryPart: 4, Text: "suppressed tail " + strings.Repeat("suppressed words ", 40), Status: "suppressed", Detail: "delivery_part_failed", Attempts: 0, NextAttemptAt: created.Add(3 * time.Minute)},
			},
		},
	}
}

func contextDeliveryControl(action personal.DeliveryControlAction, taskID, originalID, resultID int64) personal.DeliveryControlResult {
	request := personal.DeliveryControlRequest{Action: action, TaskID: taskID, DeliveryID: originalID}
	return personal.DeliveryControlResult{
		ID: 800 + resultID, Request: request, DeliveryID: resultID,
		CreatedAt: time.Date(2026, time.November, 13, 9, 0, 0, 0, time.UTC),
	}
}

func contextDeliveryToolRound(t *testing.T, results map[string]string) inputbudget.ToolRound {
	t.Helper()
	calls := make([]*schema.ContentBlock, 0, len(results))
	toolResults := make([]*schema.ContentBlock, 0, len(results))
	tools := make([]string, 0, len(results))
	for i, name := range []string{"get_message", "get_delivery", "list_deliveries", "stop_delivery", "resume_task_delivery", "resend_delivery", "cancel_task", "custom_tool"} {
		result, ok := results[name]
		if !ok {
			continue
		}
		callID := fmt.Sprintf("delivery-call-%d", i+1)
		calls = append(calls, schema.NewContentBlock(&schema.FunctionToolCall{CallID: callID, Name: name, Arguments: `{}`}))
		toolResults = append(toolResults, schema.NewContentBlock(&schema.FunctionToolResult{CallID: callID, Name: name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: result}}}}))
		tools = append(tools, name)
	}
	if len(calls) == 0 {
		t.Fatal("delivery test round requires at least one tool result")
	}
	return inputbudget.ToolRound{Key: "delivery-facts-round", Complete: true, ToolNames: tools, Messages: []*schema.AgenticMessage{
		{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: calls},
		{Role: schema.AgenticRoleTypeUser, ContentBlocks: toolResults},
	}}
}

func contextJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func contextResultText(t *testing.T, messages []*schema.AgenticMessage, name string) string {
	t.Helper()
	for _, message := range messages {
		for _, block := range message.ContentBlocks {
			if result := block.FunctionToolResult; result != nil && result.Name == name && len(result.Content) == 1 && result.Content[0].Text != nil {
				return result.Content[0].Text.Text
			}
		}
	}
	t.Fatalf("result %q missing from reduced tool round", name)
	return ""
}

func assertDeliveryContextCard(t *testing.T, got *personal.DeliveryIntent, original personal.DeliveryIntent) {
	t.Helper()
	if got == nil {
		t.Fatal("delivery card missing")
	}
	want := personal.DeliveryCard(original)
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("delivery card=%+v progress=%+v, want=%+v progress=%+v", *got, got.Progress, want, want.Progress)
	}
}

func assertDeliveryFactsSurviveTextCompaction(t *testing.T, got *personal.DeliveryIntent, original personal.DeliveryIntent) {
	t.Helper()
	if got == nil {
		t.Fatal("delivery facts missing")
	}
	if !reflect.DeepEqual(personal.DeliveryCard(*got), personal.DeliveryCard(original)) {
		t.Fatalf("delivery status facts changed during text compaction: got=%+v want=%+v", personal.DeliveryCard(*got), personal.DeliveryCard(original))
	}
}

func TestDeliveryToolRoundReductionKeepsCardsAndOriginalArchive(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Minute)
	run := testkit.CreateRunningTask(t, store, "delivery-context", personal.TaskInput{Goal: "Keep delivery facts during compression"})
	scope := personal.ReadScope{RunID: run.ID}
	now := time.Date(2026, time.November, 13, 9, 30, 0, 0, time.UTC)
	intent := contextDeliveryIntent(42, personal.DeliveryResult, personal.DeliveryIntentRendered)
	secondIntent := contextDeliveryIntent(43, personal.DeliveryStatus, personal.DeliveryIntentSuppressed)
	secondIntent.Progress = nil
	secondIntent.ContentOmitted = false
	stopControl := contextDeliveryControl(personal.DeliveryStop, 41, 0, 91)
	resumeControl := contextDeliveryControl(personal.DeliveryResume, 41, 0, 92)
	resendControl := contextDeliveryControl(personal.DeliveryResend, 0, intent.ID, 93)
	cancelTask := personal.Task{ID: 41, Revision: 3, Title: "Canceled task", Goal: "A saved cancellation result", Status: personal.TaskCancelled, UpdatedAt: now}

	message := personal.RecordText{
		Delivery: &intent, Kind: "message", ID: 501, Section: "", Page: textpage.Page{
			Text: "ORIGINAL MESSAGE BODY " + strings.Repeat("message body ", 200), Offset: 0, End: 200, TotalRunes: 4000,
			NextOffset: intPointer(200), HasMore: true, Version: strings.Repeat("a", 64),
		},
	}
	values := map[string]string{
		"get_message":          contextJSON(t, message),
		"get_delivery":         contextJSON(t, contextDeliveryReply{OK: true, Delivery: &intent}),
		"list_deliveries":      contextJSON(t, contextDeliveryReply{OK: true, Deliveries: &personal.DeliveryPage{Items: []personal.DeliveryIntent{intent, secondIntent}, StoppedTaskIDs: []int64{41}}}),
		"stop_delivery":        contextJSON(t, contextDeliveryReply{OK: true, ActionID: "action-stop", Control: &stopControl, Delivery: &secondIntent}),
		"resume_task_delivery": contextJSON(t, contextDeliveryReply{OK: true, ActionID: "action-resume", Control: &resumeControl, Delivery: &secondIntent}),
		"resend_delivery":      contextJSON(t, contextDeliveryReply{OK: true, ActionID: "action-resend", Control: &resendControl, Delivery: &intent}),
		"cancel_task": contextJSON(t, contextDeliveryReply{OK: true, Task: &cancelTask, Deliveries: &personal.DeliveryPage{
			Items: []personal.DeliveryIntent{secondIntent, contextDeliveryIntent(44, personal.DeliveryControl, personal.DeliveryIntentRendered)}, StoppedTaskIDs: []int64{41},
		}}),
		"custom_tool": strings.Repeat("ordinary saved observation ", 160),
	}
	round := contextDeliveryToolRound(t, values)
	originalBody, err := json.Marshal(round.Messages)
	if err != nil {
		t.Fatal(err)
	}
	reduced, err := reduceToolRound(ctx, store, scope, round)
	if err != nil {
		t.Fatal(err)
	}

	var messageCard struct {
		personal.RecordText
		ContentOmitted       bool   `json:"content_omitted"`
		ArchivedToolMaterial string `json:"archived_tool_material"`
	}
	if err := json.Unmarshal([]byte(contextResultText(t, reduced, "get_message")), &messageCard); err != nil {
		t.Fatal(err)
	}
	if messageCard.ID != 501 || messageCard.Kind != "message" || messageCard.Page.Text != "" || !messageCard.ContentOmitted || messageCard.Page.End != 200 || !messageCard.Page.HasMore || messageCard.Page.Version != strings.Repeat("a", 64) {
		t.Fatalf("compacted message lost its direct read cursor: %+v", messageCard)
	}
	assertDeliveryContextCard(t, messageCard.Delivery, intent)
	if len(personal.MaterialReferences(messageCard.ArchivedToolMaterial)) != 1 {
		t.Fatalf("message archive reference missing: %q", messageCard.ArchivedToolMaterial)
	}

	var getReply contextDeliveryReply
	if err := json.Unmarshal([]byte(contextResultText(t, reduced, "get_delivery")), &getReply); err != nil {
		t.Fatal(err)
	}
	assertDeliveryFactsSurviveTextCompaction(t, getReply.Delivery, intent)
	var listReply contextDeliveryReply
	if err := json.Unmarshal([]byte(contextResultText(t, reduced, "list_deliveries")), &listReply); err != nil {
		t.Fatal(err)
	}
	if listReply.Deliveries == nil || len(listReply.Deliveries.Items) != 2 || !reflect.DeepEqual(listReply.Deliveries.StoppedTaskIDs, []int64{41}) {
		t.Fatalf("delivery list card lost page facts: %+v", listReply.Deliveries)
	}
	assertDeliveryContextCard(t, &listReply.Deliveries.Items[0], intent)
	assertDeliveryContextCard(t, &listReply.Deliveries.Items[1], secondIntent)

	for name, want := range map[string]personal.DeliveryControlResult{
		"stop_delivery":        stopControl,
		"resume_task_delivery": resumeControl,
		"resend_delivery":      resendControl,
	} {
		var reply contextDeliveryReply
		if err := json.Unmarshal([]byte(contextResultText(t, reduced, name)), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Control == nil || !reflect.DeepEqual(*reply.Control, want) {
			t.Fatalf("%s compact control=%+v, want %+v", name, reply.Control, want)
		}
		if len(personal.MaterialReferences(contextResultText(t, reduced, name))) != 1 {
			t.Fatalf("%s archive reference missing", name)
		}
	}
	var cancelReply contextDeliveryReply
	if err := json.Unmarshal([]byte(contextResultText(t, reduced, "cancel_task")), &cancelReply); err != nil {
		t.Fatal(err)
	}
	if cancelReply.Task == nil || cancelReply.Task.ID != cancelTask.ID || cancelReply.Task.Status != personal.TaskCancelled || cancelReply.Deliveries == nil || len(cancelReply.Deliveries.Items) != 2 {
		t.Fatalf("cancel task lost Task/control deliveries: %+v", cancelReply)
	}
	assertDeliveryContextCard(t, &cancelReply.Deliveries.Items[0], secondIntent)

	ordinary := contextResultText(t, reduced, "custom_tool")
	if !strings.Contains(ordinary, "read_material") || !strings.Contains(ordinary, "material m_") {
		t.Fatalf("ordinary same-round output was not archived: %q", ordinary)
	}
	if strings.Contains(contextResultText(t, reduced, "get_delivery"), "FULL ORIGINAL DRAFT") || strings.Contains(contextResultText(t, reduced, "get_delivery"), "accepted chunk") {
		t.Fatal("delivery body leaked into compact card")
	}
	if originalText := round.Messages[1].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text; !strings.Contains(originalText, "ORIGINAL MESSAGE BODY") {
		t.Fatal("compression mutated the source tool result")
	}

	materials, err := store.ListMaterials(ctx, scope, run.ID, personal.ListPage{})
	if err != nil || len(materials.Items) != 1 {
		t.Fatalf("archived rounds=%+v err=%v", materials, err)
	}
	var archivedBody strings.Builder
	request := textpage.Request{Limit: textpage.MaxTextLimit}
	for {
		archived, err := store.ReadMaterial(ctx, scope, materials.Items[0].ID, "body", request)
		if err != nil {
			t.Fatalf("read archived tool round: %v", err)
		}
		archivedBody.WriteString(archived.Page.Text)
		if !archived.Page.HasMore {
			break
		}
		request.Offset, request.Version = archived.Page.End, archived.Page.Version
	}
	if archivedBody.String() != string(originalBody) {
		t.Fatal("archived tool round changed")
	}
}

func intPointer(v int) *int { return &v }

func TestBoundDeliveryResponsesKeepProgressItemsAndPageLists(t *testing.T) {
	intent := contextDeliveryIntent(42, personal.DeliveryResult, personal.DeliveryIntentRendered)
	getTool, err := utils.InferTool("get_delivery", "test", func(context.Context, struct{}) (contextDeliveryReply, error) {
		return contextDeliveryReply{OK: true, Delivery: &intent}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	getOutput := invokeBoundContextTool(t, getTool, textpage.WithMaxBytes(context.Background(), 2200), `{}`)
	var getReply contextDeliveryReply
	if err := json.Unmarshal([]byte(getOutput), &getReply); err != nil {
		t.Fatal(err)
	}
	assertDeliveryFactsSurviveTextCompaction(t, getReply.Delivery, intent)
	if len(getOutput) > 2200 {
		t.Fatalf("bounded get_delivery is %d bytes", len(getOutput))
	}

	page := personal.DeliveryPage{Items: make([]personal.DeliveryIntent, 0, 7), StoppedTaskIDs: []int64{41}, NextBeforeID: 1}
	for id := int64(90); id >= 30; id -= 10 {
		page.Items = append(page.Items, contextDeliveryIntent(id, personal.DeliveryResult, personal.DeliveryIntentRendered))
	}
	listTool, err := utils.InferTool("list_deliveries", "test", func(context.Context, struct{}) (contextDeliveryReply, error) {
		return contextDeliveryReply{OK: true, Deliveries: &page}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	listOutput := invokeBoundContextTool(t, listTool, textpage.WithMaxBytes(context.Background(), 3800), `{}`)
	var listReply contextDeliveryReply
	if err := json.Unmarshal([]byte(listOutput), &listReply); err != nil {
		t.Fatal(err)
	}
	if len(listOutput) > 3800 || listReply.Deliveries == nil || len(listReply.Deliveries.Items) == 0 || len(listReply.Deliveries.Items) >= len(page.Items) {
		t.Fatalf("delivery list was not compactly paged: bytes=%d page=%+v", len(listOutput), listReply.Deliveries)
	}
	last := listReply.Deliveries.Items[len(listReply.Deliveries.Items)-1]
	if listReply.Deliveries.NextBeforeID != last.ID || !reflect.DeepEqual(listReply.Deliveries.StoppedTaskIDs, page.StoppedTaskIDs) {
		t.Fatalf("compacted delivery cursor/stopped IDs=(%d,%v), last ID=%d", listReply.Deliveries.NextBeforeID, listReply.Deliveries.StoppedTaskIDs, last.ID)
	}
	for _, got := range listReply.Deliveries.Items {
		if got.Progress == nil || len(got.Progress.Items) != len(intent.Progress.Items) {
			t.Fatalf("list compaction dropped per-delivery progress items: %+v", got)
		}
		for i, item := range got.Progress.Items {
			if item.OutboxID != got.ID*10+int64(i+1) || item.Status != intent.Progress.Items[i].Status {
				t.Fatalf("list item %d lost identity/status: %+v", i, item)
			}
		}
		assertDeliveryFactsSurviveTextCompaction(t, &got, contextDeliveryIntent(got.ID, personal.DeliveryResult, personal.DeliveryIntentRendered))
	}
}

func TestBudgetHandlerArchivesDeliveryFactsBeforeSummaryAndKeepsCards(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Minute)
	run := testkit.CreateRunningTask(t, store, "delivery-summary", personal.TaskInput{Goal: "Keep state during summary"})
	scope := personal.ReadScope{RunID: run.ID}
	limits, err := inputbudget.NewLimits(22000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	model := &contextSummaryModel{}
	bounded, err := inputbudget.NewAgenticModel(model, inputbudget.ModelConfig{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := BudgetHandler(ctx, bounded, store, scope)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, err = handler.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	intent := contextDeliveryIntent(42, personal.DeliveryResult, personal.DeliveryIntentRendered)
	resultText := contextJSON(t, contextDeliveryReply{OK: true, Delivery: &intent})
	toolRound := contextDeliveryToolRound(t, map[string]string{"get_delivery": resultText})
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{
		schema.SystemAgenticMessage("system"),
		schema.UserAgenticMessage("Initial task request"),
		schema.UserAgenticMessage("PRE-CONTROL CONTEXT " + strings.Repeat("old decision details ", 650)),
		toolRound.Messages[0], toolRound.Messages[1],
		schema.UserAgenticMessage("POST-CONTROL CONTEXT " + strings.Repeat("later history details ", 150)),
		{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "latest-call", Name: "latest_lookup", Arguments: `{}`})}},
		{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "latest-call", Name: "latest_lookup", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "latest lookup result"}}}})}},
		schema.UserAgenticMessage("latest user message"),
	}}
	ctx, rewritten, err := handler.BeforeModelRewriteState(ctx, state, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil {
		t.Fatal(err)
	}
	if rewritten == nil || model.calls != 1 {
		t.Fatalf("summary calls=%d rewritten=%v", model.calls, rewritten != nil)
	}
	var summaryInput strings.Builder
	for _, message := range model.inputs[0] {
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		summaryInput.Write(encoded)
	}
	if !strings.Contains(summaryInput.String(), "PRE-CONTROL CONTEXT") || strings.Contains(summaryInput.String(), "receipt-42") || strings.Contains(summaryInput.String(), "FULL ORIGINAL DRAFT") || strings.Contains(summaryInput.String(), "delivery-call") {
		t.Fatalf("delivery facts entered model summary input: %s", summaryInput.String())
	}
	var compacted string
	for _, message := range rewritten.Messages {
		for _, block := range message.ContentBlocks {
			if result := block.FunctionToolResult; result != nil && result.Name == "get_delivery" && len(result.Content) == 1 && result.Content[0].Text != nil {
				compacted = result.Content[0].Text.Text
			}
		}
	}
	var reply contextDeliveryReply
	if compacted == "" || json.Unmarshal([]byte(compacted), &reply) != nil {
		t.Fatalf("compacted delivery result missing or invalid: %q", compacted)
	}
	assertDeliveryContextCard(t, reply.Delivery, intent)
}

type contextSummaryModel struct {
	calls  int
	inputs [][]*schema.AgenticMessage
}

func (m *contextSummaryModel) Generate(_ context.Context, messages []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	m.calls++
	m.inputs = append(m.inputs, append([]*schema.AgenticMessage(nil), messages...))
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.AssistantGenText{Text: "Earlier task notes summarized."}),
	}}, nil
}

func (m *contextSummaryModel) Stream(ctx context.Context, messages []*schema.AgenticMessage, options ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	response, err := m.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{response}), nil
}

func invokeBoundContextTool(t *testing.T, base tool.BaseTool, ctx context.Context, args string) string {
	t.Helper()
	info, err := base.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, bounded := range BoundTools([]tool.BaseTool{base}) {
		invokable, ok := bounded.(tool.InvokableTool)
		if !ok {
			t.Fatalf("%s is not invokable", info.Name)
		}
		raw, err := invokable.InvokableRun(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	t.Fatalf("%s was not wrapped", info.Name)
	return ""
}

type contextDeliveryRecordStore struct {
	ReadRepository
	record personal.RecordText
	body   string
}

func (s contextDeliveryRecordStore) ReadRecord(_ context.Context, _ personal.ReadScope, request personal.RecordRequest) (personal.RecordText, error) {
	page, err := textpage.Read(s.body, request.Request, false)
	if err != nil {
		return personal.RecordText{}, err
	}
	page.Version = textpage.Version(s.body)
	record := s.record
	record.Page = page
	return record, nil
}

func TestGetMessageCompactsDeliveryAfterPagingAndCanRereadBody(t *testing.T) {
	intent := contextDeliveryIntent(42, personal.DeliveryResult, personal.DeliveryIntentRendered)
	body := "MESSAGE-BODY " + strings.Repeat("full original history text ", 240)
	store := contextDeliveryRecordStore{
		record: personal.RecordText{Delivery: &intent, Kind: "message", ID: 501},
		body:   body,
	}
	tools, err := ReadTools(ReadToolsConfig{Store: store, Scope: personal.ReadScope{EventID: 9}})
	if err != nil {
		t.Fatal(err)
	}
	var getMessage tool.BaseTool
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "get_message" {
			getMessage = candidate
			break
		}
	}
	if getMessage == nil {
		t.Fatal("get_message tool missing")
	}
	ctx := textpage.WithMaxBytes(context.Background(), 1800)
	firstRaw := invokeBoundContextTool(t, getMessage, ctx, `{"history_id":501,"limit":4000}`)
	var first struct {
		personal.RecordText
		ContentOmitted bool `json:"content_omitted"`
	}
	if err := json.Unmarshal([]byte(firstRaw), &first); err != nil {
		t.Fatal(err)
	}
	if len(firstRaw) > 1800 || first.ID != 501 || first.Page.Text == "" || !first.Page.HasMore || first.Page.NextOffset == nil || first.Page.End != len([]rune(first.Page.Text)) {
		t.Fatalf("bounded message page lost its continuation facts: bytes=%d record=%+v", len(firstRaw), first)
	}
	assertDeliveryContextCard(t, first.Delivery, intent)
	if !first.Delivery.ContentOmitted {
		t.Fatal("compacted message did not mark delivery content as omitted")
	}

	args := contextJSON(t, struct {
		HistoryID int64 `json:"history_id"`
		textpage.Request
	}{HistoryID: first.ID, Request: textpage.Request{Offset: first.Page.End, Limit: 100, Version: first.Page.Version}})
	nextRaw := invokeBoundContextTool(t, getMessage, ctx, args)
	var next personal.RecordText
	if err := json.Unmarshal([]byte(nextRaw), &next); err != nil {
		t.Fatal(err)
	}
	expected := []rune(body)[first.Page.End:next.Page.End]
	if next.Page.Offset != first.Page.End || next.Page.End <= first.Page.End || next.Page.Text != string(expected) || next.Page.Version != first.Page.Version {
		t.Fatalf("get_message did not reread the archived body: first_end=%d next=%+v", first.Page.End, next.Page)
	}
}
