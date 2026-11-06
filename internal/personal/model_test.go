package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"kagari/internal/domain"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/components/model"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"kagari/internal/inputbudget"
)

type fixtureResponse struct {
	status string
	output []any
}

type responsesFixture struct {
	t         *testing.T
	responses []fixtureResponse
	requests  []map[string]any
	index     int
}

func (f *responsesFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		f.t.Errorf("decode Responses request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.requests = append(f.requests, request)
	if f.index >= len(f.responses) {
		f.t.Errorf("unexpected Responses request %d", f.index+1)
		http.Error(w, "no fixture response", http.StatusInternalServerError)
		return
	}
	response := f.responses[f.index]
	f.index++
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": fmt.Sprintf("resp_%d", f.index), "object": "response", "status": response.status,
		"model": "fixture", "output": response.output,
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
	})
}

func (f *responsesFixture) model(t *testing.T) model.AgenticModel {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	timeout := 4 * time.Second
	maxRetries, store, parallelCalls := 0, false, false
	m, err := agenticopenai.NewResponsesModel(context.Background(), &agenticopenai.ResponsesConfig{
		BaseURL: server.URL + "/v1", APIKey: "test-key", Model: "fixture-model",
		Timeout: &timeout, MaxRetries: &maxRetries, Store: &store, ParallelToolCalls: &parallelCalls,
	})
	if err != nil {
		t.Fatal(err)
	}
	limits, err := inputbudget.NewLimits(262144, 5000)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := inputbudget.NewAgenticModel(m, inputbudget.ModelConfig{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return bounded
}

func responseTool(name string, args any, id string) fixtureResponse {
	encoded, _ := json.Marshal(args)
	return fixtureResponse{status: "completed", output: []any{map[string]any{
		"type": "function_call", "id": "fc_" + id, "call_id": id, "name": name, "arguments": string(encoded),
	}}}
}

func responseText(text string) fixtureResponse {
	return fixtureResponse{status: "completed", output: []any{map[string]any{
		"type": "message", "id": "msg", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text}},
	}}}
}

func responseRefusal() fixtureResponse {
	return fixtureResponse{status: "completed", output: []any{map[string]any{
		"type": "message", "id": "msg", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "refusal", "refusal": "I cannot help with that request."}},
	}}}
}

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

func assertPersonalToolMode(t *testing.T, request map[string]any) {
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
		"create_task", "list_tasks", "get_task", "update_task", "cancel_task", "continue_task", "settle_task", "get_run", "list_runs",
		"memory_search", "memory_get", "memory_put", "memory_forget", "memory_history", "read_attachment", "write_attachment",
		"create_schedule", "list_schedules", "get_schedule", "update_schedule", "cancel_schedule",
		"propose_schedule_context_update", "resolve_schedule_context_update", "add_task_input",
	} {
		if !names[name] {
			t.Errorf("personal tool loop is missing %q", name)
		}
	}
	for _, raw := range tools {
		definition, _ := raw.(map[string]any)
		if definition["name"] != "settle_task" {
			continue
		}
		parameters, _ := definition["parameters"].(map[string]any)
		properties, _ := parameters["properties"].(map[string]any)
		status, _ := properties["status"].(map[string]any)
		if status["type"] != "string" {
			t.Fatalf("settle_task status must retain its string schema: %v", status)
		}
		return
	}
	t.Fatal("personal tool loop is missing settle_task")
}

func requestInput(request map[string]any) string {
	encoded, _ := json.Marshal(request["input"])
	return string(encoded)
}

type toolRepo struct {
	sources map[string]domain.Source
	Repository
	tasks         map[int64]Task
	runs          map[int64]Run
	memories      map[string]Memory
	created       []TaskInput
	createdKeys   []string
	revisedKeys   []string
	continued     []continueRecord
	settled       []settleRecord
	cancelled     []int64
	memoryChanges []MemoryChange
	memoryKeys    []string
	memoryResult  Memory
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
	status           TaskStatus
	summary          string
}

func newToolRepo() *toolRepo {
	return &toolRepo{tasks: make(map[int64]Task), runs: make(map[int64]Run), memories: make(map[string]Memory)}
}

