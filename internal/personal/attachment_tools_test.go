package personal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
)

func TestTaskReadsSelectedFileAndProducesCandidate(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	file, err := s.SaveAttachment(ctx, "input.txt", "text/plain", strings.NewReader("source evidence"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("read_attachment", map[string]string{"id": file.ID}, "read-file"),
		responseTool("write_attachment", map[string]string{"name": "result.txt", "text": "processed evidence"}, "write-file"),
		analysisResponse("Result is ready for review."),
	}}
	result, err := ReadingExecutor(readingEngine(fixture, nil), s)(ctx, executorRun(TaskInput{Goal: "Process the file", Attachments: []channel.Attachment{file}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Attachments) != 1 {
		t.Fatalf("missing candidate: %+v", result)
	}
	text, err := readAttachmentText(ctx, s, result.Attachments[0].ID)
	if err != nil || text != "processed evidence" {
		t.Fatalf("file: %q %v", text, err)
	}
	if !strings.Contains(requestInput(fixture.requests[1]), "source evidence") {
		t.Fatal("file text did not reach the task")
	}
	raw, _ := json.Marshal(StageResult{Summary: strings.Repeat("summary", 1000), Attachments: result.Attachments})
	bounded := boundedTurnInput(TurnInput{CurrentRun: &Run{Kind: RunTask, Result: string(raw)}})
	var decoded StageResult
	if err := json.Unmarshal([]byte(bounded.CurrentRun.Result), &decoded); err != nil || len(decoded.Attachments) != 1 || decoded.Attachments[0].ID != result.Attachments[0].ID {
		t.Fatalf("attachment lost during context truncation: %v", err)
	}
	if err := s.DeleteAttachment(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := readAttachmentText(ctx, s, file.ID); err == nil {
		t.Fatal("read deleted file")
	}
	if _, err := readAttachmentText(ctx, s, result.Attachments[0].ID); err != nil {
		t.Fatalf("deleting original affected result: %v", err)
	}
}

func TestAttachmentToolsRejectUnselectedAndBinaryFiles(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	file, err := s.SaveAttachment(ctx, "binary.bin", "application/octet-stream", strings.NewReader("a\x00b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readAttachmentText(ctx, s, file.ID); err == nil {
		t.Fatal("binary accepted as text")
	}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("read_attachment", map[string]string{"id": file.ID}, "read-file"), analysisResponse("Unavailable."),
	}}
	_, err = ReadingExecutor(readingEngine(fixture, nil), s)(ctx, executorRun(TaskInput{Goal: "Read selected material"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestInput(fixture.requests[1]), "not part of this task") {
		t.Fatal("task accessed unselected file")
	}
}

func TestDecodeFileOnlyReply(t *testing.T) {
	reply, err := decodePersonalTurnResponse(`{"text":"","notify":true,"attachments":["file-id"]}`)
	if err != nil || len(reply.Attachments) != 1 {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
	for _, raw := range []string{`{"text":"","notify":true,"attachments":[]}`, `{"text":"ok","notify":true,"attachments":null}`, `{"text":"ok","notify":true,"attachments":[""]}`} {
		if _, err := decodePersonalTurnResponse(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestFileOnlyModelReplyQueuesAttachment(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	file, err := s.SaveAttachment(ctx, "result.txt", "text/plain", strings.NewReader("immutable result"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindChannel(ctx, "weixin", "bot", "owner", false); err != nil {
		t.Fatal(err)
	}
	_, err = s.AcceptBatch(ctx, "weixin", "bot", "next", []Input{{Channel: "weixin", Account: "bot", Sender: "owner", MessageID: "opaque-id", Text: "Send the file", Attachments: []channel.Attachment{file}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(personalTurnResponse{Notify: true, Attachments: []string{file.ID}})
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseText(string(raw))}}
	main := &MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 3}
	runtime := testRuntime(s)
	runtime.Think = main.Think
	if worked, err := runtime.turn(ctx); err != nil || !worked {
		t.Fatalf("turn: %v %v", worked, err)
	}
	queued, err := s.ClaimDelivery(ctx)
	if err != nil || queued == nil || queued.AttachmentID != file.ID || queued.Text != "" {
		t.Fatalf("queued=%+v err=%v", queued, err)
	}
	if _, err = s.SaveAttachment(ctx, "result.txt", "text/plain", strings.NewReader("new result")); err != nil {
		t.Fatal(err)
	}
	text, err := readAttachmentText(ctx, s, queued.AttachmentID)
	if err != nil || text != "immutable result" {
		t.Fatalf("queued contents changed: %q %v", text, err)
	}
}

func TestUnavailableModelAttachmentBecomesReplyFailure(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.BindChannel(ctx, "weixin", "bot", "owner", false); err != nil {
		t.Fatal(err)
	}
	_, err = s.AcceptBatch(ctx, "weixin", "bot", "next", []Input{{Channel: "weixin", Account: "bot", Sender: "owner", MessageID: "request", Text: "Send the file"}})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseText(`{"text":"","notify":true,"attachments":["00000000000000000000000000000000"]}`)}}
	main := &MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 3}
	runtime := testRuntime(s)
	runtime.Think = main.Think
	if worked, err := runtime.turn(ctx); err != nil || !worked {
		t.Fatalf("turn: %v %v", worked, err)
	}
	queued, err := s.ClaimDelivery(ctx)
	if err != nil || queued == nil || queued.AttachmentID != "" || !strings.Contains(queued.Text, "附件") {
		t.Fatalf("missing failure reply: %+v %v", queued, err)
	}
}

func TestTaskUpdatePreservesOmittedAttachmentsAndClearsExplicitEmpty(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	file, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("source"))
	if err != nil {
		t.Fatal(err)
	}
	event := openUserEvent(t, s, 1, 1, "Create a task")
	task, _, err := s.CreateTask(ctx, event.ID, "create", "task", TaskInput{Goal: "Read material", Attachments: []channel.Attachment{file}})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{}, nil)
	for i, clear := range []bool{false, true} {
		event = openUserEvent(t, s, int64(i+2), int64(i+2), "Update the task")
		args := map[string]any{"task_id": task.ID, "expected_revision": task.Revision, "goal": "Revise the analysis", "context": ""}
		if clear {
			args["attachments"] = []string{}
		}
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("update_task", args, "update"), responseText(`{"text":"Updated.","notify":true,"attachments":[]}`),
		}}
		main := &MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 4}
		output, err := main.Think(ctx, TurnInput{Event: event})
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
		finishEvent(t, s, event, output, nil)
	}
}
