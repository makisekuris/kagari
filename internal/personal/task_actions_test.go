package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
)

func invokeNamedTool(t *testing.T, tools []tool.BaseTool, name string, args any) string {
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
		if info.Name != name {
			continue
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatalf("%s is not invokable", name)
		}
		output, err := invokable.InvokableRun(context.Background(), string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	t.Fatalf("%s tool not found", name)
	return ""
}

func invokeActionTool(t *testing.T, tools []tool.BaseTool, name string, args any) personalToolReply {
	t.Helper()
	raw := invokeNamedTool(t, tools, name, args)
	var reply personalToolReply
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatalf("decode %s reply %q: %v", name, raw, err)
	}
	return reply
}

func newTaskActionTools(t *testing.T, store Repository, event Event, input TurnInput) (*personalTools, []tool.BaseTool) {
	t.Helper()
	p := &personalTools{store: store, event: event, input: input, taskIDs: make(map[int64]struct{})}
	tools, err := p.build()
	if err != nil {
		t.Fatal(err)
	}
	return p, tools
}

func TestTaskActionIDSchemaExplainsRetryContractForEveryCaller(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, store, 900, 1900, "Review this task")
	_, tools := newTaskActionTools(t, store, event, TurnInput{})
	wantNames := map[string]bool{"create_task": true, "update_task": true, "continue_task": true, "add_task_input": true}
	wantDescription := ""
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !wantNames[info.Name] {
			continue
		}
		delete(wantNames, info.Name)
		schema, err := info.ToJSONSchema()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		description := decoded.Properties["action_id"].Description
		if !strings.Contains(description, "Omit for a new operation") || !strings.Contains(description, "reuse the returned action_id") ||
			!strings.Contains(description, "Do not omit or change it") || !strings.Contains(description, "does not resolve the failure") {
			t.Errorf("%s action_id description=%q", info.Name, description)
		}
		if wantDescription == "" {
			wantDescription = description
		} else if description != wantDescription {
			t.Errorf("%s action_id description differs from peers: %q; want %q", info.Name, description, wantDescription)
		}
	}
	if len(wantNames) != 0 {
		t.Fatalf("missing action tools: %v", wantNames)
	}
}

type taskActionFaultRepository struct {
	Repository
	createTaskError    error
	addTaskInputsError error
}

func (r *taskActionFaultRepository) CreateTask(ctx context.Context, eventID int64, key, title string, input TaskInput) (Task, Run, error) {
	if r.createTaskError != nil {
		err := r.createTaskError
		r.createTaskError = nil
		return Task{}, Run{}, err
	}
	return r.Repository.CreateTask(ctx, eventID, key, title, input)
}

func (r *taskActionFaultRepository) AddTaskInputs(ctx context.Context, eventID int64, key string, taskID, expectedRevision int64, inputIDs []int64) (Task, error) {
	if r.addTaskInputsError != nil {
		err := r.addTaskInputsError
		r.addTaskInputsError = nil
		return Task{}, err
	}
	return r.Repository.AddTaskInputs(ctx, eventID, key, taskID, expectedRevision, inputIDs)
}

func TestTaskActionsKeepAnEarlierFailureAfterLaterSuccess(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, store, 901, 1901, "Please organize these tasks")
	target, _ := createTask(t, store, event, "task-action-target", "Existing report", "Summarize the report", "")
	p, tools := newTaskActionTools(t, store, event, TurnInput{Tasks: []Task{target}})

	createdA := invokeActionTool(t, tools, "create_task", createTaskArgs{Title: "A", Goal: "First task"})
	failedB := invokeActionTool(t, tools, "update_task", reviseTaskArgs{TaskID: target.ID, ExpectedRevision: target.Revision, Goal: " "})
	createdC := invokeActionTool(t, tools, "create_task", createTaskArgs{Title: "C", Goal: "Third task"})
	if !createdA.OK || !createdC.OK || failedB.OK || createdA.ActionID != "task-action-1" || failedB.ActionID != "task-action-2" || createdC.ActionID != "task-action-3" {
		t.Fatalf("action replies A/B/C=(%+v,%+v,%+v)", createdA, failedB, createdC)
	}
	failures := p.unresolvedTaskActions()
	if len(failures) != 1 || failures[0].ActionID != failedB.ActionID || failures[0].Operation != "update_task" || failures[0].TaskID != target.ID || failures[0].Target != target.Title || failures[0].Error == "" {
		t.Fatalf("unresolved action failures=%+v", failures)
	}
	if p.mustNotify {
		t.Fatal("task action tracking must not force notification")
	}
}

