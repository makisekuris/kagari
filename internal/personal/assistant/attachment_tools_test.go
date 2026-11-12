package assistant

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestDecodeFileOnlyReply(t *testing.T) {
	reply, err := decodePersonalTurnResponse(`{"text":"","reply_kind":"content","attachments":["file-id"]}`)
	if err != nil || len(reply.Attachments) != 1 {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
	for _, raw := range []string{`{"text":"","reply_kind":"content","attachments":[]}`, `{"text":"ok","reply_kind":"content","attachments":null}`, `{"text":"ok","reply_kind":"content","attachments":[""]}`} {
		if _, err := decodePersonalTurnResponse(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestTaskUpdatePreservesOmittedAttachmentsAndClearsExplicitEmpty(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	file, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("source"))
	if err != nil {
		t.Fatal(err)
	}
	event := testkit.OpenUserEvent(t, s, 1, 1, "Create a task")
	task, _, err := s.CreateTask(ctx, event.ID, "create", "task", personal.TaskInput{
		Goal: "Read material", Attachments: []channel.Attachment{file},
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, s, event, personal.TurnOutput{}, nil)
	for i, clear := range []bool{false, true} {
		event = testkit.OpenUserEvent(t, s, int64(i+2), int64(i+2), "Update the task")
		args := map[string]any{"task_id": task.ID, "expected_revision": task.Revision, "goal": "Revise the analysis", "context": ""}
		if clear {
			args["attachments"] = []string{}
		}
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("update_task", args, "update"),
			testkit.ResponseText(`{"text":"Updated.","reply_kind":"content","attachments":[]}`),
		}}
		main := newMainAgent(t, fixture, s)
		output, err := main.Think(ctx, personal.TurnInput{Event: event})
		if err != nil {
			t.Fatal(err)
		}
		updated, err := s.Task(ctx, task.ID)
		if err != nil || updated == nil {
			t.Fatalf("task: %+v %v", updated, err)
		}
		want := 1
		if clear {
			want = 0
		}
		if len(updated.Attachments) != want {
			t.Fatalf("clear=%v attachments=%+v", clear, updated.Attachments)
		}
		task = *updated
		testkit.FinishEvent(t, s, event, output, nil)
	}
}
