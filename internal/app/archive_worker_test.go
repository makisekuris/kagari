package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/store"
)

func TestArchiveCommandsCompleteThroughPrivateOutbox(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(t.TempDir() + "/archive.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sub := domain.Submission{UserID: 7, ChatID: 7, Text: "original submission", ReceivedAt: time.Now()}
	payload, _ := json.Marshal(sub)
	id, _, err := s.Enqueue(ctx, "analyze", "archive", payload, -100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(domain.Result{Analysis: domain.Analysis{Title: "private archive title", Overview: "saved report"}})
	if err := s.CompleteJob(ctx, id, result, nil, nil); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: s, Config: config.Config{Telegram: config.Telegram{TargetChatID: -100}}}
	sequence := 0
	command := func(userID int64, text string) string {
		t.Helper()
		sequence++
		payload, _ := json.Marshal(domain.Command{UserID: userID, ChatID: userID, Text: text})
		jobID, _, err := s.Enqueue(ctx, "command", fmt.Sprintf("command:%d", sequence), payload, userID)
		if err != nil {
			t.Fatal(err)
		}
		job, err := s.StartJob(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Process(ctx, job); err != nil {
			t.Fatal(err)
		}
		job, err = s.Job(ctx, jobID)
		if err != nil || job.Status != "completed" {
			t.Fatalf("command outcome = %+v, %v", job, err)
		}
		delivery, err := s.ClaimDelivery(ctx)
		if err != nil || delivery == nil || delivery.JobID != jobID || delivery.ChatID != userID {
			t.Fatalf("private reply delivery = %+v, %v", delivery, err)
		}
		if err := s.SentDelivery(ctx, delivery.ID, int64(sequence)); err != nil {
			t.Fatal(err)
		}
		return delivery.Text
	}
	if text := command(7, "/archive"); !strings.Contains(text, "original submission") {
		t.Fatalf("archive list reply = %s", text)
	}
	if text := command(8, fmt.Sprintf("/archive_delete %d confirm", id)); strings.Contains(text, "private archive title") {
		t.Fatalf("unowned archive title leaked: %s", text)
	}
	if entry, err := s.ArchiveEntry(ctx, 7, id); err != nil || entry == nil {
		t.Fatalf("other user's command removed archive: %+v, %v", entry, err)
	}
	if text := command(7, fmt.Sprintf("/archive_delete %d", id)); !strings.Contains(text, "confirm") {
		t.Fatalf("missing delete confirmation reply: %s", text)
	}
	if entry, err := s.ArchiveEntry(ctx, 7, id); err != nil || entry == nil {
		t.Fatalf("preview removed archive: %+v, %v", entry, err)
	}
	command(7, fmt.Sprintf("/archive_delete %d confirm", id))
	if entry, err := s.ArchiveEntry(ctx, 7, id); err != nil || entry != nil {
		t.Fatalf("confirmed command did not remove archive: %+v, %v", entry, err)
	}
}