func TestTaskActionRetryWithSameIDCanCorrectArguments(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, store, 902, 1902, "Create a task")
	p, tools := newTaskActionTools(t, store, event, TurnInput{})

	failed := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: "create-report", Title: "Report", Goal: " "})
	if failed.OK || failed.ActionID != "create-report" || len(p.unresolvedTaskActions()) != 1 ||
		!strings.Contains(failed.Error, `same action_id "create-report"`) || !strings.Contains(failed.Error, "different ID is a separate action") {
		t.Fatalf("initial reply=%+v; failures=%+v", failed, p.unresolvedTaskActions())
	}
	succeeded := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: failed.ActionID, Title: "Report", Goal: "Summarize the report"})
	if !succeeded.OK || succeeded.ActionID != failed.ActionID || len(p.unresolvedTaskActions()) != 0 {
		t.Fatalf("corrected reply=%+v; failures=%+v", succeeded, p.unresolvedTaskActions())
	}
}

func TestTaskActionIDCannotClearAnotherFailureOrChangeTask(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, store, 903, 1903, "Update a task")
	firstTask, _ := createTask(t, store, event, "task-action-first", "First target", "First goal", "")
	secondTask, _ := createTask(t, store, event, "task-action-second", "Second target", "Second goal", "")
	p, tools := newTaskActionTools(t, store, event, TurnInput{Tasks: []Task{firstTask, secondTask}})

	failed := invokeActionTool(t, tools, "update_task", reviseTaskArgs{ActionID: "fix-first", TaskID: firstTask.ID, ExpectedRevision: firstTask.Revision, Goal: ""})
	otherIDSuccess := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: "different-action", Title: "Unrelated", Goal: "Unrelated goal"})
	wrongTask := invokeActionTool(t, tools, "update_task", reviseTaskArgs{ActionID: failed.ActionID, TaskID: secondTask.ID, ExpectedRevision: secondTask.Revision, Goal: "Changed second target"})
	failures := p.unresolvedTaskActions()
	if failed.OK || !otherIDSuccess.OK || wrongTask.OK || wrongTask.ActionID == failed.ActionID || !strings.Contains(wrongTask.Error, "another task") {
		t.Fatalf("replies failed/other/wrong=(%+v,%+v,%+v)", failed, otherIDSuccess, wrongTask)
	}
	if len(failures) != 2 || failures[0].ActionID != failed.ActionID || failures[0].TaskID != firstTask.ID || failures[0].Target != firstTask.Title || failures[1].ActionID != wrongTask.ActionID || failures[1].TaskID != secondTask.ID || failures[1].Target != secondTask.Title {
		t.Fatalf("unresolved failures after unrelated success or wrong task=%+v", failures)
	}
	recovered := invokeActionTool(t, tools, "update_task", reviseTaskArgs{ActionID: wrongTask.ActionID, TaskID: secondTask.ID, ExpectedRevision: secondTask.Revision, Goal: "Changed second target"})
	failures = p.unresolvedTaskActions()
	if !recovered.OK || len(failures) != 1 || failures[0].ActionID != failed.ActionID {
		t.Fatalf("recovered different-task action=%+v; unresolved failures=%+v", recovered, failures)
	}
}

func TestTaskActionIDCannotBeReusedForDifferentSuccessfulPayload(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, store, 904, 1904, "Create a task")
	p, tools := newTaskActionTools(t, store, event, TurnInput{})
	first := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: "one-task", Title: "First", Goal: "First goal"})
	changed := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: first.ActionID, Title: "Changed", Goal: "Different goal"})
	failures := p.unresolvedTaskActions()
	if !first.OK || changed.OK || changed.ActionID == first.ActionID || !strings.Contains(changed.Error, "already succeeded") || len(failures) != 1 || failures[0].ActionID != changed.ActionID {
		t.Fatalf("replies first/changed=(%+v,%+v); failures=%+v", first, changed, p.unresolvedTaskActions())
	}
	recovered := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: changed.ActionID, Title: "Changed", Goal: "Different goal"})
	if !recovered.OK || len(p.unresolvedTaskActions()) != 0 {
		t.Fatalf("recovered action=%+v; failures=%+v", recovered, p.unresolvedTaskActions())
	}
}

