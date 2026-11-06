package personal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"kagari/internal/textpage"
)

func invokeTaskInputTool(t *testing.T, tools []tool.BaseTool, args addTaskInputArgs) (addTaskInputReply, string) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name != "add_task_input" {
			continue
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatal("add_task_input is not invokable")
		}
		output, err := invokable.InvokableRun(context.Background(), string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		var reply addTaskInputReply
		if err := json.Unmarshal([]byte(output), &reply); err != nil {
			t.Fatalf("decode add_task_input reply %q: %v", output, err)
		}
		return reply, output
	}
	t.Fatal("add_task_input tool not found")
	return addTaskInputReply{}, ""
}

func TestInputToolsReplyWithSelectedOriginalMessages(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, s, 40, 140, "Please add the current details to this task")
	selectedID := acceptInput(t, s, "41", sourceInput("141", "https://example.com/selected"))
	unselectedID := acceptInput(t, s, "42", sourceInput("142", "https://example.org/unselected"))
	task, _ := createTask(t, s, event, "create-tool-input", "Research", "Research the pages", "")
	pending, err := s.PendingInputs(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := &personalTools{store: s, event: event, input: TurnInput{PendingInputs: pending}, taskIDs: make(map[int64]struct{})}
	tools, err := p.inputTools()
	if err != nil || len(tools) != 1 {
		t.Fatalf("inputTools()=(%d,%v), want one tool", len(tools), err)
	}
	reply, raw := invokeTaskInputTool(t, tools, addTaskInputArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, selectedID},
	})
	if !reply.OK || reply.Task == nil || reply.Task.ID != task.ID || len(reply.Inputs) != 2 || len([]rune(raw)) > maxTaskChars {
		t.Fatalf("add_task_input reply=(%+v), chars=%d", reply, runeLen(raw))
	}
	texts := []string{reply.Inputs[0].Text, reply.Inputs[1].Text}
	if !strings.Contains(strings.Join(texts, "\n"), event.Text) || !strings.Contains(strings.Join(texts, "\n"), "https://example.com/selected") ||
		strings.Contains(strings.Join(texts, "\n"), "https://example.org/unselected") {
		t.Fatalf("tool returned the wrong original messages: %+v", reply.Inputs)
	}
	if p.taskID() != task.ID {
		t.Fatalf("task association=%d, want %d", p.taskID(), task.ID)
	}
	var selectedStatus, unselectedStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, selectedID).Scan(&selectedStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, unselectedID).Scan(&unselectedStatus); err != nil {
		t.Fatal(err)
	}
	if selectedStatus != string(EventCompleted) || unselectedStatus != string(EventQueued) {
		t.Fatalf("selected/unselected states=(%q,%q)", selectedStatus, unselectedStatus)
	}
}

func TestInputToolsRejectInvalidSelectionAndNonUserCallback(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, s, 50, 150, "Add current details")
	commandID := acceptInput(t, s, "51", sourceInput("151", "/continue_task https://example.com"))
	task, _ := createTask(t, s, event, "create-invalid-tool-input", "Research", "Research this", "")
	snapshot, err := s.PendingInputs(ctx, event.ID)
	if err != nil || len(snapshot) != 0 {
		t.Fatalf("pending snapshot=(%+v,%v), want no command candidates", snapshot, err)
	}
	p := &personalTools{store: s, event: event, input: TurnInput{PendingInputs: snapshot}, taskIDs: make(map[int64]struct{})}
	tools, err := p.inputTools()
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, commandID},
	})
	if reply.OK || !strings.Contains(reply.Error, "pending URL input") {
		t.Fatalf("command input was accepted: %+v", reply)
	}
	arrivedAfterSnapshot := acceptInput(t, s, "52", sourceInput("152", "https://example.com/arrived-later"))
	lateReply, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, arrivedAfterSnapshot},
	})
	if lateReply.OK || !strings.Contains(lateReply.Error, "pending URL input") {
		t.Fatalf("tool selected an event outside its turn snapshot: %+v", lateReply)
	}
	callback := Event{ID: 999, Kind: EventTask, TaskID: task.ID}
	callbackTools, err := (&personalTools{store: s, event: callback, taskIDs: map[int64]struct{}{task.ID: {}}}).inputTools()
	if err != nil {
		t.Fatal(err)
	}
	callbackReply, _ := invokeTaskInputTool(t, callbackTools, addTaskInputArgs{
		TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{callback.ID},
	})
	if callbackReply.OK || !strings.Contains(callbackReply.Error, "direct user event") {
		t.Fatalf("task callback was allowed to add inputs: %+v", callbackReply)
	}
}

