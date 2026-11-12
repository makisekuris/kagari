package assistant

import (
	"context"
	"fmt"
	"testing"
	"time"
	"unicode/utf8"

	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func assertStrictSchema(t *testing.T, request map[string]any, name string) map[string]any {
	t.Helper()
	textConfig, _ := request["text"].(map[string]any)
	format, _ := textConfig["format"].(map[string]any)
	schema, _ := format["schema"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != name || format["strict"] != true || schema["additionalProperties"] != false {
		t.Fatalf("request lacks strict %s schema: text=%#v", name, request["text"])
	}
	return schema
}

func assertPersonalToolMode(t *testing.T, request map[string]any, eventKind personal.EventKind) {
	t.Helper()
	text, _ := request["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	tools, _ := request["tools"].([]any)
	if format["type"] == "json_schema" {
		t.Fatalf("personal tool loop must expose tools without forcing final JSON format: tools=%d format=%v", len(tools), format["type"])
	}
	names := make(map[string]bool, len(tools))
	for _, raw := range tools {
		definition := raw.(map[string]any)
		name := definition["name"].(string)
		if names[name] {
			t.Fatalf("duplicate personal tool %q", name)
		}
		names[name] = true
	}
	for _, name := range []string{
		"create_task", "list_tasks", "get_task", "update_task", "cancel_task", "continue_task", "get_run", "list_runs",
		"memory_search", "memory_get", "memory_put", "memory_forget", "memory_history", "read_attachment", "write_attachment",
		"create_schedule", "list_schedules", "get_schedule", "update_schedule", "cancel_schedule",
		"propose_schedule_context_update", "resolve_schedule_context_update", "add_task_input", "set_user_questions",
	} {
		if !names[name] {
			t.Errorf("personal tool loop is missing %q", name)
		}
	}
	if names["settle_task"] != (eventKind == personal.EventTask) {
		t.Fatalf("settle_task exposure for %s event = %t", eventKind, names["settle_task"])
	}
}

// toolRepo supplies only behavior exercised by assistant tests; the embedded
// interface keeps unused repository operations outside the fake's scope.
type toolRepo struct {
	Repository
	sources       map[string]domain.Source
	tasks         map[int64]personal.Task
	runs          map[int64]personal.Run
	memories      map[string]personal.Memory
	created       []personal.TaskInput
	createdKeys   []string
	revisedKeys   []string
	continued     []continueRecord
	settled       []settleRecord
	cancelled     []int64
	memoryChanges []personal.MemoryChange
	memoryKeys    []string
	memoryResult  personal.Memory
	memoryErr     error
	getTaskIDs    []int64
	getRunIDs     []int64
	listRunIDs    []int64
	historyKeys   []string
}

type continueRecord struct {
	taskID, revision int64
	step             string
	extend           bool
	key              string
}

type settleRecord struct {
	taskID, revision int64
	status           personal.TaskStatus
	summary          string
}

func newToolRepo() *toolRepo {
	return &toolRepo{tasks: make(map[int64]personal.Task), runs: make(map[int64]personal.Run), memories: make(map[string]personal.Memory)}
}

func (r *toolRepo) CreateTask(_ context.Context, _ int64, key, title string, input personal.TaskInput) (personal.Task, personal.Run, error) {
	r.createdKeys = append(r.createdKeys, key)
	r.created = append(r.created, input)
	task := personal.Task{ID: 90, Title: title, Goal: input.Goal, Context: input.Context, Revision: 1, Status: personal.TaskQueued, BudgetMS: 1_800_000}
	r.tasks[task.ID] = task
	run := personal.Run{ID: 190, TaskID: task.ID, Revision: 1, Kind: personal.RunTask, Status: personal.RunQueued, Input: input.Goal}
	r.runs[run.ID] = run
	return task, run, nil
}

func (r *toolRepo) ReviseTask(_ context.Context, _ int64, key string, taskID, revision int64, input personal.TaskInput) (personal.Task, personal.Run, error) {
	r.revisedKeys = append(r.revisedKeys, key)
	task := r.tasks[taskID]
	task.Goal, task.Context, task.Revision = input.Goal, input.Context, revision+1
	task.Status = personal.TaskQueued
	r.tasks[taskID] = task
	run := personal.Run{ID: 191, TaskID: taskID, Revision: task.Revision, Kind: personal.RunTask, Status: personal.RunQueued}
	r.runs[run.ID] = run
	return task, run, nil
}

func (r *toolRepo) CancelTask(_ context.Context, _, taskID, _ int64) (personal.Task, error) {
	r.cancelled = append(r.cancelled, taskID)
	task := r.tasks[taskID]
	task.Status = personal.TaskCancelled
	r.tasks[taskID] = task
	return task, nil
}

func (r *toolRepo) ContinueTask(_ context.Context, _ int64, key string, taskID, revision int64, step string, extend bool) (personal.Task, personal.Run, error) {
	r.continued = append(r.continued, continueRecord{taskID: taskID, revision: revision, step: step, extend: extend, key: key})
	task := r.tasks[taskID]
	task.Status = personal.TaskQueued
	r.tasks[taskID] = task
	run := personal.Run{ID: 192, TaskID: taskID, Revision: revision, Kind: personal.RunTask, Status: personal.RunQueued, Input: step}
	r.runs[run.ID] = run
	return task, run, nil
}

func (r *toolRepo) SettleTask(_ context.Context, _, taskID, revision int64, status personal.TaskStatus, summary string, _ ...*personal.CheckAssessment) (personal.Task, error) {
	r.settled = append(r.settled, settleRecord{taskID: taskID, revision: revision, status: status, summary: summary})
	task := r.tasks[taskID]
	task.Status, task.Summary = status, summary
	r.tasks[taskID] = task
	return task, nil
}

func (r *toolRepo) Task(_ context.Context, taskID int64) (*personal.Task, error) {
	r.getTaskIDs = append(r.getTaskIDs, taskID)
	task, ok := r.tasks[taskID]
	if !ok {
		return nil, nil
	}
	return &task, nil
}

func (r *toolRepo) Run(_ context.Context, runID int64) (*personal.Run, error) {
	r.getRunIDs = append(r.getRunIDs, runID)
	run, ok := r.runs[runID]
	if !ok {
		return nil, nil
	}
	return &run, nil
}

func (r *toolRepo) QueryTasks(context.Context, personal.ReadScope, personal.TaskQuery) (personal.TaskPage, error) {
	tasks := make([]personal.Task, 0, len(r.tasks))
	for _, task := range r.tasks {
		tasks = append(tasks, task)
	}
	return personal.TaskPage{Tasks: tasks}, nil
}

func (r *toolRepo) QueryRuns(_ context.Context, _ personal.ReadScope, taskID int64, _ personal.ListPage) (personal.RunPage, error) {
	r.listRunIDs = append(r.listRunIDs, taskID)
	var runs []personal.Run
	for _, run := range r.runs {
		if run.TaskID == taskID {
			runs = append(runs, run)
		}
	}
	return personal.RunPage{Runs: runs}, nil
}

func (r *toolRepo) SaveSource(_ context.Context, runID int64, key string, source domain.Source) (domain.SourceRef, error) {
	if r.sources == nil {
		r.sources = map[string]domain.Source{}
	}
	id := "m_fixture_" + key
	source.ID = id
	r.sources[id] = source
	return domain.SourceRef{ID: id, RunID: runID, URL: source.URL, Title: source.Title, Status: source.Status, FetchedAt: source.FetchedAt, SourceTruncated: source.Truncated, TotalRunes: utf8.RuneCountInString(source.Content)}, nil
}

func (r *toolRepo) Memory(_ context.Context, key string) (*personal.Memory, error) {
	memory, ok := r.memories[key]
	if !ok {
		return nil, nil
	}
	return &memory, nil
}

func (r *toolRepo) Memories(context.Context, string, int) ([]personal.Memory, error) {
	memories := make([]personal.Memory, 0, len(r.memories))
	for _, memory := range r.memories {
		memories = append(memories, memory)
	}
	return memories, nil
}

func (r *toolRepo) WriteMemory(_ context.Context, _ int64, requestKey string, change personal.MemoryChange) (personal.Memory, error) {
	r.memoryKeys = append(r.memoryKeys, requestKey)
	r.memoryChanges = append(r.memoryChanges, change)
	if r.memoryErr == nil && r.memoryResult.Key != "" {
		r.memories[r.memoryResult.Key] = r.memoryResult
	}
	return r.memoryResult, r.memoryErr
}

func (r *toolRepo) MemoryHistory(_ context.Context, key string, _ int) ([]personal.Memory, error) {
	r.historyKeys = append(r.historyKeys, key)
	if key == "" {
		return nil, nil
	}
	return []personal.Memory{{Key: key, Value: "old", Version: 2}, {Key: key, Version: 3, Deleted: true}}, nil
}

func newMainAgent(t *testing.T, fixture *testkit.ResponsesFixture, repo Repository) *MainAgent {
	t.Helper()
	return &MainAgent{
		Store: repo, Model: fixture.Model(t), Persona: "Persona style token.", Profile: "Profile facts token.",
		MaxIterations: 6, Timeout: 4 * time.Second,
	}
}

func fixtureSource(t *testing.T, repo *toolRepo, ref domain.SourceRef) domain.Source {
	t.Helper()
	source, ok := repo.sources[ref.ID]
	if !ok {
		t.Fatal(fmt.Sprintf("missing source %s", ref.ID))
	}
	return source
}