func (r *toolRepo) CreateTask(_ context.Context, _ int64, key, title string, input TaskInput) (Task, Run, error) {
	r.createdKeys = append(r.createdKeys, key)
	r.created = append(r.created, input)
	task := Task{ID: 90, Title: title, Goal: input.Goal, Context: input.Context, Revision: 1, Status: TaskQueued, BudgetMS: 1_800_000}
	r.tasks[task.ID] = task
	run := Run{ID: 190, TaskID: task.ID, Revision: 1, Kind: RunTask, Status: RunQueued, Input: input.Goal}
	r.runs[run.ID] = run
	return task, run, nil
}

func (r *toolRepo) ReviseTask(_ context.Context, _ int64, key string, taskID, revision int64, input TaskInput) (Task, Run, error) {
	r.revisedKeys = append(r.revisedKeys, key)
	task := r.tasks[taskID]
	task.Goal, task.Context, task.Revision = input.Goal, input.Context, revision+1
	task.Status = TaskQueued
	r.tasks[taskID] = task
	run := Run{ID: 191, TaskID: taskID, Revision: task.Revision, Kind: RunTask, Status: RunQueued}
	return task, run, nil
}

func (r *toolRepo) CancelTask(_ context.Context, _, taskID, _ int64) (Task, error) {
	r.cancelled = append(r.cancelled, taskID)
	task := r.tasks[taskID]
	task.Status = TaskCancelled
	r.tasks[taskID] = task
	return task, nil
}

func (r *toolRepo) ContinueTask(_ context.Context, _ int64, key string, taskID, revision int64, step string, extend bool) (Task, Run, error) {
	r.continued = append(r.continued, continueRecord{taskID: taskID, revision: revision, step: step, extend: extend, key: key})
	task := r.tasks[taskID]
	task.Status = TaskQueued
	r.tasks[taskID] = task
	run := Run{ID: 192, TaskID: taskID, Revision: revision, Kind: RunTask, Status: RunQueued, Input: step}
	r.runs[run.ID] = run
	return task, run, nil
}

func (r *toolRepo) SettleTask(_ context.Context, _, taskID, revision int64, status TaskStatus, summary string, assessment ...*CheckAssessment) (Task, error) {
	r.settled = append(r.settled, settleRecord{taskID: taskID, revision: revision, status: status, summary: summary})
	task := r.tasks[taskID]
	task.Status, task.Summary = status, summary
	r.tasks[taskID] = task
	return task, nil
}

func (r *toolRepo) Task(_ context.Context, taskID int64) (*Task, error) {
	r.getTaskIDs = append(r.getTaskIDs, taskID)
	task, ok := r.tasks[taskID]
	if !ok {
		return nil, nil
	}
	return &task, nil
}