func TestTaskActionIDDoesNotChangeStoreIdempotencyKey(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, store, 905, 1905, "Create the same task")
	_, tools := newTaskActionTools(t, store, event, TurnInput{})
	first := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: "first-call", Title: "Repeated", Goal: "Same payload"})
	second := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: "second-call", Title: "Repeated", Goal: "Same payload"})
	if !first.OK || !second.OK || first.Task == nil || second.Task == nil || first.Task.ID != second.Task.ID || first.Run == nil || second.Run == nil || first.Run.ID != second.Run.ID {
		t.Fatalf("repeated payload replies=(%+v,%+v)", first, second)
	}
}

func TestTaskActionSuccessfulSamePayloadWithDifferentIDClearsFailure(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, store, 908, 1908, "Create a task")
	faultStore := &taskActionFaultRepository{Repository: store, createTaskError: errors.New("temporary storage error")}
	p, tools := newTaskActionTools(t, faultStore, event, TurnInput{})

	failed := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: "first-attempt", Title: "Report", Goal: "Summarize the report"})
	if failed.OK || len(p.unresolvedTaskActions()) != 1 {
		t.Fatalf("initial reply=%+v; failures=%+v", failed, p.unresolvedTaskActions())
	}
	succeeded := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: "second-attempt", Title: "Report", Goal: "Summarize the report"})
	if !succeeded.OK || len(p.unresolvedTaskActions()) != 0 {
		t.Fatalf("recovered reply=%+v; failures=%+v", succeeded, p.unresolvedTaskActions())
	}
}

func TestTaskActionCorrectedRequiredFieldsWithNewIDClearFailures(t *testing.T) {
	t.Run("create fills goal", func(t *testing.T) {
		store := testStore(t, ":memory:", time.Hour)
		event := openUserEvent(t, store, 916, 1916, "Create report B")
		p, tools := newTaskActionTools(t, store, event, TurnInput{})

		failed := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: "create-empty-goal", Title: "Report B", Context: "Quarterly report", Goal: " "})
		succeeded := invokeActionTool(t, tools, "create_task", createTaskArgs{ActionID: "create-goal", Title: "Report B", Context: "Quarterly report", Goal: "Summarize report B"})
		if failed.OK || !succeeded.OK || len(p.unresolvedTaskActions()) != 0 {
			t.Fatalf("failed/success replies=(%+v,%+v); failures=%+v", failed, succeeded, p.unresolvedTaskActions())
		}
	})

	t.Run("update fills goal and fixes revision", func(t *testing.T) {
		store := testStore(t, ":memory:", time.Hour)
		event := openUserEvent(t, store, 917, 1917, "Update this report")
		task, _ := createTask(t, store, event, "task-action-update-correction", "Update target", "Original goal", "")
		p, tools := newTaskActionTools(t, store, event, TurnInput{Tasks: []Task{task}})

		failed := invokeActionTool(t, tools, "update_task", reviseTaskArgs{ActionID: "update-empty-goal", TaskID: task.ID, ExpectedRevision: task.Revision + 1, Goal: "", Context: "Keep this context"})
		succeeded := invokeActionTool(t, tools, "update_task", reviseTaskArgs{ActionID: "update-goal", TaskID: task.ID, ExpectedRevision: task.Revision, Goal: "Updated goal", Context: "Keep this context"})
		if failed.OK || !succeeded.OK || len(p.unresolvedTaskActions()) != 0 {
			t.Fatalf("failed/success replies=(%+v,%+v); failures=%+v", failed, succeeded, p.unresolvedTaskActions())
		}
	})

	t.Run("continue fills step", func(t *testing.T) {
		store := testStore(t, ":memory:", time.Hour)
		event := openUserEvent(t, store, 918, 1918, "Continue the report")
		task, _ := createTask(t, store, event, "task-action-continue-step", "Continue target", "Original goal", "")
		if _, err := store.db.ExecContext(context.Background(), `UPDATE personal_tasks SET status=? WHERE id=?`, TaskWaitingInput, task.ID); err != nil {
			t.Fatal(err)
		}
		p, tools := newTaskActionTools(t, store, event, TurnInput{Tasks: []Task{task}})

		failed := invokeActionTool(t, tools, "continue_task", continueTaskArgs{ActionID: "continue-empty-step", TaskID: task.ID, ExpectedRevision: task.Revision, Step: "", ExtendBudget: false})
		succeeded := invokeActionTool(t, tools, "continue_task", continueTaskArgs{ActionID: "continue-step", TaskID: task.ID, ExpectedRevision: task.Revision, Step: "Continue the report", ExtendBudget: false})
		if failed.OK || !succeeded.OK || len(p.unresolvedTaskActions()) != 0 {
			t.Fatalf("failed/success replies=(%+v,%+v); failures=%+v", failed, succeeded, p.unresolvedTaskActions())
		}
	})

	t.Run("continue fixes revision", func(t *testing.T) {
		store := testStore(t, ":memory:", time.Hour)
		event := openUserEvent(t, store, 919, 1919, "Continue the report")
		task, _ := createTask(t, store, event, "task-action-continue-revision", "Continue target", "Original goal", "")
		if _, err := store.db.ExecContext(context.Background(), `UPDATE personal_tasks SET status=? WHERE id=?`, TaskWaitingInput, task.ID); err != nil {
			t.Fatal(err)
		}
		p, tools := newTaskActionTools(t, store, event, TurnInput{Tasks: []Task{task}})

		failed := invokeActionTool(t, tools, "continue_task", continueTaskArgs{ActionID: "continue-wrong-revision", TaskID: task.ID, ExpectedRevision: task.Revision + 1, Step: "Continue the report", ExtendBudget: false})
		succeeded := invokeActionTool(t, tools, "continue_task", continueTaskArgs{ActionID: "continue-current-revision", TaskID: task.ID, ExpectedRevision: task.Revision, Step: "Continue the report", ExtendBudget: false})
		if failed.OK || !succeeded.OK || len(p.unresolvedTaskActions()) != 0 {
			t.Fatalf("failed/success replies=(%+v,%+v); failures=%+v", failed, succeeded, p.unresolvedTaskActions())
		}
	})
}

