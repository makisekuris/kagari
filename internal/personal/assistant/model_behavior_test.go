package assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestThinkContinuesOnlyCallbackTaskAndCanBeSilent(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("get_task", map[string]any{"task_id": 7}, "get-task"),
		testkit.ResponseTool("continue_task", map[string]any{"task_id": 7, "expected_revision": 2, "step": "Read two more sources", "extend_budget": false}, "continue-task"),
		testkit.ResponseText(`{"text":"我会沿原目标继续执行。","reply_kind":"silent","attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.tasks[7] = personal.Task{ID: 7, Title: "Compare approaches", Goal: "Compare two documented approaches", Revision: 2, Status: personal.TaskAwaitingReview, BudgetMS: 1_800_000, UsedMS: 500_000}
	currentRun := personal.Run{ID: 31, TaskID: 7, Revision: 2, Kind: personal.RunTask, Status: personal.RunCompleted, Result: `{"summary":"One source found."}`}
	repo.runs[31] = currentRun
	input := personal.TurnInput{Event: personal.Event{ID: 20, Kind: personal.EventTask, TaskID: 7, RunID: 31, Text: "Run 31 found one source."},
		Tasks: []personal.Task{repo.tasks[7]}, CurrentRun: &currentRun}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "我会沿原目标继续执行。" || !output.Silent || output.TaskID != 7 {
		t.Fatalf("output=%+v", output)
	}
	if len(repo.continued) != 1 || repo.continued[0].taskID != 7 || repo.continued[0].extend || repo.continued[0].step != "Read two more sources" {
		t.Fatalf("continuation calls=%+v", repo.continued)
	}
	for _, request := range fixture.Requests {
		assertPersonalToolMode(t, request, personal.EventTask)
	}
	if inputText := testkit.RequestInput(fixture.Requests[0]); !strings.Contains(inputText, "Compare two documented approaches") || !strings.Contains(inputText, "One source found") {
		t.Fatalf("callback lacked task goal or run result: %s", inputText)
	}
}

func TestThinkSettlesItsOwnTaskAndNotifiesOnCompletion(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("settle_task", map[string]any{"task_id": 7, "expected_revision": 2, "status": personal.TaskCompleted, "summary": "The original comparison is complete."}, "settle-own-task"),
		testkit.ResponseText(`{"text":"任务已完成：比较结果已保存。","reply_kind":"silent","attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.tasks[7] = personal.Task{ID: 7, Goal: "Compare two approaches", Revision: 2, Status: personal.TaskAwaitingReview}
	currentRun := personal.Run{ID: 33, TaskID: 7, Revision: 2, Status: personal.RunCompleted, Result: `{"summary":"Both approaches compared."}`}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 201, Kind: personal.EventTask, TaskID: 7, RunID: currentRun.ID}, Tasks: []personal.Task{repo.tasks[7]}, CurrentRun: &currentRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Silent || output.TaskID != 7 || len(repo.settled) != 1 || repo.settled[0].status != personal.TaskCompleted {
		t.Fatalf("settlement output=%+v settled=%+v", output, repo.settled)
	}
	assertPersonalToolMode(t, fixture.Requests[0], personal.EventTask)
}

func TestThinkCallbackCannotMutateOtherTasksOrMemory(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_task", map[string]any{"title": "Side quest", "goal": "Do something else", "context": ""}, "create-denied"),
		testkit.ResponseTool("memory_put", map[string]any{"key": "language", "value": "English", "expected_version": 0, "evidence": "Please remember"}, "memory-denied"),
		testkit.ResponseTool("continue_task", map[string]any{"task_id": 8, "expected_revision": 1, "step": "continue unrelated", "extend_budget": false}, "other-task-denied"),
		testkit.ResponseTool("continue_task", map[string]any{"task_id": 7, "expected_revision": 1, "step": "extend from callback", "extend_budget": true}, "budget-denied"),
		testkit.ResponseText(`{"text":"我不能从任务回调中创建任务、改记忆或继续其他任务。","reply_kind":"silent","attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.tasks[7] = personal.Task{ID: 7, Goal: "Only this task", Revision: 1, Status: personal.TaskAwaitingReview, BudgetMS: 1000}
	input := personal.TurnInput{Event: personal.Event{ID: 21, Kind: personal.EventTask, TaskID: 7, Text: "callback"}, Tasks: []personal.Task{repo.tasks[7]}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.createdKeys) != 0 || len(repo.memoryChanges) != 0 || len(repo.continued) != 0 || len(repo.settled) != 0 {
		t.Fatalf("callback performed forbidden mutation: %+v", repo)
	}
	for i, want := range []string{"persisted direct user event", "persisted direct user event", "their own task", "cannot extend the budget"} {
		if !strings.Contains(testkit.RequestInput(fixture.Requests[i+1]), want) {
			t.Fatalf("tool failure %d was not returned to model: %s", i, testkit.RequestInput(fixture.Requests[i+1]))
		}
	}
	if !output.Silent || output.TaskID != 7 {
		t.Fatalf("callback output=%+v", output)
	}
}

func TestThinkDirectUserQueuesTaskAndCommitsEvidenceBackedMemory(t *testing.T) {
	evidence := "Please remember that I prefer concise answers."
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_task", map[string]any{"title": "Find a guide", "goal": "Find a guide to local cycling", "context": "Preference snapshot: concise answers"}, "create-task"),
		testkit.ResponseTool("memory_put", map[string]any{"key": "answer_style", "value": "concise", "expected_version": 0, "evidence": evidence}, "put-memory"),
		testkit.ResponseText(`{"text":"任务已排队，记忆偏好已保存。","reply_kind":"content","attachments":[]}`),
	}}
	repo := newToolRepo()
	event := personal.Event{ID: 22, Kind: personal.EventUser, Text: "Find a guide to local cycling. " + evidence}
	repo.memoryResult = personal.Memory{Key: "answer_style", Value: "concise", Version: 1, SourceEventID: event.ID}
	input := personal.TurnInput{Event: event}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if output.Silent || output.TaskID != 90 {
		t.Fatalf("direct user turn must reply and associate new task: %+v", output)
	}
	if len(repo.created) != 1 || repo.created[0].Context != "Preference snapshot: concise answers" || repo.created[0].Goal != "Find a guide to local cycling" {
		t.Fatalf("created task input=%+v", repo.created)
	}
	if len(repo.memoryChanges) != 1 || repo.memoryChanges[0].Evidence != evidence || repo.memoryChanges[0].Operation != "put" || repo.memoryChanges[0].ExpectedVersion != 0 {
		t.Fatalf("memory changes=%+v", repo.memoryChanges)
	}
	if len(repo.createdKeys) != 1 || len(repo.memoryKeys) != 1 || repo.createdKeys[0] == repo.memoryKeys[0] {
		t.Fatalf("request keys missing or not operation-specific: created=%v memory=%v", repo.createdKeys, repo.memoryKeys)
	}
	if memory := repo.memoryResult; memory.Version != 1 || memory.Value != "concise" || memory.SourceEventID != event.ID {
		t.Fatalf("memory tool result missing: %+v", memory)
	}
	toolResults := testkit.RequestInput(fixture.Requests[2])
	if !strings.Contains(toolResults, `\"memory\":{\"key\":\"answer_style\"`) {
		t.Fatalf("memory tool did not return its committed result: %s", toolResults)
	}
	for _, request := range fixture.Requests {
		assertPersonalToolMode(t, request, personal.EventUser)
	}
}

func TestThinkMemoryPutRejectsStaleVersionAndUnsupportedEvidence(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("memory_put", map[string]any{"key": "answer_style", "value": "detailed", "expected_version": 2, "evidence": "I now prefer detailed answers."}, "stale-memory"),
		testkit.ResponseTool("memory_put", map[string]any{"key": "answer_style", "value": "detailed", "expected_version": 3, "evidence": "I now prefer short answers."}, "unsupported-evidence"),
		testkit.ResponseText(`{"text":"我无法验证这次记忆变更。","reply_kind":"content","attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.memoryErr = errors.New("expected_version does not match the current memory")
	event := personal.Event{ID: 221, Kind: personal.EventUser, Text: "Please remember that I now prefer detailed answers."}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	if output.Silent || len(repo.memoryChanges) != 1 {
		t.Fatalf("stale or unsupported memory write persisted: output=%+v changes=%+v", output, repo.memoryChanges)
	}
	if !strings.Contains(testkit.RequestInput(fixture.Requests[1]), "expected_version does not match") || !strings.Contains(testkit.RequestInput(fixture.Requests[2]), "exact current-message evidence quote") {
		t.Fatalf("memory write errors were not returned to model: %s", testkit.RequestInput(fixture.Requests[2]))
	}
}

func TestThinkMemoryToolFailureIsReturnedAndLoggedWithoutEvidence(t *testing.T) {
	const value = "PRIVATE_MEMORY_VALUE"
	const evidence = "PRIVATE_EVIDENCE_QUOTE"
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("memory_put", memoryPutArgs{Key: "answer_style", Value: value, Evidence: evidence}, "failed-put"),
		testkit.ResponseText(`{"text":"暂时无法保存。","reply_kind":"content","attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.memoryErr = errors.New("store unavailable")
	core, logs := observer.New(zapcore.WarnLevel)
	main := newMainAgent(t, fixture, repo)
	main.Log = zap.New(core)
	_, err := main.Think(context.Background(), personal.TurnInput{Event: personal.Event{ID: 232, Kind: personal.EventUser, Text: evidence}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(testkit.RequestInput(fixture.Requests[1]), `store unavailable`) {
		t.Fatal("tool failure was not returned to the model")
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("memory write failure log count=%d", len(entries))
	}
	fields := entries[0].ContextMap()
	if len(fields) != 4 || fields["event_id"] != int64(232) || fields["key"] != "answer_style" || fields["operation"] != "put" || fields["error"] != "store unavailable" {
		t.Fatalf("unexpected memory failure log fields: %v", fields)
	}
	if strings.Contains(fmt.Sprint(entries), value) || strings.Contains(fmt.Sprint(entries), evidence) {
		t.Fatal("memory value or evidence appeared in failure logs")
	}
}

func TestThinkReadsRunsAndReturnsMemoryTombstone(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("list_runs", map[string]any{"task_id": 7}, "list-runs"),
		testkit.ResponseTool("memory_get", map[string]any{"key": "answer_style"}, "get-memory"),
		testkit.ResponseTool("memory_history", map[string]any{"key": "answer_style"}, "memory-history"),
		testkit.ResponseText(`{"text":"当前记忆已删除，历史记录可供审计。","reply_kind":"content","attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.tasks[7] = personal.Task{ID: 7, Goal: "Old goal", Revision: 1}
	repo.runs[81] = personal.Run{ID: 81, TaskID: 7, Revision: 1, Kind: personal.RunTask, Status: personal.RunCompleted}
	repo.memories["answer_style"] = personal.Memory{Key: "answer_style", Value: "old forgotten value", Version: 4, Deleted: true}
	input := personal.TurnInput{Event: personal.Event{ID: 23, Kind: personal.EventUser, Text: "Show my memory history."}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if output.Silent || len(repo.listRunIDs) != 1 || repo.listRunIDs[0] != 7 || len(repo.historyKeys) != 1 || repo.historyKeys[0] != "answer_style" {
		t.Fatalf("read tools or reply missing: output=%+v repo=%+v", output, repo)
	}
	if !strings.Contains(testkit.RequestInput(fixture.Requests[2]), `\"version\":4`) || !strings.Contains(testkit.RequestInput(fixture.Requests[2]), `\"deleted\":true`) || strings.Contains(testkit.RequestInput(fixture.Requests[2]), "old forgotten value") {
		t.Fatalf("memory_get did not return a value-free tombstone: %s", testkit.RequestInput(fixture.Requests[2]))
	}
}

func TestThinkNotificationRulesAndStrictFailures(t *testing.T) {
	tests := []struct {
		name     string
		response testkit.Response
		wantErr  string
	}{
		{name: "invalid json", response: testkit.ResponseText(`{"text":"hello","reply_kind":"content","attachments":[],"extra":1}`), wantErr: "unexpected field"},
		{name: "blank text", response: testkit.ResponseText(`{"text":"  ","reply_kind":"content","attachments":[]}`), wantErr: "blank"},
		{name: "refusal", response: testkit.ResponseRefusal(), wantErr: "refused"},
		{name: "incomplete", response: testkit.Response{Status: "incomplete", Output: testkit.ResponseText(`{"text":"not final","reply_kind":"content","attachments":[]}`).Output}, wantErr: "incomplete"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{tc.response}}
			if tc.wantErr == "unexpected field" || tc.wantErr == "blank" {
				fixture.Responses = append(fixture.Responses, tc.response)
			}
			repo := newToolRepo()
			_, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{Event: personal.Event{ID: 24, Kind: personal.EventUser, Text: "Hi"}})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error=%v, want %q", err, tc.wantErr)
			}
		})
	}

	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{testkit.ResponseText(`{"text":"Continued quietly.","reply_kind":"silent","attachments":[]}`)}}
	repo := newToolRepo()
	repo.tasks[7] = personal.Task{ID: 7, Revision: 1, Goal: "Goal", Status: personal.TaskAwaitingReview, BudgetMS: 1000}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{Event: personal.Event{ID: 25, Kind: personal.EventTask, TaskID: 7}, Tasks: []personal.Task{repo.tasks[7]}})
	if err != nil || !output.Silent {
		t.Fatalf("ordinary continuation should be silent, output=%+v err=%v", output, err)
	}
	fixture = &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{testkit.ResponseText(`{"text":"The task completed.","reply_kind":"silent","attachments":[]}`)}}
	output, err = newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{Event: personal.Event{ID: 26, Kind: personal.EventTask, TaskID: 7}, Tasks: []personal.Task{{ID: 7, Status: personal.TaskCompleted}}})
	if err != nil || output.Silent {
		t.Fatalf("task completion must notify, output=%+v err=%v", output, err)
	}
}

func TestRequestKeyIsStableForRepeatedToolInput(t *testing.T) {
	tools := personalTools{event: personal.Event{ID: 99}, taskIDs: make(map[int64]struct{})}
	input := createTaskArgs{Title: "Title", Goal: "Goal", Context: "Context"}
	first, err := tools.requestKey("create_task", input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tools.requestKey("create_task", input)
	if err != nil || first != second {
		t.Fatalf("repeated request key=(%q,%q,%v)", first, second, err)
	}
}

func TestTaskOutputAssociationRejectsMultipleTaskIDs(t *testing.T) {
	tools := personalTools{event: personal.Event{TaskID: 7}, taskIDs: map[int64]struct{}{7: {}, 8: {}}}
	if got := tools.taskID(); got != 0 {
		t.Fatalf("task ID=%d, want 0 for multiple tasks", got)
	}
	tools.taskIDs = map[int64]struct{}{7: {}}
	if got := tools.taskID(); got != 7 {
		t.Fatalf("task ID=%d, want explicit event task 7", got)
	}
}
