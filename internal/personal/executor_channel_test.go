package personal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kagari/internal/channel"
)

func TestTaskExecutorCombinesWorkspaceAndAttachmentTools(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "personal.db")
	s := testStore(t, database, time.Minute)
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(database+".workspaces", storeID)
	source, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("selected source evidence"))
	if err != nil {
		t.Fatal(err)
	}
	event := openUserEvent(t, s, 1, 1, "Prepare a report and attachment")
	task, run, err := s.CreateTask(ctx, event.ID, "report", "report", TaskInput{
		Goal: "Write a report and a processed attachment.", Attachments: []channel.Attachment{source},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{TaskID: task.ID}, nil)

	const report = "Report based on selected source evidence."
	const candidate = "Processed selected source evidence."
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("write_file", map[string]string{"file_path": "draft.md", "content": report}, "write-report"),
		responseTool("read_attachment", map[string]string{"id": source.ID}, "read-source"),
		responseTool("write_attachment", map[string]string{"name": "result.txt", "text": candidate}, "write-result"),
		responseText("Saved draft.md and result.txt."),
	}}
	runtime := testRuntime(s)
	runtime.Execute = TaskExecutor(readingEngine(fixture, nil), s, directory)
	if worked, err := runtime.task(ctx); err != nil || !worked {
		t.Fatalf("task worker: worked=%v, err=%v", worked, err)
	}

	persisted, err := s.Run(ctx, run.ID)
	if err != nil || persisted == nil || persisted.Status != RunCompleted {
		t.Fatalf("task stage was not saved: %+v, %v", persisted, err)
	}
	var result StageResult
	if err := json.Unmarshal([]byte(persisted.Result), &result); err != nil {
		t.Fatal(err)
	}
	if result.Summary != "Saved draft.md and result.txt." || len(result.Files) != 1 || result.Files[0] != "draft.md" || len(result.Attachments) != 1 {
		t.Fatalf("stage result lost workspace or attachment output: %+v", result)
	}
	content, err := os.ReadFile(filepath.Join(directory, "tasks", strconv.FormatInt(task.ID, 10), "draft.md"))
	if err != nil || string(content) != report {
		t.Fatalf("workspace report=%q, err=%v", content, err)
	}
	contentText, err := readAttachmentText(ctx, s, result.Attachments[0].ID)
	if err != nil || contentText != candidate {
		t.Fatalf("attachment result=%q, err=%v", contentText, err)
	}
	if len(fixture.requests) != 4 || !strings.Contains(requestInput(fixture.requests[2]), "selected source evidence") {
		t.Fatalf("selected attachment was not read into the task: requests=%d", len(fixture.requests))
	}
	var names []string
	for _, raw := range fixture.requests[0]["tools"].([]any) {
		names = append(names, raw.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"read_attachment", "write_attachment", "read_file", "write_file"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("task request omitted %s: %v", want, names)
		}
	}
}

type cancelOnTaskToolRepository struct {
	Repository
	eventID, taskID, revision int64
	runCalls                  atomic.Int64
}

func (s *cancelOnTaskToolRepository) Run(ctx context.Context, runID int64) (*Run, error) {
	if s.runCalls.Add(1) == 2 {
		if _, err := s.Repository.CancelTask(ctx, s.eventID, s.taskID, s.revision); err != nil {
			return nil, err
		}
	}
	return s.Repository.Run(ctx, runID)
}

func TestTaskExecutorDoesNotWriteAttachmentAfterTaskCancellation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "personal.db"), time.Minute)
	input, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("source"))
	if err != nil {
		t.Fatal(err)
	}
	createEvent := openUserEvent(t, s, 1, 1, "Process source")
	task, _, err := s.CreateTask(ctx, createEvent.ID, "process", "process", TaskInput{
		Goal: "Process the selected attachment.", Attachments: []channel.Attachment{input},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, createEvent, TurnOutput{TaskID: task.ID}, nil)
	run, err := s.ClaimRun(ctx, RunTask)
	if err != nil || run == nil {
		t.Fatalf("claim task run: %+v, %v", run, err)
	}
	cancelEvent := openUserEvent(t, s, 2, 2, "Cancel task")
	store := &cancelOnTaskToolRepository{Repository: s, eventID: cancelEvent.ID, taskID: task.ID, revision: task.Revision}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("write_attachment", map[string]string{"name": "must-not-exist.txt", "text": "blocked"}, "cancelled-write"),
		responseText("Task was cancelled."),
	}}
	_, _ = TaskExecutor(readingEngine(fixture, nil), store, t.TempDir())(ctx, *run, nil)
	if store.runCalls.Load() < 2 {
		t.Fatal("attachment write did not pass through the active-task guard")
	}
	files, err := s.Attachments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != input.ID {
		t.Fatalf("attachment write ran after cancellation: %+v", files)
	}
	if task, err := s.Task(ctx, task.ID); err != nil || task == nil || task.Status != TaskCancelled {
		t.Fatalf("task was not cancelled: %+v, %v", task, err)
	}
	finishEvent(t, s, cancelEvent, TurnOutput{}, nil)
}