func TestTaskActionCorrectionDoesNotClearDifferentIntent(t *testing.T) {
	tests := []struct {
		name          string
		operation     string
		failedTaskID  int64
		successTaskID int64
		failedArgs    any
		successArgs   any
	}{
		{
			name:        "create changed nonempty goal",
			operation:   "create_task",
			failedArgs:  createTaskArgs{Title: "Report B", Goal: "Summarize results", Context: "Quarterly"},
			successArgs: createTaskArgs{Title: "Report B", Goal: "Compare alternatives", Context: "Quarterly"},
		},
		{
			name:        "create changed context",
			operation:   "create_task",
			failedArgs:  createTaskArgs{Title: "Report B", Goal: "", Context: "Quarterly"},
			successArgs: createTaskArgs{Title: "Report B", Goal: "Summarize results", Context: "Annual"},
		},
		{
			name:        "create without an intent anchor",
			operation:   "create_task",
			failedArgs:  createTaskArgs{Title: "", Goal: "", Context: ""},
			successArgs: createTaskArgs{Title: "New task", Goal: "Research something", Context: ""},
		},
		{
			name:          "update changed nonempty goal",
			operation:     "update_task",
			failedTaskID:  7,
			successTaskID: 7,
			failedArgs:    reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "First goal", Context: "Same context"},
			successArgs:   reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "Different goal", Context: "Same context"},
		},
		{
			name:          "update changed context",
			operation:     "update_task",
			failedTaskID:  7,
			successTaskID: 7,
			failedArgs:    reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "", Context: "First context"},
			successArgs:   reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "Filled goal", Context: "Different context"},
		},
		{
			name:          "update changed attachments",
			operation:     "update_task",
			failedTaskID:  7,
			successTaskID: 7,
			failedArgs:    reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "", Attachments: []string{"attachment-a"}},
			successArgs:   reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "Filled goal", Attachments: []string{"attachment-b"}},
		},
		{
			name:          "update explicit clear differs from preserve",
			operation:     "update_task",
			failedTaskID:  7,
			successTaskID: 7,
			failedArgs:    reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "", Attachments: []string{}},
			successArgs:   reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "Filled goal", Attachments: nil},
		},
		{
			name:          "update preserve differs from explicit clear",
			operation:     "update_task",
			failedTaskID:  7,
			successTaskID: 7,
			failedArgs:    reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "", Attachments: nil},
			successArgs:   reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "Filled goal", Attachments: []string{}},
		},
		{
			name:          "update changed source runs",
			operation:     "update_task",
			failedTaskID:  7,
			successTaskID: 7,
			failedArgs:    reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "", SourceRunIDs: []int64{10}},
			successArgs:   reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "Filled goal", SourceRunIDs: []int64{11}},
		},
		{
			name:          "update different task",
			operation:     "update_task",
			failedTaskID:  7,
			successTaskID: 8,
			failedArgs:    reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: ""},
			successArgs:   reviseTaskArgs{TaskID: 8, ExpectedRevision: 1, Goal: "Filled goal"},
		},
		{
			name:          "continue changed budget authorization",
			operation:     "continue_task",
			failedTaskID:  7,
			successTaskID: 7,
			failedArgs:    continueTaskArgs{TaskID: 7, ExpectedRevision: 1, Step: "", ExtendBudget: false},
			successArgs:   continueTaskArgs{TaskID: 7, ExpectedRevision: 1, Step: "Continue", ExtendBudget: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := &personalTools{event: Event{ID: 1}}
			failed, err := p.beginTaskAction(test.operation, "failed", test.failedTaskID, "target", test.failedArgs)
			if err != nil {
				t.Fatal(err)
			}
			p.recordTaskActionFailure(failed, errors.New("failed"))
			succeeded, err := p.beginTaskAction(test.operation, "succeeded", test.successTaskID, "target", test.successArgs)
			if err != nil {
				t.Fatal(err)
			}
			p.succeedTaskAction(succeeded)
			failures := p.unresolvedTaskActions()
			if len(failures) != 1 || failures[0].ActionID != failed.actionID {
				t.Fatalf("unresolved failures=%+v", failures)
			}
		})
	}
}

