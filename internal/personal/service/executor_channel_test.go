package service

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/execution"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

func TestTaskExecutorCombinesWorkspaceAndAttachmentTools(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "personal.db")
	s := testkit.Store(t, database, time.Minute)
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(database+".workspaces", storeID)
	source, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("selected source evidence"))
	if err != nil {
		t.Fatal(err)
	}
	event := testkit.OpenUserEvent(t, s, 1, 1, "Prepare a report and attachment")
	task, run, err := s.CreateTask(ctx, event.ID, "report", "report", personal.TaskInput{
		Goal: "Write a report and a processed attachment.", Attachments: []channel.Attachment{source},
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, s, event, personal.TurnOutput{TaskID: task.ID}, nil)

	const report = "Report based on selected source evidence."
	const candidate = "Processed selected source evidence."
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("write_file", map[string]string{"file_path": "draft.md", "content": report}, "write-report"),
		testkit.ResponseTool("read_attachment", map[string]string{"id": source.ID}, "read-source"),
		testkit.ResponseTool("write_attachment", map[string]string{"name": "result.txt", "text": candidate}, "write-result"),
		testkit.ResponseText("Saved draft.md and result.txt."),
	}}
	runtime := testRuntime(s)
	runtime.Execute = execution.TaskExecutor(testkit.ReadingEngine(fixture, nil), s, directory)
	if worked, err := runtime.task(ctx); err != nil || !worked {
		t.Fatalf("task worker: worked=%v, err=%v", worked, err)
	}

	persisted, err := s.Run(ctx, run.ID)
	if err != nil || persisted == nil || persisted.Status != personal.RunCompleted {
		t.Fatalf("task stage was not saved: %+v, %v", persisted, err)
	}
	var result personal.StageResult
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
	attachment, err := s.OpenAttachment(ctx, result.Attachments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer attachment.Close()
	content, err = io.ReadAll(attachment)
	if err != nil || string(content) != candidate {
		t.Fatalf("attachment result=%q, err=%v", content, err)
	}
	if len(fixture.Requests) != 4 || !strings.Contains(testkit.RequestInput(fixture.Requests[2]), "selected source evidence") {
		t.Fatalf("selected attachment was not read into the task: requests=%d", len(fixture.Requests))
	}
}

type cancelBeforeAttachment struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
	store                     *sqlite.Store
	eventID, taskID, revision int64
}

func (h *cancelBeforeAttachment) WrapInvokableToolCall(ctx context.Context, endpoint adk.InvokableToolCallEndpoint, tool *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	if tool.Name == "write_attachment" {
		if _, err := h.store.CancelTask(ctx, h.eventID, h.taskID, h.revision); err != nil {
			return nil, err
		}
	}
	return endpoint, nil
}

func TestTaskExecutorDoesNotWriteAttachmentAfterTaskCancellation(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "personal.db"), time.Minute)
	input, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("source"))
	if err != nil {
		t.Fatal(err)
	}
	createEvent := testkit.OpenUserEvent(t, s, 1, 1, "Process source")
	task, _, err := s.CreateTask(ctx, createEvent.ID, "process", "process", personal.TaskInput{
		Goal: "Process the selected attachment.", Attachments: []channel.Attachment{input},
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, s, createEvent, personal.TurnOutput{TaskID: task.ID}, nil)
	run, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatalf("claim task run: %+v, %v", run, err)
	}
	cancelEvent := testkit.OpenUserEvent(t, s, 2, 2, "Cancel task")
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("write_attachment", map[string]string{"name": "must-not-exist.txt", "text": "blocked"}, "cancelled-write"),
		testkit.ResponseText("Task was cancelled."),
	}}
	engine := testkit.ReadingEngine(fixture, nil)
	engine.Handlers = append(engine.Handlers, &cancelBeforeAttachment{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{},
		store:                             s, eventID: cancelEvent.ID, taskID: task.ID, revision: task.Revision,
	})
	_, _ = execution.TaskExecutor(engine, s, t.TempDir())(ctx, *run, nil)
	files, err := s.Attachments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != input.ID {
		t.Fatalf("attachment write ran after cancellation: %+v", files)
	}
	if task, err := s.Task(ctx, task.ID); err != nil || task == nil || task.Status != personal.TaskCancelled {
		t.Fatalf("task was not cancelled: %+v, %v", task, err)
	}
	testkit.FinishEvent(t, s, cancelEvent, personal.TurnOutput{}, nil)
}
