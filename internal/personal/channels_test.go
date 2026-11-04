package personal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
)

func TestAcceptBatchCursorIsAtomicAndDeduplicatesByChannelAccount(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	if cursor, err := s.Cursor(ctx, "telegram", "test-bot"); err != nil || cursor != "" {
		t.Fatalf("initial cursor = (%q,%v)", cursor, err)
	}
	valid := sourceInput("same-id", "first")
	invalid := valid
	invalid.MessageID, invalid.Sender = "second", "not-bound"
	if _, err := s.AcceptBatch(ctx, "telegram", "test-bot", "cursor-1", []Input{valid, invalid}); err == nil {
		t.Fatal("batch with unbound sender was accepted")
	}
	if cursor, err := s.Cursor(ctx, "telegram", "test-bot"); err != nil || cursor != "" {
		t.Fatalf("failed batch advanced cursor: (%q,%v)", cursor, err)
	}
	if event, err := s.ClaimEvent(ctx); err != nil || event != nil {
		t.Fatalf("failed batch stored event: (%+v,%v)", event, err)
	}

	ids, err := s.AcceptBatch(ctx, "telegram", "test-bot", "cursor-2", []Input{valid, valid})
	if err != nil || len(ids) != 2 || ids[0] == 0 || ids[1] != 0 {
		t.Fatalf("duplicate in batch = (%v,%v)", ids, err)
	}
	if cursor, err := s.Cursor(ctx, "telegram", "test-bot"); err != nil || cursor != "cursor-2" {
		t.Fatalf("successful cursor = (%q,%v)", cursor, err)
	}
	if err := s.BindChannel(ctx, "weixin", "wx-bot", "wx-owner", false); err != nil {
		t.Fatal(err)
	}
	weixin := Input{Channel: "weixin", Account: "wx-bot", Sender: "wx-owner", MessageID: "same-id", Text: "same ID, different source"}
	other, err := s.AcceptBatch(ctx, "weixin", "wx-bot", "wx-cursor", []Input{weixin})
	if err != nil || len(other) != 1 || other[0] == 0 || other[0] == ids[0] {
		t.Fatalf("cross-channel duplicate = (%v,%v)", other, err)
	}
	if cursor, err := s.Cursor(ctx, "telegram", "another-bot"); err != nil || cursor != "" {
		t.Fatalf("account cursor leaked: (%q,%v)", cursor, err)
	}
	if _, err := s.AcceptBatch(ctx, "telegram", "another-bot", "bad", []Input{sourceInput("other", "text")}); err == nil {
		t.Fatal("unbound account was accepted")
	}
}

func TestSharedMemoryAcrossBoundChannels(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	telegram := openUserEvent(t, s, 1, 1, "remember this preference")
	if _, err := s.WriteMemory(ctx, telegram.ID, "save", MemoryChange{Operation: "put", Key: "language", Value: "Chinese", Evidence: telegram.Text}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, telegram, TurnOutput{Silent: true}, nil)
	if err := s.BindChannel(ctx, "weixin", "wx-bot", "wx-owner", false); err != nil {
		t.Fatal(err)
	}
	ids, err := s.AcceptBatch(ctx, "weixin", "wx-bot", "wx-1", []Input{{Channel: "weixin", Account: "wx-bot", Sender: "wx-owner", MessageID: "1", Text: "continue here"}})
	if err != nil || len(ids) != 1 || ids[0] == 0 {
		t.Fatalf("Weixin event = (%v,%v)", ids, err)
	}
	event, err := s.ClaimEvent(ctx)
	if err != nil || event == nil || event.Channel != "weixin" {
		t.Fatalf("Weixin source = (%+v,%v)", event, err)
	}
	memories, err := s.Memories(ctx, "", 10)
	if err != nil || len(memories) != 1 || memories[0].Value != "Chinese" {
		t.Fatalf("shared memory = (%+v,%v)", memories, err)
	}
}