func TestSuccessfulUpdateActionCannotBeReusedForNilOrEmptyAttachments(t *testing.T) {
	for _, test := range []struct {
		name   string
		first  []string
		second []string
	}{
		{name: "preserve then clear", first: nil, second: []string{}},
		{name: "clear then preserve", first: []string{}, second: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &personalTools{event: Event{ID: 1}}
			first, err := p.beginTaskAction("update_task", "done", 7, "target", reviseTaskArgs{
				TaskID: 7, ExpectedRevision: 1, Goal: "Goal", Attachments: test.first,
			})
			if err != nil {
				t.Fatal(err)
			}
			p.succeedTaskAction(first)
			second, err := p.beginTaskAction("update_task", "done", 7, "target", reviseTaskArgs{
				TaskID: 7, ExpectedRevision: 1, Goal: "Goal", Attachments: test.second,
			})
			if err == nil || second.actionID == first.actionID {
				t.Fatalf("reused action=(%+v,%v), want a new rejected action", second, err)
			}
			failures := p.unresolvedTaskActions()
			if len(failures) != 1 || failures[0].ActionID != second.actionID {
				t.Fatalf("unresolved failures=%+v", failures)
			}
		})
	}
}

func TestUpdateActionKeepsFailureWhenReferenceClearingChanges(t *testing.T) {
	for _, field := range []string{"source_run_ids", "memory_refs"} {
		for _, clearFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/clear-first=%v", field, clearFirst), func(t *testing.T) {
				preserve := reviseTaskArgs{TaskID: 7, ExpectedRevision: 1, Goal: "Goal"}
				clear := preserve
				if field == "source_run_ids" {
					clear.SourceRunIDs = []int64{}
				} else {
					clear.MemoryRefs = []string{}
				}
				firstArgs, secondArgs := preserve, clear
				if clearFirst {
					firstArgs, secondArgs = clear, preserve
				}
				p := &personalTools{event: Event{ID: 1}}
				first, err := p.beginTaskAction("update_task", "failed", 7, "target", firstArgs)
				if err != nil {
					t.Fatal(err)
				}
				p.recordTaskActionFailure(first, errors.New("failed"))
				second, err := p.beginTaskAction("update_task", "succeeded", 7, "target", secondArgs)
				if err != nil {
					t.Fatal(err)
				}
				p.succeedTaskAction(second)
				if failures := p.unresolvedTaskActions(); len(failures) != 1 || failures[0].ActionID != first.actionID {
					t.Fatalf("different reference action cleared failure: %+v", failures)
				}
				if _, err := p.beginTaskAction("update_task", second.actionID, 7, "target", firstArgs); err == nil {
					t.Fatal("successful action ID accepted changed reference clearing")
				}
			})
		}
	}
}

