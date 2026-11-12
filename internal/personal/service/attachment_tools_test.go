package service

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal/internal/testkit"
)

func TestFileOnlyModelReplyQueuesAttachment(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	file, err := s.SaveAttachment(ctx, "result.txt", "text/plain", strings.NewReader("immutable result"))
	if err != nil {
		t.Fatal(err)
	}
	input := testkit.SourceInput("opaque-id", "Send the file")
	input.Attachments = []channel.Attachment{file}
	testkit.AcceptInput(t, s, "next", input)
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseText(`{"text":"","reply_kind":"content","attachments":["` + file.ID + `"]}`),
	}}
	main := testMainAgent(t, fixture, s)
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
	contents, err := s.OpenAttachment(ctx, queued.AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	text, err := io.ReadAll(contents)
	if err != nil || string(text) != "immutable result" {
		t.Fatalf("queued contents changed: %q %v", text, err)
	}
}

func TestUnavailableModelAttachmentBecomesReplyFailure(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	testkit.AcceptInput(t, s, "next", testkit.SourceInput("request", "Send the file"))
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseText(`{"text":"","reply_kind":"content","attachments":["00000000000000000000000000000000"]}`),
	}}
	main := testMainAgent(t, fixture, s)
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