func TestTaskInputProcessingReadsPagesAndAppliesOnlyReportedInputs(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	text := strings.Repeat("汉🙂x", 30)
	event, task, run, ids := createActiveTaskWithInputs(t, s, 80, text, "https://example.com/source")
	var applied []int64
	tools, err := taskInputProcessingTools(s, run, &applied)
	if err != nil || len(tools) != 2 {
		t.Fatalf("taskInputProcessingTools()=(%d,%v)", len(tools), err)
	}

	readCtx := textpage.WithMaxBytes(ctx, 8<<10)
	firstRaw := invokeTaskProcessingTool(t, readCtx, tools, "get_task_input", getTaskInputArgs{EventID: event.ID, Limit: 5})
	var first getTaskInputReply
	if err := json.Unmarshal([]byte(firstRaw), &first); err != nil || !first.OK {
		t.Fatalf("first page=(%s,%v)", firstRaw, err)
	}
	if first.Input == nil || first.Input.EventID != event.ID || first.Page.Text != string([]rune(text)[:5]) || first.Page.Offset != 0 || first.Page.End != 5 || first.Page.TotalRunes != runeLen(text) || !first.Page.HasMore || first.Page.NextOffset == nil || *first.Page.NextOffset != 5 || first.Page.Version == "" {
		t.Fatalf("first input page=%+v", first)
	}
	secondRaw := invokeTaskProcessingTool(t, readCtx, tools, "get_task_input", getTaskInputArgs{
		EventID: event.ID, Offset: 5, Limit: 5, Version: first.Page.Version,
	})
	var second getTaskInputReply
	if err := json.Unmarshal([]byte(secondRaw), &second); err != nil || !second.OK {
		t.Fatalf("second page=(%s,%v)", secondRaw, err)
	}
	if second.Page.Offset != 5 || second.Page.Text != string([]rune(text)[5:10]) || second.Page.Version != first.Page.Version {
		t.Fatalf("second input page=%+v", second)
	}
	if len([]byte(firstRaw)) > 8<<10 || len([]byte(secondRaw)) > 8<<10 {
		t.Fatalf("task input page exceeded context tool budget: first=%d second=%d", len(firstRaw), len(secondRaw))
	}

	lateEvent := openUserEventDirect(t, s, 83, 183, "Arrived after this Run was frozen")
	if _, err := s.AddTaskInputs(ctx, lateEvent.ID, "append-late-to-active", task.ID, task.Revision, []int64{lateEvent.ID}); err != nil {
		t.Fatal(err)
	}
	deniedRaw := invokeTaskProcessingTool(t, ctx, tools, "get_task_input", getTaskInputArgs{EventID: lateEvent.ID})
	var denied getTaskInputReply
	if err := json.Unmarshal([]byte(deniedRaw), &denied); err != nil || denied.OK {
		t.Fatalf("read outside frozen run was accepted: (%+v,%v)", denied, err)
	}
	if _, err := s.ReadTaskInput(ctx, run.ID, lateEvent.ID); err == nil {
		t.Fatal("store read allowed an input outside the run snapshot")
	}

	badReport := invokeTaskProcessingTool(t, ctx, tools, "report_task_inputs", reportTaskInputsArgs{EventIDs: []int64{event.ID, lateEvent.ID}})
	var bad reportTaskInputsReply
	if err := json.Unmarshal([]byte(badReport), &bad); err != nil || bad.OK || len(applied) != 0 {
		t.Fatalf("invalid report changed staged applied IDs: (%+v,%v,%v)", bad, err, applied)
	}
	goodReport := invokeTaskProcessingTool(t, ctx, tools, "report_task_inputs", reportTaskInputsArgs{EventIDs: []int64{event.ID}})
	var good reportTaskInputsReply
	if err := json.Unmarshal([]byte(goodReport), &good); err != nil || !good.OK || good.Count != 1 || len(applied) != 1 || applied[0] != event.ID {
		t.Fatalf("valid input report=(%+v,%v), staged=%v", good, err, applied)
	}
	repeatedReport := invokeTaskProcessingTool(t, ctx, tools, "report_task_inputs", reportTaskInputsArgs{EventIDs: []int64{event.ID}})
	var repeated reportTaskInputsReply
	if err := json.Unmarshal([]byte(repeatedReport), &repeated); err != nil || !repeated.OK || repeated.Count != 1 || len(applied) != 1 {
		t.Fatalf("repeated report was not idempotent=(%+v,%v), staged=%v", repeated, err, applied)
	}
	var beforeFinish string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, event.ID).Scan(&beforeFinish); err != nil || beforeFinish != "assigned" {
		t.Fatalf("tool report persisted before stage commit=(%q,%v)", beforeFinish, err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Processed the note", AppliedInputIDs: applied}, ""); err != nil {
		t.Fatal(err)
	}
	var appliedStatus, remainingStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, ids[0]).Scan(&appliedStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, ids[1]).Scan(&remainingStatus); err != nil {
		t.Fatal(err)
	}
	if appliedStatus != "applied" || remainingStatus != "pending" {
		t.Fatalf("input statuses after partial success=(%q,%q), want applied/pending", appliedStatus, remainingStatus)
	}
	if pending, err := s.PendingTaskInputs(ctx, task.ID); err != nil || len(pending) != 2 {
		t.Fatalf("pending task inputs should include the unreported and late inputs=(%+v,%v)", pending, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := validateTaskInputsSettledTx(ctx, tx, task.ID); err == nil || !strings.Contains(err.Error(), "event_id:status") || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("completion gate error=%v, want bounded pending input IDs", err)
	}
}

