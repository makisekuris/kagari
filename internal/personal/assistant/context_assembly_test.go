package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"kagari/internal/domain"
	"kagari/internal/inputbudget"
	"kagari/internal/personal"
)

type assemblyTestTool struct{ info *schema.ToolInfo }

func (t assemblyTestTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func TestAssembleTurnPreservesRequiredFieldsAndCopiesInput(t *testing.T) {
	trigger := &personal.ScheduleTrigger{
		ScheduleID: 4, ScheduleRevision: 6, ScopeRevision: 7, OccurrenceID: 12,
		ScheduledAt: time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC),
		Timezone:    "UTC", CoverageEligible: true,
	}
	rawTaskInput, err := json.Marshal(personal.TaskInput{Goal: "frozen goal", Context: "frozen context", Trigger: trigger})
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(personal.StageResult{Summary: "A result", Sources: []domain.SourceRef{{ID: "m_0123456789abcdef0123456789abcdef"}}})
	if err != nil {
		t.Fatal(err)
	}
	input := personal.TurnInput{
		Event:          personal.Event{ID: 15, Kind: personal.EventTask, TaskID: 23, Text: strings.Repeat("用户请求🙂", 150)},
		Tasks:          []personal.Task{{ID: 23, Title: "Current", Goal: strings.Repeat("不可裁剪目标", 100), Context: strings.Repeat("不可裁剪上下文", 100), Revision: 5, Status: personal.TaskRunning}},
		Memories:       []personal.Memory{{Key: "private.fact", Value: "memory must be separately injected"}},
		RepliedMessage: &personal.Message{OutboxID: 31, HistoryID: 28, EventID: 14, Role: "assistant", MessageID: "outbox-message", Text: "The exact sent reply."},
		History:        []personal.Message{{HistoryID: 28, EventID: 14, Role: "assistant", Text: "The exact sent reply."}},
		CurrentRun:     &personal.Run{ID: 35, TaskID: 23, Revision: 5, Input: string(rawTaskInput), Result: string(result), ContextManifest: strings.Repeat("internal trace", 100)},
	}
	limits, err := inputbudget.NewLimits(24_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}

	assembled, err := assembleTurn(context.Background(), input, "personal instruction", nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if assembled.Event.Text != input.Event.Text {
		t.Fatal("current event text was changed")
	}
	if len(assembled.Tasks) == 0 || assembled.Tasks[0].Goal != input.Tasks[0].Goal || assembled.Tasks[0].Context != input.Tasks[0].Context || assembled.Tasks[0].Revision != input.Tasks[0].Revision {
		t.Fatalf("current task was not preserved: %+v", assembled.Tasks)
	}
	if assembled.CurrentRun == nil || assembled.CurrentRun.Input != input.CurrentRun.Input || assembled.CurrentRun.Revision != input.CurrentRun.Revision {
		t.Fatalf("current run input/revision changed: %+v", assembled.CurrentRun)
	}
	if strings.Contains(assembled.CurrentRun.ContextManifest, "internal trace") {
		t.Fatal("internal context manifest was copied into the prompt")
	}
	if assembled.RepliedMessage == nil || assembled.RepliedMessage.OutboxID != 31 || assembled.RepliedMessage.HistoryID != 28 || assembled.RepliedMessage.Text != input.RepliedMessage.Text {
		t.Fatalf("explicit reply identity/content changed: %+v", assembled.RepliedMessage)
	}
	if len(assembled.History) != 1 || assembled.History[0].HistoryID != 28 || assembled.History[0].Text != input.History[0].Text {
		t.Fatalf("explicitly replied history was not retained: %+v", assembled.History)
	}
	if assembled.Memories != nil {
		t.Fatalf("memory was copied into assembled turn: %+v", assembled.Memories)
	}
	if input.Memories[0].Value != "memory must be separately injected" || input.CurrentRun.Result != string(result) || input.CurrentRun.ContextManifest == "" {
		t.Fatal("assembleTurn mutated its input")
	}
	var card personal.RunResultCard
	if err := json.Unmarshal([]byte(assembled.CurrentRun.Result), &card); err != nil || !strings.Contains(card.ReadRun, "run_id=35") {
		t.Fatalf("current run result lacks a read_run entry: %+v, %v", card, err)
	}
}

func TestAssembleTurnKeepsLongRequiredRequestBelowHardLimit(t *testing.T) {
	limits, err := inputbudget.NewLimits(14_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	request := strings.Repeat("x", 8_500)
	assembled, err := assembleTurn(context.Background(), personal.TurnInput{Event: personal.Event{ID: 1, Kind: personal.EventUser, Text: request}}, "instruction", nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if assembled.Event.Text != request {
		t.Fatal("long current request was cropped")
	}
	if estimate, err := estimateTurn(context.Background(), assembled, "instruction", nil); err != nil || estimate.TotalBytes > limits.Hard {
		t.Fatalf("assembled request estimate=%+v error=%v hard=%d", estimate, err, limits.Hard)
	}
}

func TestAssembleTurnRejectsRequiredInputAboveHardLimit(t *testing.T) {
	limits, err := inputbudget.NewLimits(14_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	_, err = assembleTurn(context.Background(), personal.TurnInput{Event: personal.Event{ID: 2, Kind: personal.EventUser, Text: strings.Repeat("required", 2_000)}}, "instruction", nil, limits)
	var limitErr *inputbudget.ContextLimitError
	if !errors.As(err, &limitErr) || limitErr.Kind != inputbudget.ErrorRequiredInputExceedsLimit || limitErr.Limit != limits.Hard {
		t.Fatalf("required overflow error=%v", err)
	}
}

func TestAssembleTurnUsesGetEventReferencesForOversizedPendingInputs(t *testing.T) {
	limits, err := inputbudget.NewLimits(14_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	large := personal.Event{ID: 51, Kind: personal.EventMaterial, TaskID: 8, ReplyOutboxID: 62, Text: strings.Repeat("large pending body", 600)}
	small := personal.Event{ID: 52, Kind: personal.EventMaterial, TaskID: 9, Text: "small pending body"}
	input := personal.TurnInput{
		Event:         personal.Event{ID: 50, Kind: personal.EventUser, Text: "Keep the current message whole."},
		PendingInputs: []personal.Event{large, small},
		PendingTaskInputs: []personal.Event{{
			ID: 53, Kind: personal.EventUser, TaskID: 8, Text: strings.Repeat("task input", 1_500),
		}},
	}
	assembled, err := assembleTurn(context.Background(), input, "instruction", nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if assembled.PendingInputs[0].Text == large.Text || !strings.Contains(assembled.PendingInputs[0].Text, "get_event event_id=51") {
		t.Fatalf("oversized pending body lacks an exact retrieval entry: %+v", assembled.PendingInputs[0])
	}
	if assembled.PendingInputs[0].ID != large.ID || assembled.PendingInputs[0].Kind != large.Kind || assembled.PendingInputs[0].TaskID != large.TaskID || assembled.PendingInputs[0].ReplyOutboxID != large.ReplyOutboxID {
		t.Fatalf("pending event identity/association changed: %+v", assembled.PendingInputs[0])
	}
	if assembled.PendingInputs[1].Text != small.Text {
		t.Fatalf("small pending body was not expanded: %+v", assembled.PendingInputs[1])
	}
	if assembled.PendingTaskInputs[0].Text == input.PendingTaskInputs[0].Text || !strings.Contains(assembled.PendingTaskInputs[0].Text, "get_event event_id=53") {
		t.Fatalf("oversized task input lacks a retrieval entry: %+v", assembled.PendingTaskInputs[0])
	}
	if input.PendingInputs[0].Text != large.Text || input.PendingTaskInputs[0].Text == assembled.PendingTaskInputs[0].Text {
		t.Fatal("pending event references mutated the original input")
	}
}

func TestAssembleTurnCountsRegisteredToolSchema(t *testing.T) {
	limits, err := inputbudget.NewLimits(12_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	tooLarge := assemblyTestTool{info: &schema.ToolInfo{Name: "large_tool", Desc: strings.Repeat("schema", 2_000)}}
	_, err = assembleTurn(context.Background(), personal.TurnInput{Event: personal.Event{ID: 3, Kind: personal.EventUser, Text: "hello"}}, "instruction", []tool.BaseTool{tooLarge}, limits)
	var limitErr *inputbudget.ContextLimitError
	if !errors.As(err, &limitErr) || limitErr.Kind != inputbudget.ErrorToolsSchemaExceedsLimit {
		t.Fatalf("tool schema overflow error=%v", err)
	}
}

func TestAssembleTurnKeepsRepliedTaskAndPrunesOldestUnrelatedHistory(t *testing.T) {
	limits, err := inputbudget.NewLimits(14_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	replied := personal.Message{OutboxID: 11, HistoryID: 70, EventID: 30, Role: "assistant", Text: "Exact reply tied to the task."}
	input := personal.TurnInput{
		Event:             personal.Event{ID: 40, Kind: personal.EventUser, TaskID: 7, ReplyOutboxID: replied.OutboxID, Text: "Continue from that."},
		RepliedMessage:    &replied,
		ReferencedTaskIDs: []int64{9},
		Tasks: []personal.Task{
			{ID: 7, Title: "Current task", Goal: "Current goal", Context: "Current constraints", Revision: 4, Status: personal.TaskRunning},
			{ID: 9, Title: "Replied task", Goal: strings.Repeat("detail", 500), Context: "private task detail", Revision: 2, Status: personal.TaskWaitingInput},
			{ID: 10, Title: "Unrelated", Goal: strings.Repeat("unrelated", 300), Revision: 1, Status: personal.TaskCompleted},
		},
		History: []personal.Message{
			{HistoryID: 1, EventID: 1, Role: "user", Text: strings.Repeat("o", 3_300)},
			replied,
			{HistoryID: 72, EventID: 31, Role: "assistant", TaskID: 9, Text: "Relevant task history."},
			{HistoryID: 73, EventID: 32, Role: "user", Text: strings.Repeat("r", 3_200)},
		},
	}
	assembled, err := assembleTurn(context.Background(), input, "instruction", nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if assembled.Tasks[0].ID != 7 || assembled.Tasks[0].Goal != input.Tasks[0].Goal || assembled.Tasks[0].Context != input.Tasks[0].Context {
		t.Fatalf("current task was displaced: %+v", assembled.Tasks)
	}
	var repliedTask *personal.Task
	for i := range assembled.Tasks {
		if assembled.Tasks[i].ID == 9 {
			repliedTask = &assembled.Tasks[i]
		}
	}
	if repliedTask == nil || repliedTask.Title != "Replied task" || repliedTask.Goal != "" || repliedTask.Context != "" {
		t.Fatalf("replied task card was displaced or copied in full: %+v", assembled.Tasks)
	}
	historyIDs := make(map[int64]bool)
	for _, message := range assembled.History {
		historyIDs[message.HistoryID] = true
	}
	if !historyIDs[70] || !historyIDs[72] || !historyIDs[73] {
		t.Fatalf("replied or recent relevant history missing: %+v", assembled.History)
	}
	if historyIDs[1] {
		t.Fatal("oldest unrelated history was retained ahead of newer context")
	}
	if !strings.Contains(assembled.RepliedMessage.Text, "Exact reply") || assembled.RepliedMessage.OutboxID != 11 {
		t.Fatal("explicit reply content or identity changed")
	}
}

func TestAssembleTurnBoundsRunSummaryWithoutCuttingSourceIDs(t *testing.T) {
	limits, err := inputbudget.NewLimits(18_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	const sourceID = "m_0123456789abcdef0123456789abcdef"
	raw, err := json.Marshal(personal.StageResult{
		Summary: strings.Repeat("The result is useful. ", 900) + " Source " + sourceID,
		Sources: []domain.SourceRef{{ID: sourceID, URL: "https://example.invalid/" + strings.Repeat("x", 1000)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := personal.TurnInput{Event: personal.Event{ID: 5, Kind: personal.EventTask, TaskID: 8, Text: "Report this run."}, CurrentRun: &personal.Run{ID: 91, TaskID: 8, Revision: 3, Input: `{"goal":"fixed"}`, Result: string(raw)}}
	assembled, err := assembleTurn(context.Background(), input, "instruction", nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	var card personal.RunResultCard
	if err := json.Unmarshal([]byte(assembled.CurrentRun.Result), &card); err != nil {
		t.Fatal(err)
	}
	if len(card.Sources) != 1 || card.Sources[0].ID != sourceID || !strings.Contains(card.ReadRun, "run_id=91") {
		t.Fatalf("source reference or read_run entry was cut: %+v", card)
	}
	if card.Sources[0].URL != "https://example.invalid/"+strings.Repeat("x", 1000) {
		t.Fatalf("source citation was rewritten: %+v", card.Sources[0])
	}
	if len(card.MaterialEvidenceIDs) != 1 || card.MaterialEvidenceIDs[0] != sourceID {
		t.Fatalf("source ID embedded in the summary was not preserved: %+v", card.MaterialEvidenceIDs)
	}
	if strings.Contains(card.Summary, sourceID) || len(card.Summary) >= len(strings.Repeat("The result is useful. ", 900)) {
		t.Fatalf("summary was not bounded separately from its full reference: bytes=%d", len(card.Summary))
	}
	if input.CurrentRun.Result != string(raw) {
		t.Fatal("run result in original input was changed")
	}
	if estimate, err := estimateTurn(context.Background(), assembled, "instruction", nil); err != nil || estimate.TotalBytes > limits.Target {
		t.Fatalf("run summary assembly estimate=%+v error=%v target=%d", estimate, err, limits.Target)
	}
}
