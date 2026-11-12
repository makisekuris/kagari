package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
	"kagari/internal/textpage"
)

func TestTaskInputProcessingReadsPagesAndAppliesOnlyReportedInputs(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "inputs.db")
	store := testkit.Store(t, database, time.Hour)
	text := strings.Repeat("汉🙂x", 30)
	event, task, run, ids := createRunningTaskWithInputs(t, store, 80, text, "https://example.com/source")
	var applied []int64
	tools, err := taskInputProcessingTools(store, run, &applied)
	if err != nil || len(tools) != 2 {
		t.Fatalf("taskInputProcessingTools()=(%d,%v), run input=%s", len(tools), err, run.Input)
	}

	readCtx := textpage.WithMaxBytes(ctx, 8<<10)
	firstRaw := invokeTaskProcessingTool(t, readCtx, tools, "get_task_input", getTaskInputArgs{EventID: event.ID, Limit: 5})
	var first getTaskInputReply
	if err := json.Unmarshal([]byte(firstRaw), &first); err != nil || !first.OK {
		t.Fatalf("first page=(%s,%v)", firstRaw, err)
	}
	if first.Input == nil || first.Input.EventID != event.ID || first.Page.Text != string([]rune(text)[:5]) || first.Page.Offset != 0 || first.Page.End != 5 || first.Page.TotalRunes != len([]rune(text)) || !first.Page.HasMore || first.Page.NextOffset == nil || *first.Page.NextOffset != 5 || first.Page.Version == "" {
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

	lateEvent := testkit.OpenUserEvent(t, store, 83, 183, "Arrived after this Run was frozen")
	if _, err := store.AddTaskInputs(ctx, lateEvent.ID, "append-late-to-active", task.ID, task.Revision, []int64{lateEvent.ID}); err != nil {
		t.Fatal(err)
	}
	deniedRaw := invokeTaskProcessingTool(t, ctx, tools, "get_task_input", getTaskInputArgs{EventID: lateEvent.ID})
	var denied getTaskInputReply
	if err := json.Unmarshal([]byte(deniedRaw), &denied); err != nil || denied.OK {
		t.Fatalf("read outside frozen run was accepted: (%+v,%v)", denied, err)
	}
	if _, err := store.ReadTaskInput(ctx, run.ID, lateEvent.ID); !errors.Is(err, personal.ErrTaskInputAccess) {
		t.Fatalf("store read outside the run snapshot error=%v", err)
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
	beforeFinish := readTaskInputStates(t, database, task.ID)
	if beforeFinish[event.ID].status != "assigned" || beforeFinish[event.ID].runID.Int64 != run.ID || beforeFinish[ids[1]].status != "assigned" {
		t.Fatalf("tool report persisted before stage commit: %+v", beforeFinish)
	}
	if err := store.FinishStage(ctx, run.ID, personal.StageResult{Summary: "Processed the note", AppliedInputIDs: applied}, ""); err != nil {
		t.Fatal(err)
	}
	finished := readTaskInputStates(t, database, task.ID)
	if finished[ids[0]].status != "applied" || finished[ids[1]].status != "pending" {
		t.Fatalf("input statuses after partial success=(%q,%q), want applied/pending", finished[ids[0]].status, finished[ids[1]].status)
	}
	if pending, err := store.PendingTaskInputs(ctx, task.ID); err != nil || len(pending) != 2 {
		t.Fatalf("pending inputs should include the unreported and late inputs=(%+v,%v)", pending, err)
	}
	callback, err := store.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask {
		t.Fatalf("ClaimEvent() = (%+v,%v), want task callback", callback, err)
	}
	if _, err := store.SettleTask(ctx, callback.ID, task.ID, task.Revision, personal.TaskCompleted, "Finished"); err == nil || !strings.Contains(err.Error(), "event_id:status") || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("completion gate error=%v, want pending input IDs", err)
	}
}

func TestTaskInputProcessingFailureReleasesEveryAssignment(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "failed-inputs.db")
	store := testkit.Store(t, database, time.Hour)
	_, task, run, ids := createRunningTaskWithInputs(t, store, 90, "Original request", "https://example.com/source")
	if err := store.FinishStage(ctx, run.ID, personal.StageResult{AppliedInputIDs: []int64{ids[0]}}, "model failed"); err != nil {
		t.Fatal(err)
	}
	states := readTaskInputStates(t, database, task.ID)
	for _, id := range ids {
		state := states[id]
		if state.status != "pending" || state.runID.Valid {
			t.Fatalf("failed-stage input %d=(%q,%v), want pending and unassigned", id, state.status, state.runID)
		}
	}
}

func TestTaskInputReadStorageErrorsPropagate(t *testing.T) {
	database := filepath.Join(t.TempDir(), "storage-error.db")
	store := testkit.Store(t, database, time.Hour)
	_, _, run, ids := createRunningTaskWithInputs(t, store, 100, "Read this input", "https://example.com/source")
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
	store := testkit.Store(t, ":memory:", time.Hour)
	_, _, run, ids := createRunningTaskWithInputs(t, store, 110, "Read both inputs", "https://example.com/source")
	var applied []int64
	tools, err := taskInputProcessingTools(store, run, &applied)
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

func (r failingTaskInputReader) ReadTaskInput(context.Context, int64, int64) (personal.Event, error) {
	return personal.Event{}, r.err
}

type taskInputState struct {
	status string
	runID  sql.NullInt64
}

func readTaskInputStates(t *testing.T, path string, taskID int64) map[int64]taskInputState {
	t.Helper()
	absPath, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	dsn := (&url.URL{Scheme: "file", Path: absPath, RawQuery: url.Values{
		"mode": {"ro"}, "_pragma": {"query_only(1)"},
	}.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event_id,status,run_id FROM personal_task_inputs WHERE task_id=? ORDER BY event_id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[int64]taskInputState{}
	for rows.Next() {
		var eventID int64
		var state taskInputState
		if err := rows.Scan(&eventID, &state.status, &state.runID); err != nil {
			t.Fatal(err)
		}
		states[eventID] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return states
}

func createRunningTaskWithInputs(t *testing.T, store *sqlite.Store, updateID int64, text, link string) (personal.Event, personal.Task, personal.Run, []int64) {
	t.Helper()
	ctx := context.Background()
	event := testkit.OpenUserEvent(t, store, updateID, updateID+1000, text)
	inputID := testkit.AcceptInput(t, store, strconv.FormatInt(updateID+1, 10), testkit.SourceInput(strconv.FormatInt(updateID+1001, 10), link))
	task, _ := testkit.CreateTask(t, store, event, fmt.Sprintf("task-%d", updateID), "Research", "Process the assigned input", "")
	task, err := store.AddTaskInputs(ctx, event.ID, fmt.Sprintf("attach-%d", updateID), task.ID, task.Revision, []int64{event.ID, inputID})
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, event, personal.TurnOutput{TaskID: task.ID}, nil)
	run, err := store.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatalf("ClaimRun() = (%+v,%v)", run, err)
	}
	return event, task, *run, []int64{event.ID, inputID}
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