func TestTaskInputProcessingFailureReleasesEveryAssignment(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	_, task, run, ids := createActiveTaskWithInputs(t, s, 90, "Original request", "https://example.com/source")
	if err := s.FinishStage(ctx, run.ID, StageResult{AppliedInputIDs: []int64{ids[0]}}, "model failed"); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		var status string
		var runID *int64
		if err := s.db.QueryRowContext(ctx, `SELECT status,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, id).Scan(&status, &runID); err != nil || status != "pending" || runID != nil {
			t.Fatalf("failed-stage input %d=(%q,%v,%v), want pending and unassigned", id, status, runID, err)
		}
	}
}

func TestTaskInputReadStorageErrorsPropagate(t *testing.T) {
	_, _, run, ids := createActiveTaskWithInputs(t, testStore(t, ":memory:", time.Hour), 100, "Read this input", "https://example.com/source")
	storeErr := errors.New("database unavailable")
	tools, err := taskInputProcessingTools(failingTaskInputReader{err: storeErr}, run, &[]int64{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(getTaskInputArgs{EventID: ids[0]})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name != "get_task_input" {
			continue
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatal("get_task_input is not invokable")
		}
		if _, err := invokable.InvokableRun(context.Background(), string(encoded)); !errors.Is(err, storeErr) {
			t.Fatalf("storage failure was converted to tool feedback: %v", err)
		}
		return
	}
	t.Fatal("get_task_input tool not found")
}

func TestTaskInputReportsMergeConcurrentCalls(t *testing.T) {
	s := testStore(t, ":memory:", time.Hour)
	_, _, run, ids := createActiveTaskWithInputs(t, s, 110, "Read both inputs", "https://example.com/source")
	var applied []int64
	tools, err := taskInputProcessingTools(s, run, &applied)
	if err != nil {
		t.Fatal(err)
	}
	var reportTool tool.InvokableTool
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "report_task_inputs" {
			reportTool, _ = candidate.(tool.InvokableTool)
			break
		}
	}
	if reportTool == nil {
		t.Fatal("report_task_inputs tool not found")
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		id := id
		args, err := json.Marshal(reportTaskInputsArgs{EventIDs: []int64{id}})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			output, err := reportTool.InvokableRun(context.Background(), string(args))
			if err != nil {
				errs <- err
				return
			}
			var reply reportTaskInputsReply
			if err := json.Unmarshal([]byte(output), &reply); err != nil {
				errs <- err
				return
			}
			if !reply.OK {
				errs <- errors.New(reply.Error)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent report failed: %v", err)
	}
	if len(applied) != len(ids) || applied[0] == applied[1] {
		t.Fatalf("concurrent report lost or duplicated IDs: %v", applied)
	}
}

type failingTaskInputReader struct{ err error }

func (r failingTaskInputReader) ReadTaskInput(context.Context, int64, int64) (Event, error) {
	return Event{}, r.err
}

func invokeTaskProcessingTool(t *testing.T, ctx context.Context, tools []tool.BaseTool, name string, args any) string {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.Name != name {
			continue
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatalf("%s is not invokable", name)
		}
		output, err := invokable.InvokableRun(ctx, string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	t.Fatalf("%s tool not found", name)
	return ""
}