func (r *toolRepo) Tasks(context.Context, int) ([]Task, error) {
	tasks := make([]Task, 0, len(r.tasks))
	for _, task := range r.tasks {
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (r *toolRepo) Run(_ context.Context, runID int64) (*Run, error) {
	r.getRunIDs = append(r.getRunIDs, runID)
	run, ok := r.runs[runID]
	if !ok {
		return nil, nil
	}
	return &run, nil
}

func (r *toolRepo) Runs(_ context.Context, taskID int64, _ int) ([]Run, error) {
	r.listRunIDs = append(r.listRunIDs, taskID)
	var runs []Run
	for _, run := range r.runs {
		if run.TaskID == taskID {
			runs = append(runs, run)
		}
	}
	return runs, nil
}

func (r *toolRepo) Memory(_ context.Context, key string) (*Memory, error) {
	memory, ok := r.memories[key]
	if !ok {
		return nil, nil
	}
	return &memory, nil
}

func (r *toolRepo) Memories(context.Context, string, int) ([]Memory, error) {
	var memories []Memory
	for _, memory := range r.memories {
		memories = append(memories, memory)
	}
	return memories, nil
}

func (r *toolRepo) WriteMemory(_ context.Context, _ int64, requestKey string, change MemoryChange) (Memory, error) {
	r.memoryKeys = append(r.memoryKeys, requestKey)
	r.memoryChanges = append(r.memoryChanges, change)
	if r.memoryErr == nil && r.memoryResult.Key != "" {
		r.memories[r.memoryResult.Key] = r.memoryResult
	}
	return r.memoryResult, r.memoryErr
}

func (r *toolRepo) MemoryHistory(_ context.Context, key string, _ int) ([]Memory, error) {
	r.historyKeys = append(r.historyKeys, key)
	if key == "" {
		return nil, nil
	}
	return []Memory{{Key: key, Value: "old", Version: 2}, {Key: key, Version: 3, Deleted: true}}, nil
}

func newMainAgent(t *testing.T, fixture *responsesFixture, repo Repository) *MainAgent {
	t.Helper()
	return &MainAgent{
		Store: repo, Model: fixture.model(t), Persona: "Persona style token.", Profile: "Profile facts token.",
		MaxIterations: 6, Timeout: 4 * time.Second,
	}
}

func TestThinkContinuesOnlyCallbackTaskAndCanBeSilent(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("get_task", map[string]any{"task_id": 7}, "get-task"),
		responseTool("continue_task", map[string]any{"task_id": 7, "expected_revision": 2, "step": "Read two more sources", "extend_budget": false}, "continue-task"),
		responseText(`{"text":"我会沿原目标继续执行。","notify":false,"attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.tasks[7] = Task{ID: 7, Title: "Compare approaches", Goal: "Compare two documented approaches", Revision: 2, Status: TaskAwaitingReview, BudgetMS: 1_800_000, UsedMS: 500_000}
	currentRun := Run{ID: 31, TaskID: 7, Revision: 2, Kind: RunTask, Status: RunCompleted, Result: `{"summary":"One source found."}`}
	repo.runs[31] = currentRun
	input := TurnInput{Event: Event{ID: 20, Kind: EventTask, TaskID: 7, RunID: 31, Text: "Run 31 found one source."},
		Tasks: []Task{repo.tasks[7]}, CurrentRun: &currentRun}
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
	for _, request := range fixture.requests {
		assertPersonalToolMode(t, request)
	}
	if inputText := requestInput(fixture.requests[0]); !strings.Contains(inputText, "Compare two documented approaches") || !strings.Contains(inputText, "One source found") {
		t.Fatalf("callback lacked task goal or run result: %s", inputText)
	}
}

func TestThinkSettlesItsOwnTaskAndNotifiesOnCompletion(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("settle_task", map[string]any{"task_id": 7, "expected_revision": 2, "status": TaskCompleted, "summary": "The original comparison is complete."}, "settle-own-task"),
		responseText(`{"text":"任务已完成：比较结果已保存。","notify":false,"attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.tasks[7] = Task{ID: 7, Goal: "Compare two approaches", Revision: 2, Status: TaskAwaitingReview}
	currentRun := Run{ID: 33, TaskID: 7, Revision: 2, Status: RunCompleted, Result: `{"summary":"Both approaches compared."}`}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), TurnInput{
		Event: Event{ID: 201, Kind: EventTask, TaskID: 7, RunID: currentRun.ID}, Tasks: []Task{repo.tasks[7]}, CurrentRun: &currentRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Silent || output.TaskID != 7 || len(repo.settled) != 1 || repo.settled[0].status != TaskCompleted {
		t.Fatalf("settlement output=%+v settled=%+v", output, repo.settled)
	}
}

func TestThinkCallbackCannotMutateOtherTasksOrMemory(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("create_task", map[string]any{"title": "Side quest", "goal": "Do something else", "context": ""}, "create-denied"),
		responseTool("memory_put", map[string]any{"key": "language", "value": "English", "expected_version": 0, "evidence": "Please remember"}, "memory-denied"),
		responseTool("continue_task", map[string]any{"task_id": 8, "expected_revision": 1, "step": "continue unrelated", "extend_budget": false}, "other-task-denied"),
		responseTool("continue_task", map[string]any{"task_id": 7, "expected_revision": 1, "step": "extend from callback", "extend_budget": true}, "budget-denied"),
		responseText(`{"text":"我不能从任务回调中创建任务、改记忆或继续其他任务。","notify":false,"attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.tasks[7] = Task{ID: 7, Goal: "Only this task", Revision: 1, Status: TaskAwaitingReview, BudgetMS: 1000}
	input := TurnInput{Event: Event{ID: 21, Kind: EventTask, TaskID: 7, Text: "callback"}, Tasks: []Task{repo.tasks[7]}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.createdKeys) != 0 || len(repo.memoryChanges) != 0 || len(repo.continued) != 0 || len(repo.settled) != 0 {
		t.Fatalf("callback performed forbidden mutation: %+v", repo)
	}
	for i, want := range []string{"persisted direct user event", "persisted direct user event", "their own task", "cannot extend the budget"} {
		if !strings.Contains(requestInput(fixture.requests[i+1]), want) {
			t.Fatalf("tool failure %d was not returned to model: %s", i, requestInput(fixture.requests[i+1]))
		}
	}
	if !output.Silent || output.TaskID != 7 {
		t.Fatalf("callback output=%+v", output)
	}
}

func TestThinkDirectUserQueuesTaskAndCommitsEvidenceBackedMemory(t *testing.T) {
	evidence := "Please remember that I prefer concise answers."
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("create_task", map[string]any{"title": "Find a guide", "goal": "Find a guide to local cycling", "context": "Preference snapshot: concise answers"}, "create-task"),
		responseTool("memory_put", map[string]any{"key": "answer_style", "value": "concise", "expected_version": 0, "evidence": evidence}, "put-memory"),
		responseText(`{"text":"任务已排队，记忆偏好已保存。","notify":true,"attachments":[]}`),
	}}
	repo := newToolRepo()
	event := Event{ID: 22, Kind: EventUser, Text: "Find a guide to local cycling. " + evidence}
	repo.memoryResult = Memory{Key: "answer_style", Value: "concise", Version: 1, SourceEventID: event.ID}
	input := TurnInput{Event: event}
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
	toolResults := requestInput(fixture.requests[2])
	if !strings.Contains(toolResults, `\"memory\":{\"key\":\"answer_style\"`) {
		t.Fatalf("memory tool did not return its committed result: %s", toolResults)
	}
	for _, request := range fixture.requests {
		assertPersonalToolMode(t, request)
	}
}

func TestThinkMemoryPutRejectsStaleVersionAndUnsupportedEvidence(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("memory_put", map[string]any{"key": "answer_style", "value": "detailed", "expected_version": 2, "evidence": "I now prefer detailed answers."}, "stale-memory"),
		responseTool("memory_put", map[string]any{"key": "answer_style", "value": "detailed", "expected_version": 3, "evidence": "I now prefer short answers."}, "unsupported-evidence"),
		responseText(`{"text":"我无法验证这次记忆变更。","notify":true,"attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.memoryErr = errors.New("expected_version does not match the current memory")
	event := Event{ID: 221, Kind: EventUser, Text: "Please remember that I now prefer detailed answers."}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), TurnInput{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	if output.Silent || len(repo.memoryChanges) != 1 {
		t.Fatalf("stale or unsupported memory write persisted: output=%+v changes=%+v", output, repo.memoryChanges)
	}
	if !strings.Contains(requestInput(fixture.requests[1]), "expected_version does not match") || !strings.Contains(requestInput(fixture.requests[2]), "exact current-message evidence quote") {
		t.Fatalf("memory write errors were not returned to model: %s", requestInput(fixture.requests[2]))
	}
}

func TestThinkMemoryToolFailureIsReturnedAndLoggedWithoutEvidence(t *testing.T) {
	const value = "PRIVATE_MEMORY_VALUE"
	const evidence = "PRIVATE_EVIDENCE_QUOTE"
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("memory_put", memoryPutArgs{Key: "answer_style", Value: value, Evidence: evidence}, "failed-put"),
		responseText(`{"text":"暂时无法保存。","notify":true,"attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.memoryErr = errors.New("store unavailable")
	core, logs := observer.New(zapcore.WarnLevel)
	main := newMainAgent(t, fixture, repo)
	main.Log = zap.New(core)
	_, err := main.Think(context.Background(), TurnInput{Event: Event{ID: 232, Kind: EventUser, Text: evidence}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestInput(fixture.requests[1]), `store unavailable`) {
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
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("list_runs", map[string]any{"task_id": 7}, "list-runs"),
		responseTool("memory_get", map[string]any{"key": "answer_style"}, "get-memory"),
		responseTool("memory_history", map[string]any{"key": "answer_style"}, "memory-history"),
		responseText(`{"text":"当前记忆已删除，历史记录可供审计。","notify":true,"attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.tasks[7] = Task{ID: 7, Goal: "Old goal", Revision: 1}
	repo.runs[81] = Run{ID: 81, TaskID: 7, Revision: 1, Kind: RunTask, Status: RunCompleted}
	repo.memories["answer_style"] = Memory{Key: "answer_style", Value: "old forgotten value", Version: 4, Deleted: true}
	input := TurnInput{Event: Event{ID: 23, Kind: EventUser, Text: "Show my memory history."}}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if output.Silent || len(repo.listRunIDs) != 1 || repo.listRunIDs[0] != 7 || len(repo.historyKeys) != 1 || repo.historyKeys[0] != "answer_style" {
		t.Fatalf("read tools or reply missing: output=%+v repo=%+v", output, repo)
	}
	if !strings.Contains(requestInput(fixture.requests[2]), `\"version\":4`) || !strings.Contains(requestInput(fixture.requests[2]), `\"deleted\":true`) || strings.Contains(requestInput(fixture.requests[2]), "old forgotten value") {
		t.Fatalf("memory_get did not return a value-free tombstone: %s", requestInput(fixture.requests[2]))
	}
}

func TestThinkNotificationRulesAndStrictFailures(t *testing.T) {
	tests := []struct {
		name     string
		response fixtureResponse
		wantErr  string
	}{
		{name: "invalid json", response: responseText(`{"text":"hello","notify":true,"attachments":[],"extra":1}`), wantErr: "unexpected field"},
		{name: "blank text", response: responseText(`{"text":"  ","notify":true,"attachments":[]}`), wantErr: "blank"},
		{name: "refusal", response: responseRefusal(), wantErr: "refused"},
		{name: "incomplete", response: fixtureResponse{status: "incomplete", output: responseText(`{"text":"not final","notify":true,"attachments":[]}`).output}, wantErr: "incomplete"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{tc.response}}
			if tc.wantErr == "unexpected field" || tc.wantErr == "blank" {
				fixture.responses = append(fixture.responses, tc.response)
			}
			repo := newToolRepo()
			_, err := newMainAgent(t, fixture, repo).Think(context.Background(), TurnInput{Event: Event{ID: 24, Kind: EventUser, Text: "Hi"}})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error=%v, want %q", err, tc.wantErr)
			}
		})
	}

	fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseText(`{"text":"Continued quietly.","notify":false,"attachments":[]}`)}}
	repo := newToolRepo()
	repo.tasks[7] = Task{ID: 7, Revision: 1, Goal: "Goal", Status: TaskAwaitingReview, BudgetMS: 1000}
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), TurnInput{Event: Event{ID: 25, Kind: EventTask, TaskID: 7}, Tasks: []Task{repo.tasks[7]}})
	if err != nil || !output.Silent {
		t.Fatalf("ordinary continuation should be silent, output=%+v err=%v", output, err)
	}
	fixture = &responsesFixture{t: t, responses: []fixtureResponse{responseText(`{"text":"The task completed.","notify":false,"attachments":[]}`)}}
	output, err = newMainAgent(t, fixture, repo).Think(context.Background(), TurnInput{Event: Event{ID: 26, Kind: EventTask, TaskID: 7}, Tasks: []Task{{ID: 7, Status: TaskCompleted}}})
	if err != nil || output.Silent {
		t.Fatalf("task completion must notify, output=%+v err=%v", output, err)
	}
}

func TestRequestKeyIsStableForRepeatedToolInput(t *testing.T) {
	tools := personalTools{event: Event{ID: 99}, taskIDs: make(map[int64]struct{})}
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
	tools := personalTools{event: Event{TaskID: 7}, taskIDs: map[int64]struct{}{7: {}, 8: {}}}
	if got := tools.taskID(); got != 0 {
		t.Fatalf("task ID=%d, want 0 for multiple tasks", got)
	}
	tools.taskIDs = map[int64]struct{}{7: {}}
	if got := tools.taskID(); got != 7 {
		t.Fatalf("task ID=%d, want explicit event task 7", got)
	}
}
