package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRuntimeContextKeepsPartialDeliveryAndExactFileReply(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Hour)
	seed := testkit.OpenUserEvent(t, store, 1, 1, "Prepare two independent reports")
	first, _ := testkit.CreateTask(t, store, seed, "first", "First", "First report", "")
	second, _ := testkit.CreateTask(t, store, seed, "second", "Second", "Second report", "")
	testkit.FinishEvent(t, store, seed, personal.TurnOutput{Silent: true}, nil)
	var files []string
	for _, name := range []string{"report.txt", "evidence.txt"} {
		file, err := store.SaveAttachment(ctx, name, "text/plain", strings.NewReader(name))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file.ID)
	}
	mixed := testkit.OpenUserEvent(t, store, 2, 2, "Cancel the first and send the second report")
	if _, err := store.CancelTask(ctx, mixed.ID, first.ID, first.Revision); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(store)
	if err := runtime.publish(ctx, mixed, personal.TurnOutput{SupplementalReply: &personal.SupplementalReply{
		Draft:   personal.DeliveryDraft{Text: "Saved second report", Attachments: files},
		TaskIDs: []int64{second.ID}, Purpose: personal.DeliveryResult,
	}}); err != nil {
		t.Fatal(err)
	}
	var acceptedFile *personal.Outgoing
	for part := 0; part < 4; part++ {
		outgoing, err := store.ClaimDelivery(ctx)
		if err != nil || outgoing == nil {
			t.Fatalf("claim %d: %+v, %v", part, outgoing, err)
		}
		status, detail := "sent", ""
		receipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: fmt.Sprintf("accepted-%d", part)}
		if outgoing.AttachmentID == files[1] {
			status, detail, receipt = "failed", "channel rejected evidence file", channel.Receipt{}
		}
		if outgoing.AttachmentID == files[0] {
			acceptedFile = outgoing
		}
		if err := store.UpdateDelivery(ctx, outgoing.ID, status, receipt, detail, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if acceptedFile == nil {
		t.Fatal("report file was not sent")
	}
	reply := testkit.SourceInput("file-reply", "The report arrived; where is its evidence?")
	reply.ReplyToID = "accepted-2"
	testkit.AcceptInput(t, store, "3", reply)
	event, err := store.ClaimEvent(ctx)
	if err != nil || event == nil {
		t.Fatalf("claim reply: %+v, %v", event, err)
	}
	if event.TaskID != second.ID || event.ReplyOutboxID != acceptedFile.ID {
		t.Fatalf("file reply inherited the control target: %+v", event)
	}
	input, err := runtime.context(ctx, *event)
	if err != nil {
		t.Fatal(err)
	}
	var report, control *personal.Message
	for i := range input.History {
		m := &input.History[i]
		if m.EventID != mixed.ID || m.Role != "assistant" {
			continue
		}
		if m.Delivery == nil {
			t.Fatalf("assistant history has no delivery facts: %+v", m)
		}
		if m.Delivery.Purpose == personal.DeliveryControl {
			control = m
		} else {
			report = m
		}
	}
	if report == nil || control == nil || report.TaskID != second.ID || control.TaskID != first.ID {
		t.Fatalf("mixed history scopes: report=%+v control=%+v", report, control)
	}
	progress := report.Delivery.Progress
	if progress == nil || progress.FullyAccepted || progress.AcceptedItems != 2 || len(progress.Items) != 3 {
		t.Fatalf("partial delivery was represented as complete: %+v", progress)
	}
	if progress.Items[1].OutboxID != acceptedFile.ID || progress.Items[1].Receipt == nil || progress.Items[1].Receipt.MessageID != reply.ReplyToID || progress.Items[2].Status != "failed" || progress.Items[2].Detail != "channel rejected evidence file" {
		t.Fatalf("partial delivery lost item identities or failure: %+v", progress.Items)
	}
	exact := input.RepliedMessage
	if exact == nil || exact.HistoryID != report.HistoryID || exact.OutboxID != acceptedFile.ID || exact.TaskID != second.ID || exact.Text != "" || len(exact.Attachments) != 1 || exact.Attachments[0].ID != files[0] {
		t.Fatalf("reply did not identify the exact accepted file: %+v", exact)
	}
	if exact.Delivery == nil || exact.Delivery.ID != report.Delivery.ID || exact.Delivery.Progress == nil || exact.Delivery.Progress.FullyAccepted {
		t.Fatalf("exact file reply lost the rest of its delivery: %+v", exact)
	}
}
