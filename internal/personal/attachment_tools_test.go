package personal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"kagari/internal/channel"
	"kagari/internal/inputbudget"
	"kagari/internal/textpage"
)

func invokeAttachmentTool(t *testing.T, tools []tool.BaseTool, args any) (attachmentReply, string) {
	return invokeAttachmentToolContext(t, context.Background(), tools, args)
}

func invokeAttachmentToolContext(t *testing.T, ctx context.Context, tools []tool.BaseTool, args any) (attachmentReply, string) {
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
		if info.Name != "read_attachment" {
			continue
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatal("read_attachment is not invokable")
		}
		output, err := invokable.InvokableRun(ctx, string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		var reply attachmentReply
		if err := json.Unmarshal([]byte(output), &reply); err != nil {
			t.Fatalf("decode read_attachment reply %q: %v", output, err)
		}
		return reply, output
	}
	t.Fatal("read_attachment tool not found")
	return attachmentReply{}, ""
}

func TestTaskReadsSelectedFileAndProducesCandidate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	file, err := s.SaveAttachment(ctx, "input.txt", "text/plain", strings.NewReader("source evidence"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("read_attachment", map[string]string{"id": file.ID}, "read-file"),
		responseTool("write_attachment", map[string]string{"name": "result.txt", "text": "processed evidence"}, "write-file"),
		responseText("Result is ready for review."),
	}}
	input := TaskInput{Goal: "Process the file", Attachments: []channel.Attachment{file}}
	run := createRunningTask(t, s, "read-file", input)
	result, err := TaskExecutor(readingEngine(fixture, nil), s, t.TempDir())(ctx, run, nil)
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
	limits, err := inputbudget.NewLimits(20_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := assembleTurn(ctx, TurnInput{Event: Event{ID: run.EventID, Kind: EventTask, TaskID: run.TaskID}, CurrentRun: &Run{ID: run.ID, Kind: RunTask, TaskID: run.TaskID, Revision: run.Revision, Result: string(raw)}}, "instruction", nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	var decoded runResultCard
	if err := json.Unmarshal([]byte(assembled.CurrentRun.Result), &decoded); err != nil || len(decoded.AttachmentIDs) != 1 || decoded.AttachmentIDs[0] != result.Attachments[0].ID {
		t.Fatalf("attachment reference lost during context assembly: %v", err)
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
	s := testStore(t, filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	file, err := s.SaveAttachment(ctx, "binary.bin", "application/octet-stream", strings.NewReader("a\x00b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readAttachmentText(ctx, s, file.ID); err == nil {
		t.Fatal("binary accepted as text")
	}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("read_attachment", map[string]string{"id": file.ID}, "read-file"), responseText("Unavailable."),
	}}
	run := createRunningTask(t, s, "unselected-file", TaskInput{Goal: "Read selected material"})
	_, err = TaskExecutor(readingEngine(fixture, nil), s, t.TempDir())(ctx, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestInput(fixture.requests[1]), "not part of this task") {
		t.Fatal("task accessed unselected file")
	}
}

func TestReadAttachmentPagesStayBoundedAndRecheckDeletion(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	content := strings.Repeat("<", 8000)
	file, err := s.SaveAttachment(ctx, "large.txt", "text/plain", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	observed := []textpage.Page{}
	tools, err := attachmentTools(s, map[string]bool{file.ID: true}, nil, nil, func(id string, page textpage.Page) {
		if id != file.ID {
			t.Fatalf("observed attachment id = %s", id)
		}
		observed = append(observed, page)
	})
	if err != nil {
		t.Fatal(err)
	}
	budget := textpage.WithMaxBytes(ctx, 1024)
	first, raw := invokeAttachmentToolContext(t, budget, tools, map[string]any{"id": file.ID, "limit": 8000})
	if len(raw) > textpage.MaxBytes(budget) || first.Error != "" || first.Page == nil || first.Page.TotalRunes != 8000 || first.Page.End != len([]rune(first.Page.Text)) || !first.Page.HasMore {
		t.Fatalf("first attachment page bytes=%d reply=%+v", len(raw), first)
	}
	if len(observed) != 1 || !reflect.DeepEqual(observed[0], *first.Page) {
		t.Fatalf("observed page = %+v, reply = %+v", observed, first.Page)
	}
	page := *first.Page
	var got strings.Builder
	got.WriteString(page.Text)
	for page.HasMore {
		next, raw := invokeAttachmentToolContext(t, budget, tools, map[string]any{"id": file.ID, "offset": page.End, "limit": 4000, "version": page.Version})
		if len(raw) > textpage.MaxBytes(budget) || next.Error != "" || next.Page == nil || next.Page.Offset != page.End || next.Page.End <= page.End {
			t.Fatalf("attachment page bytes=%d reply=%+v", len(raw), next)
		}
		page = *next.Page
		got.WriteString(page.Text)
	}
	if got.String() != content {
		t.Fatal("bounded attachment pages did not preserve the complete text")
	}
	if err := s.DeleteAttachment(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	deleted, _ := invokeAttachmentToolContext(t, budget, tools, map[string]any{"id": file.ID, "offset": first.Page.End, "version": first.Page.Version})
	if deleted.Error == "" {
		t.Fatal("later page read succeeded after deletion")
	}
}

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
	raw, _ := json.Marshal(personalTurnResponse{ReplyKind: replyContent, Attachments: []string{file.ID}})
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
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseText(`{"text":"","reply_kind":"content","attachments":["00000000000000000000000000000000"]}`)}}
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
			responseTool("update_task", args, "update"), responseText(`{"text":"Updated.","reply_kind":"content","attachments":[]}`),
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