func TestAddTaskInputActionCanRetryWithCurrentEventID(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, store, 906, 1906, "Add this message to the task")
	task, _ := createTask(t, store, event, "task-action-input-target", "Source review", "Review source material", "")
	p, tools := newTaskActionTools(t, store, event, TurnInput{Tasks: []Task{task}})

	failed, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{ActionID: "add-current", TaskID: task.ID, ExpectedRevision: task.Revision})
	if failed.OK || failed.ActionID != "add-current" || len(p.unresolvedTaskActions()) != 1 || !strings.Contains(failed.Error, `same action_id "add-current"`) {
		t.Fatalf("initial add input reply=%+v; failures=%+v", failed, p.unresolvedTaskActions())
	}
	succeeded, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{
		ActionID: failed.ActionID, TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID},
	})
	if !succeeded.OK || succeeded.ActionID != failed.ActionID || len(p.unresolvedTaskActions()) != 0 {
		t.Fatalf("corrected add input reply=%+v; failures=%+v", succeeded, p.unresolvedTaskActions())
	}
}

func TestAddTaskInputCoverageClearsEarlierFailuresForSameTask(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	event := openUserEvent(t, store, 909, 1909, "Add the selected source")
	pendingID := acceptInput(t, store, "910", sourceInput("1910", "https://example.com/selected"))
	task, _ := createTask(t, store, event, "task-action-input-coverage", "Source review", "Review source material", "")
	pending, err := store.PendingInputs(context.Background(), event.ID)
	if err != nil {
		t.Fatal(err)
	}
	faultStore := &taskActionFaultRepository{Repository: store, addTaskInputsError: errors.New("temporary storage error")}
	p, tools := newTaskActionTools(t, faultStore, event, TurnInput{Tasks: []Task{task}, PendingInputs: pending})

	missingCurrent := invokeNamedTool(t, tools, "add_task_input", addTaskInputArgs{TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{pendingID}})
	var firstFailure addTaskInputReply
	if err := json.Unmarshal([]byte(missingCurrent), &firstFailure); err != nil || firstFailure.OK {
		t.Fatalf("missing-current reply=(%s,%v)", missingCurrent, err)
	}
	storageFailure, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, pendingID}})
	if storageFailure.OK || storageFailure.ActionID == "" || len(p.unresolvedTaskActions()) != 2 {
		t.Fatalf("temporary storage reply=%+v; failures=%+v", storageFailure, p.unresolvedTaskActions())
	}
	recovered, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{ActionID: storageFailure.ActionID, TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, pendingID}})
	if !recovered.OK || len(p.unresolvedTaskActions()) != 0 {
		t.Fatalf("recovered add input reply=%+v; failures=%+v", recovered, p.unresolvedTaskActions())
	}
}

func TestAddTaskInputCoverageKeepsUnselectedInputsAndOtherTasks(t *testing.T) {
	t.Run("unselected input remains unresolved", func(t *testing.T) {
		store := testStore(t, ":memory:", time.Hour)
		event := openUserEvent(t, store, 911, 1911, "Add one selected source")
		inputTwo := acceptInput(t, store, "912", sourceInput("1912", "https://example.com/two"))
		inputThree := acceptInput(t, store, "913", sourceInput("1913", "https://example.com/three"))
		task, _ := createTask(t, store, event, "task-action-unselected", "Source review", "Review source material", "")
		pending, err := store.PendingInputs(context.Background(), event.ID)
		if err != nil {
			t.Fatal(err)
		}
		faultStore := &taskActionFaultRepository{Repository: store, addTaskInputsError: errors.New("temporary storage error")}
		p, tools := newTaskActionTools(t, faultStore, event, TurnInput{Tasks: []Task{task}, PendingInputs: pending})

		failed, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{ActionID: "needs-input-three", TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, inputThree}})
		succeeded, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{ActionID: "adds-input-two", TaskID: task.ID, ExpectedRevision: task.Revision, InputEventIDs: []int64{event.ID, inputTwo}})
		failures := p.unresolvedTaskActions()
		if failed.OK || !succeeded.OK || len(failures) != 1 || failures[0].ActionID != failed.ActionID ||
			!strings.Contains(p.requiredTaskReply(), "未能补充材料") {
			t.Fatalf("failed/success replies=(%+v,%+v); failures=%+v", failed, succeeded, failures)
		}
	})

	t.Run("other task does not clear failure", func(t *testing.T) {
		store := testStore(t, ":memory:", time.Hour)
		event := openUserEvent(t, store, 914, 1914, "Add one source to a task")
		inputID := acceptInput(t, store, "915", sourceInput("1915", "https://example.com/source"))
		failedTask, _ := createTask(t, store, event, "task-action-other-failed", "Failed target", "Review the source", "")
		successTask, _ := createTask(t, store, event, "task-action-other-success", "Successful target", "Review the source", "")
		pending, err := store.PendingInputs(context.Background(), event.ID)
		if err != nil {
			t.Fatal(err)
		}
		faultStore := &taskActionFaultRepository{Repository: store, addTaskInputsError: errors.New("temporary storage error")}
		p, tools := newTaskActionTools(t, faultStore, event, TurnInput{Tasks: []Task{failedTask, successTask}, PendingInputs: pending})

		failed, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{ActionID: "failed-task-two", TaskID: failedTask.ID, ExpectedRevision: failedTask.Revision, InputEventIDs: []int64{event.ID, inputID}})
		succeeded, _ := invokeTaskInputTool(t, tools, addTaskInputArgs{ActionID: "success-task-one", TaskID: successTask.ID, ExpectedRevision: successTask.Revision, InputEventIDs: []int64{event.ID, inputID}})
		failures := p.unresolvedTaskActions()
		if failed.OK || !succeeded.OK || len(failures) != 1 || failures[0].ActionID != failed.ActionID || failures[0].TaskID != failedTask.ID {
			t.Fatalf("failed/success replies=(%+v,%+v); failures=%+v", failed, succeeded, failures)
		}
	})
}

