package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func TestDeliveryProgressSnapshotMatchesHistoryReadAndDiagnostic(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	source := openUserEvent(t, s, 1, 1, "Create a task and save a report")
	task, _ := createTask(t, s, source, "create", "Report", "Prepare a report", "")
	var attachments []string
	for _, name := range []string{"one.txt", "two.txt", "three.txt", "four.txt"} {
		attachment, err := s.SaveAttachment(ctx, name, "text/plain", strings.NewReader(name))
		if err != nil {
			t.Fatal(err)
		}
		attachments = append(attachments, attachment.ID)
	}
	finishEvent(t, s, source, personal.TurnOutput{Text: "Full report", TaskID: task.ID, Attachments: attachments}, []string{"first chunk", "second chunk", "third chunk"})
	var deliveryID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_delivery_intents WHERE event_id=?`, source.ID).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	statuses := []string{"pending", "sending", "sent", "failed", "uncertain", "suppressed", "sent"}
	for part, status := range statuses {
		receipt := channel.Receipt{}
		if status == "sent" {
			receipt = channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: fmt.Sprintf("remote-%d", part+1)}
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE personal_outbox SET status=?,attempts=?,detail=?,receipt_channel=?,receipt_account=?,receipt_conversation=?,receipt_message_id=?
			WHERE delivery_id=? AND delivery_part=?`, status, part+1, "item detail", receipt.Channel, receipt.Account, receipt.Conversation, receipt.MessageID, deliveryID, part+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE personal_delivery_intents SET state='suppressed' WHERE id=?`, deliveryID); err != nil {
		t.Fatal(err)
	}
	reader := openUserEvent(t, s, 2, 2, "Inspect the delivery")
	direct, err := s.Delivery(ctx, personal.ReadScope{EventID: reader.ID}, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Progress == nil || len(direct.Progress.Items) != len(statuses) || direct.Progress.AcceptedItems != 2 || direct.Progress.FullyAccepted {
		t.Fatalf("delivery progress = %+v, want seven items, two accepted, incomplete", direct.Progress)
	}
	for i, status := range statuses {
		item := direct.Progress.Items[i]
		if item.DeliveryPart != i+1 || item.Status != status || item.Attempts != i+1 || item.Detail != "item detail" {
			t.Fatalf("progress item %d = %+v, want status %q", i+1, item, status)
		}
	}
	for i, text := range []string{"first chunk", "second chunk", "third chunk"} {
		if direct.Progress.Items[i].Text != text {
			t.Fatalf("text item %d = %+v, want %q", i+1, direct.Progress.Items[i], text)
		}
	}
	for i, attachmentID := range attachments {
		if direct.Progress.Items[i+3].AttachmentID != attachmentID {
			t.Fatalf("attachment item %d = %+v, want %q", i+1, direct.Progress.Items[i+3], attachmentID)
		}
	}
	if direct.Progress.Items[2].Receipt == nil || direct.Progress.Items[2].Receipt.MessageID != "remote-3" ||
		direct.Progress.Items[6].AttachmentID != attachments[3] || direct.Progress.Items[6].Receipt == nil || direct.Progress.Items[6].Receipt.MessageID != "remote-7" {
		t.Fatalf("text or attachment receipts lost: %+v", direct.Progress.Items)
	}
	page, err := s.QueryDeliveries(ctx, personal.ReadScope{EventID: reader.ID}, personal.DeliveryQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Progress == nil || len(page.Items[0].Progress.Items) != len(statuses) {
		t.Fatalf("QueryDeliveries progress=(%+v,%v)", page, err)
	}
	history, err := s.History(ctx, reader.ID+1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var assistant personal.Message
	for _, message := range history {
		if message.Role == "assistant" && message.Delivery != nil && message.Delivery.ID == deliveryID {
			assistant = message
		}
	}
	if assistant.HistoryID == 0 || assistant.TaskID != task.ID || assistant.MessageID != "" || assistant.ReplyToID != "" ||
		assistant.Delivery.Progress == nil || len(assistant.Delivery.Progress.Items) != len(statuses) {
		t.Fatalf("assistant History snapshot = %+v", assistant)
	}
	read, err := s.ReadRecord(ctx, personal.ReadScope{EventID: reader.ID}, personal.RecordRequest{Kind: "message", ID: assistant.HistoryID})
	if err != nil || read.Page.Text != "Full report" || read.Delivery == nil || read.Delivery.ID != deliveryID || read.Delivery.Progress.AcceptedItems != 2 {
		t.Fatalf("ReadRecord delivery snapshot=(%+v,%v)", read, err)
	}
	snapshot, err := s.Diagnostic(ctx, source.ID, 0)
	if err != nil || len(snapshot.DeliveryIntents) != 1 || snapshot.DeliveryIntents[0].Progress == nil || len(snapshot.DeliveryIntents[0].Progress.Items) != len(statuses) {
		t.Fatalf("Diagnostic progress=(%+v,%v)", snapshot.DeliveryIntents, err)
	}
	var secondAttachmentOutboxID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_outbox WHERE delivery_id=? AND delivery_part=7`, deliveryID).Scan(&secondAttachmentOutboxID); err != nil {
		t.Fatal(err)
	}
	replied, err := s.RepliedMessage(ctx, secondAttachmentOutboxID)
	if err != nil || replied == nil || replied.OutboxID != secondAttachmentOutboxID || replied.HistoryID != assistant.HistoryID || replied.Delivery == nil || replied.Delivery.ID != deliveryID ||
		replied.Text != "" || len(replied.Attachments) != 1 || replied.Attachments[0].ID != attachments[3] || replied.MessageID != "remote-7" || replied.Channel != "telegram" {
		t.Fatalf("exact RepliedMessage=(%+v,%v)", replied, err)
	}
}

