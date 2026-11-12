package execution

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestTaskReadsSelectedFileAndProducesCandidate(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Minute)
	file, err := store.SaveAttachment(ctx, "input.txt", "text/plain", strings.NewReader("source evidence"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("read_attachment", map[string]string{"id": file.ID}, "read-file"),
		testkit.ResponseTool("write_attachment", map[string]string{"name": "result.txt", "text": "processed evidence"}, "write-file"),
		testkit.ResponseText("Result is ready for review."),
	}}
	input := personal.TaskInput{Goal: "Process the file", Attachments: []channel.Attachment{file}}
	run := testkit.CreateRunningTask(t, store, "read-file", input)
	result, err := runTaskExecutor(t, ctx, testkit.ReadingEngine(fixture, nil), store, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Attachments) != 1 {
		t.Fatalf("missing candidate: %+v", result)
	}
	output, err := store.OpenAttachment(ctx, result.Attachments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(output)
	closeErr := output.Close()
	if readErr != nil || closeErr != nil || string(content) != "processed evidence" {
		t.Fatalf("candidate contents=%q read error=%v close error=%v", content, readErr, closeErr)
	}
	if !strings.Contains(testkit.RequestInput(fixture.Requests[1]), "source evidence") {
		t.Fatal("file text did not reach the task")
	}
	if err := store.DeleteAttachment(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenAttachment(ctx, file.ID); err == nil {
		t.Fatal("read deleted source file")
	}
	output, err = store.OpenAttachment(ctx, result.Attachments[0].ID)
	if err != nil {
		t.Fatalf("deleting original affected result: %v", err)
	}
	defer output.Close()
	content, err = io.ReadAll(output)
	if err != nil || string(content) != "processed evidence" {
		t.Fatalf("candidate after deleting source=%q error=%v", content, err)
	}
}