func TestContinueFailuresTrackOnlyDirectUserActions(t *testing.T) {
	store := testStore(t, ":memory:", time.Hour)
	userEvent := openUserEvent(t, store, 907, 1907, "Continue the task")
	task, _ := createTask(t, store, userEvent, "task-action-continue-target", "Continue target", "Continue the goal", "")
	p, tools := newTaskActionTools(t, store, userEvent, TurnInput{Tasks: []Task{task}})
	directFailure := invokeActionTool(t, tools, "continue_task", continueTaskArgs{TaskID: task.ID, ExpectedRevision: task.Revision})
	if directFailure.OK || directFailure.ActionID == "" || len(p.unresolvedTaskActions()) != 1 || p.unresolvedTaskActions()[0].Operation != "continue_task" {
		t.Fatalf("direct continue reply=%+v; failures=%+v", directFailure, p.unresolvedTaskActions())
	}

	callback := Event{ID: 1908, Kind: EventTask, TaskID: task.ID}
	callbackToolsOwner, callbackTools := newTaskActionTools(t, store, callback, TurnInput{Tasks: []Task{task}})
	settlement := invokeActionTool(t, callbackTools, "settle_task", settleTaskArgs{TaskID: task.ID + 1, ExpectedRevision: task.Revision, Status: TaskCompleted, Summary: "Done"})
	if settlement.OK || !strings.Contains(settlement.Error, "their own task") || len(callbackToolsOwner.unresolvedTaskActions()) != 0 || callbackToolsOwner.mustNotify {
		t.Fatalf("cross-task settlement denial reply=%+v; failures=%+v mustNotify=%t", settlement, callbackToolsOwner.unresolvedTaskActions(), callbackToolsOwner.mustNotify)
	}
	denied := invokeActionTool(t, callbackTools, "continue_task", continueTaskArgs{ActionID: "callback-denied", TaskID: task.ID + 1, ExpectedRevision: task.Revision, Step: "Continue"})
	if denied.OK || len(callbackToolsOwner.unresolvedTaskActions()) != 0 || callbackToolsOwner.mustNotify {
		t.Fatalf("callback denial reply=%+v; failures=%+v mustNotify=%t", denied, callbackToolsOwner.unresolvedTaskActions(), callbackToolsOwner.mustNotify)
	}

	userToolsOwner, userTools := newTaskActionTools(t, store, userEvent, TurnInput{Tasks: []Task{task}})
	for _, candidate := range userTools {
		info, err := candidate.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == "settle_task" {
			t.Fatal("direct user tool list exposed callback-only settle_task")
		}
	}
	if len(userToolsOwner.unresolvedTaskActions()) != 0 || userToolsOwner.mustNotify {
		t.Fatalf("unavailable user settlement mutated reply state: failures=%+v mustNotify=%t", userToolsOwner.unresolvedTaskActions(), userToolsOwner.mustNotify)
	}
}