func TestReplyIdentityUsesExactSecondDeliveryScopeAndOutboxItem(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	source := openUserEvent(t, s, 1, 1, "Create three tasks")
	taskA, _ := createTask(t, s, source, "task-a", "Task A", "Goal A", "")
	taskB, _ := createTask(t, s, source, "task-b", "Task B", "Goal B", "")
	taskC, _ := createTask(t, s, source, "task-c", "Task C", "Goal C", "")
	control, err := s.ControlDelivery(ctx, source.ID, "stop-a", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: taskA.ID})
	if err != nil {
		t.Fatal(err)
	}
	secondGroupText := "second delivery full text"
	finishEvent(t, s, source, personal.TurnOutput{Silent: true, SupplementalReply: &personal.SupplementalReply{
		Draft: personal.DeliveryDraft{Text: secondGroupText}, TaskIDs: []int64{taskB.ID, taskC.ID},
		Purpose: personal.DeliveryResult, Chunks: []string{"second group first", "second group second"},
	}}, nil)
	var secondDeliveryID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_delivery_intents WHERE event_id=? AND purpose='result'`, source.ID).Scan(&secondDeliveryID); err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimDelivery(ctx)
	if err != nil || first == nil || first.DeliveryID != control.DeliveryID || first.TaskID != taskA.ID {
		t.Fatalf("first delivery claim=(%+v,%v), want Task A control", first, err)
	}
	if err := s.UpdateDelivery(ctx, first.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "control-receipt"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimDelivery(ctx)
	if err != nil || second == nil || second.DeliveryID != secondDeliveryID || second.DeliveryPart != 1 || second.TaskID != 0 {
		t.Fatalf("second group first item=(%+v,%v), want ambiguous Task scope", second, err)
	}
	if err := s.UpdateDelivery(ctx, second.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "second-part-one"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	third, err := s.ClaimDelivery(ctx)
	if err != nil || third == nil || third.DeliveryID != secondDeliveryID || third.DeliveryPart != 2 || third.TaskID != 0 {
		t.Fatalf("second group second item=(%+v,%v), want ambiguous Task scope", third, err)
	}
	// This deliberately collides with the original inbound message ID. A direct
	// receipt match must take precedence over the original-message fallback.
	secondReceipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: source.MessageID}
	if err := s.UpdateDelivery(ctx, third.ID, "sent", secondReceipt, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	replyInput := sourceInput("reply-to-second", "Reply to the second group, second item")
	replyInput.ReplyToID = source.MessageID
	replyID := acceptInput(t, s, "2", replyInput)
	replyEvent, err := s.ClaimEvent(ctx)
	if err != nil || replyEvent == nil || replyEvent.ID != replyID || replyEvent.ReplyOutboxID != third.ID || replyEvent.TaskID != 0 {
		t.Fatalf("receipt reply Event=(%+v,%v), want exact second delivery and ambiguous Task", replyEvent, err)
	}
	referenced, err := s.ReferencedTasks(ctx, replyID)
	if err != nil || len(referenced) != 2 || referenced[0].ID != taskB.ID || referenced[1].ID != taskC.ID {
		t.Fatalf("ReferencedTasks=(%+v,%v), want only second delivery's two Tasks", referenced, err)
	}
	replied, err := s.RepliedMessage(ctx, third.ID)
	if err != nil || replied == nil || replied.EventID != source.ID || replied.HistoryID == 0 || replied.Delivery == nil || replied.Delivery.ID != secondDeliveryID ||
		replied.Text != "second group second" || replied.MessageID != source.MessageID || replied.TaskID != 0 {
		t.Fatalf("RepliedMessage for exact second item=(%+v,%v)", replied, err)
	}
}

func TestSuppressedDeliveryCanBeFullyAcceptedAfterInflightSend(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	resultEvent := openUserEvent(t, s, 1, 1, "Send one reply")
	finishEvent(t, s, resultEvent, personal.TurnOutput{Text: "one part"}, []string{"one part"})
	var deliveryID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_delivery_intents WHERE event_id=?`, resultEvent.ID).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	inflight, err := s.ClaimDelivery(ctx)
	if err != nil || inflight == nil || inflight.DeliveryID != deliveryID {
		t.Fatalf("ClaimDelivery()=(%+v,%v)", inflight, err)
	}
	stopEvent := openUserEvent(t, s, 2, 2, "Stop this delivery")
	if _, err := s.ControlDelivery(ctx, stopEvent.ID, "stop-delivery", personal.DeliveryControlRequest{Action: personal.DeliveryStop, DeliveryID: deliveryID}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDelivery(ctx, inflight.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "last-inflight"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	intent, err := s.Delivery(ctx, personal.ReadScope{EventID: stopEvent.ID}, deliveryID)
	if err != nil || intent.State != personal.DeliveryIntentSuppressed || intent.Progress == nil || !intent.Progress.FullyAccepted || intent.Progress.AcceptedItems != 1 {
		t.Fatalf("suppressed final in-flight result=(%+v,%v), want fully accepted", intent, err)
	}
}

func TestAttachmentFallbackProgressDoesNotCountAsFullyAcceptedDraft(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := deliveryTestCallback(t, s)
	attachment, err := s.SaveAttachment(ctx, "result.txt", "text/plain", strings.NewReader("result"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, personal.TaskSettlement{
		Status: personal.TaskCompleted, Summary: "Saved result", Delivery: personal.DeliveryDraft{Attachments: []string{attachment.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	intents, err := s.EventDeliveries(ctx, callback.ID)
	if err != nil || len(intents) != 1 {
		t.Fatalf("EventDeliveries=(%+v,%v)", intents, err)
	}
	if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
		t.Fatal(err)
	}
	fallback := "The selected attachment is no longer available."
	chunks, err := channel.PrepareText(fallback)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEvent(ctx, callback.ID, personal.TurnOutput{Silent: true}, nil, personal.RenderedDelivery{
		DeliveryID: intents[0].ID, Text: fallback, Chunks: chunks, AttachmentFailure: true,
	}); err != nil {
		t.Fatal(err)
	}
	item, err := s.ClaimDelivery(ctx)
	if err != nil || item == nil || item.Text != fallback {
		t.Fatalf("fallback delivery claim=(%+v,%v)", item, err)
	}
	if err := s.UpdateDelivery(ctx, item.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "fallback"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	reader := openUserEvent(t, s, 2, 2, "Check the saved result")
	delivered, err := s.Delivery(ctx, personal.ReadScope{EventID: reader.ID}, intents[0].ID)
	if err != nil || delivered.PreparationError == "" || delivered.Progress == nil || delivered.Progress.AcceptedItems != 1 || delivered.Progress.FullyAccepted {
		t.Fatalf("fallback delivery progress=(%+v,%v), want accepted notice but incomplete original draft", delivered, err)
	}
	var historyID int64
	var deliveryID sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT id,delivery_id FROM personal_history WHERE event_id=? AND role='assistant'`, callback.ID).Scan(&historyID, &deliveryID); err != nil || !deliveryID.Valid || deliveryID.Int64 != intents[0].ID {
		t.Fatalf("fallback history=(%d,%v,%v)", historyID, deliveryID, err)
	}
	read, err := s.ReadRecord(ctx, personal.ReadScope{EventID: reader.ID}, personal.RecordRequest{Kind: "message", ID: historyID})
	if err != nil || read.Delivery == nil || read.Delivery.PreparationError == "" || read.Delivery.Progress.FullyAccepted {
		t.Fatalf("fallback History snapshot=(%+v,%v)", read, err)
	}
}