func TestFileOnlyInputSurvivesRestartAndRetainsSource(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "files.db")
	s := testStore(t, path, time.Minute)
	body := bytes.Repeat([]byte("x"), 2048)
	attachment, err := s.SaveAttachment(ctx, "../../note.txt", "text/plain; charset=utf-8", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if attachment.Name != "note.txt" || attachment.MIME != "text/plain" || attachment.Size != int64(len(body)) {
		t.Fatalf("saved metadata = %+v", attachment)
	}
	input := sourceInput("file-only", "")
	input.Attachments = []channel.Attachment{attachment}
	eventID := acceptInput(t, s, "file-cursor", input)
	if eventID == 0 {
		t.Fatal("file-only input was ignored")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = testStore(t, path, time.Minute)
	event, err := s.ClaimEvent(ctx)
	if err != nil || event == nil || event.ID != eventID || event.Text != "" || len(event.Attachments) != 1 || event.Attachments[0].ID != attachment.ID {
		t.Fatalf("restored file-only event = (%+v,%v)", event, err)
	}
	if event.Channel != "telegram" || event.Account != "test-bot" || event.Sender != "test-owner" || event.MessageID != "file-only" {
		t.Fatalf("source IDs were lost: %+v", event)
	}
	reader, err := s.OpenAttachment(ctx, attachment.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("restored bytes length=%d err=%v", len(got), err)
	}
	history, err := s.History(ctx, eventID+1, 10)
	if err != nil || len(history) != 1 || len(history[0].Attachments) != 1 {
		t.Fatalf("file history = (%+v,%v)", history, err)
	}
	finishEvent(t, s, *event, TurnOutput{Silent: true}, nil)
}

func TestAttachmentLimitDeleteProjectionAndFileOutboxReceipt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "attachments.db"), time.Minute)
	max := bytes.Repeat([]byte("a"), int(channel.MaxFileBytes))
	attachment, err := s.SaveAttachment(ctx, "boundary.bin", "", bytes.NewReader(max))
	if err != nil || attachment.Size != channel.MaxFileBytes {
		t.Fatalf("exact size boundary: (%+v,%v)", attachment, err)
	}
	if _, err := s.SaveAttachment(ctx, "over.bin", "", bytes.NewReader(bytes.Repeat([]byte("b"), int(channel.MaxFileBytes+1)))); err == nil {
		t.Fatal("attachment one byte over limit was accepted")
	}
	path, _, _, err := s.attachmentPath(ctx, attachment.ID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("stored file mode = (%v,%v)", info, err)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil || dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("attachment directory mode = (%v,%v)", dirInfo, err)
	}

	input := sourceInput("outgoing-file", "")
	input.Attachments = []channel.Attachment{attachment}
	eventID := acceptInput(t, s, "file-1", input)
	event, err := s.ClaimEvent(ctx)
	if err != nil || event == nil || event.ID != eventID {
		t.Fatalf("file event = (%+v,%v)", event, err)
	}
	finishEvent(t, s, *event, TurnOutput{Attachments: []string{attachment.ID}}, nil)
	delivery, err := s.ClaimDelivery(ctx)
	if err != nil || delivery == nil || delivery.Text != "" || delivery.AttachmentID != attachment.ID {
		t.Fatalf("file delivery = (%+v,%v)", delivery, err)
	}
	if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAttachment(ctx, attachment.ID); !errors.Is(err, ErrAttachmentUnavailable) {
		t.Fatalf("deleted attachment open = %v", err)
	}
	history, err := s.History(ctx, eventID+1, 10)
	if err != nil || len(history) != 2 || !history[0].Attachments[0].Deleted || history[0].Attachments[0].Error == "" || !history[1].Attachments[0].Deleted {
		t.Fatalf("deleted attachment history projection = (%+v,%v)", history, err)
	}
	if err := s.UpdateDelivery(ctx, delivery.ID, "failed", channel.Receipt{Channel: "weixin", Account: "wx-bot", Conversation: "wx-owner"}, "attachment deleted", time.Time{}); err != nil {
		t.Fatal(err)
	}

	second, err := s.SaveAttachment(ctx, "no-message-id.txt", "text/plain", strings.NewReader("file"))
	if err != nil {
		t.Fatal(err)
	}
	event2 := openUserEvent(t, s, 2, 2, "send the file")
	finishEvent(t, s, event2, TurnOutput{Attachments: []string{second.ID}}, nil)
	delivery2, err := s.ClaimDelivery(ctx)
	if err != nil || delivery2 == nil || delivery2.AttachmentID != second.ID {
		t.Fatalf("second file delivery = (%+v,%v)", delivery2, err)
	}
	if err := s.UpdateDelivery(ctx, delivery2.ID, "sent", channel.Receipt{Channel: "weixin", Account: "wx-bot", Conversation: "wx-owner"}, "", time.Time{}); err != nil {
		t.Fatalf("receipt without remote ID: %v", err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
		t.Fatalf("sent file resent: (%+v,%v)", next, err)
	}
	listed, err := s.Attachments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundDeleted := false
	for _, item := range listed {
		if item.ID == attachment.ID && item.Deleted {
			foundDeleted = true
		}
	}
	if !foundDeleted {
		t.Fatalf("deleted tombstone missing from attachment list: %+v", listed)
	}
}

func TestCompleteEventRejectsUnavailableAttachmentWithoutCompletingEvent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	attachment, err := s.SaveAttachment(ctx, "will-delete.bin", "", strings.NewReader("file"))
	if err != nil {
		t.Fatal(err)
	}
	event := openUserEvent(t, s, 1, 1, "respond with file")
	if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
		t.Fatal(err)
	}
	err = s.CompleteEvent(ctx, event.ID, TurnOutput{Attachments: []string{attachment.ID}}, nil)
	if !errors.Is(err, ErrAttachmentUnavailable) {
		t.Fatalf("CompleteEvent unavailable file = %v", err)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM personal_events WHERE id=?`, event.ID).Scan(&status); err != nil || status != string(EventRunning) {
		t.Fatalf("failed completion changed event status to %q: %v", status, err)
	}
}
