package execution

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestTaskConsultsVisionModelForSelectedImage(t *testing.T) {
	ctx := t.Context()
	store := testkit.Store(t, ":memory:", time.Minute)
	image, err := store.SaveAttachment(ctx, "selected.png", "image/png", base64.NewDecoder(base64.StdEncoding,
		strings.NewReader(testkit.OnePixelPNG)))
	if err != nil {
		t.Fatal(err)
	}
	const goal = "Read the heading on the selected slide."
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("analyze_image", map[string]string{"id": image.ID, "question": goal}, "inspect-slide"),
		testkit.ResponseText("The heading is Revenue."),
	}}
	vision := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{testkit.ResponseText("Heading: Revenue")}}
	engine := testkit.ReadingEngine(fixture, nil)
	engine.MultimodalModel = vision.RawModel(t)
	run := testkit.CreateRunningTask(t, store, "image", personal.TaskInput{Goal: goal, Attachments: []channel.Attachment{image}})
	result, err := runTaskExecutor(t, ctx, engine, store, run, nil)
	if err != nil || len(fixture.Requests) != 2 || len(vision.Requests) != 1 ||
		!strings.Contains(testkit.RequestInput(fixture.Requests[1]), "Heading: Revenue") || !strings.Contains(result.Summary, "Revenue") {
		t.Fatalf("stage summary=%q err=%v stage calls=%d vision calls=%d", result.Summary, err, len(fixture.Requests), len(vision.Requests))
	}
}

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
