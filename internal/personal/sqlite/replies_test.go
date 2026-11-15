package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func TestRecoverRetiresPendingLegacyWaitingNotices(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	type notice struct {
		name       string
		eventID    int64
		deliveryID int64
		outboxID   int64
	}
	notices := make(map[string]notice)
	newNotice := func(name string, updateID int64) notice {
		t.Helper()
		event := openUserEvent(t, s, updateID, updateID, name)
		deliveryID := legacyWaitFixture(t, s, event.ID)
		var outboxID int64
		if err := s.db.QueryRowContext(ctx, `SELECT wait_outbox_id FROM personal_reply_groups WHERE id=?`, event.ID).Scan(&outboxID); err != nil {
			t.Fatal(err)
		}
		return notice{name: name, eventID: event.ID, deliveryID: deliveryID, outboxID: outboxID}
	}

	notices["pending"] = newNotice("pending legacy wait", 1)
	if _, err := s.db.ExecContext(ctx, `UPDATE personal_outbox SET next_attempt_at=? WHERE id=?`, timestamp(time.Now().Add(time.Hour)), notices["pending"].outboxID); err != nil {
		t.Fatal(err)
	}

	notices["retry"] = newNotice("retrying legacy wait", 2)
	claimed, err := s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.ID != notices["retry"].outboxID {
		t.Fatalf("retry ClaimDelivery()=(%+v,%v), want outbox %d", claimed, err, notices["retry"].outboxID)
	}
	if err := s.UpdateDelivery(ctx, claimed.ID, "waiting", channel.Receipt{}, "rate limited", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	notices["sending"] = newNotice("sending legacy wait", 3)
	claimed, err = s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.ID != notices["sending"].outboxID {
		t.Fatalf("sending ClaimDelivery()=(%+v,%v), want outbox %d", claimed, err, notices["sending"].outboxID)
	}

	notices["sent"] = newNotice("sent legacy wait", 4)
	claimed, err = s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.ID != notices["sent"].outboxID {
		t.Fatalf("sent ClaimDelivery()=(%+v,%v), want outbox %d", claimed, err, notices["sent"].outboxID)
	}
	sentReceipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "legacy-sent"}
	if err := s.UpdateDelivery(ctx, claimed.ID, "sent", sentReceipt, "", time.Time{}); err != nil {
		t.Fatal(err)
	}

	notices["uncertain"] = newNotice("uncertain legacy wait", 5)
	claimed, err = s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.ID != notices["uncertain"].outboxID {
		t.Fatalf("uncertain ClaimDelivery()=(%+v,%v), want outbox %d", claimed, err, notices["uncertain"].outboxID)
	}
	uncertainReceipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner"}
	if err := s.UpdateDelivery(ctx, claimed.ID, "uncertain", uncertainReceipt, "remote outcome unknown", time.Time{}); err != nil {
		t.Fatal(err)
	}

	resultEvent := openUserEvent(t, s, 6, 6, "ordinary result")
	finishEvent(t, s, resultEvent, personal.TurnOutput{Text: "Unrelated result", Attachments: []string{}}, []string{"Unrelated result"})
	var resultDeliveryID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_delivery_intents WHERE event_id=? AND purpose='result'`, resultEvent.ID).Scan(&resultDeliveryID); err != nil {
		t.Fatal(err)
	}

	statusEvent := openUserEvent(t, s, 7, 7, "ordinary status")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	statusIntent, _, err := s.appendSupplementalDeliveryTx(ctx, tx, statusEvent.ID, personal.SupplementalReply{
		Purpose: personal.DeliveryStatus,
		Draft:   personal.DeliveryDraft{Text: "Unrelated status", Attachments: []string{}},
		Chunks:  []string{"Unrelated status"},
	})
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var statusOutboxID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_outbox WHERE delivery_id=?`, statusIntent.ID).Scan(&statusOutboxID); err != nil {
		t.Fatal(err)
	}

	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"pending", "retry"} {
		item := notices[name]
		var outboxStatus, outboxDetail, intentState, intentDetail string
		if err := s.db.QueryRowContext(ctx, `SELECT o.status,o.detail,i.state,i.detail FROM personal_outbox o
			JOIN personal_delivery_intents i ON i.id=o.delivery_id WHERE o.id=?`, item.outboxID).
			Scan(&outboxStatus, &outboxDetail, &intentState, &intentDetail); err != nil {
			t.Fatal(err)
		}
		if outboxStatus != "suppressed" || !strings.Contains(outboxDetail, deliverySuppressionWaitRetired) ||
			intentState != string(personal.DeliveryIntentSuppressed) || !strings.Contains(intentDetail, deliverySuppressionWaitRetired) {
			t.Fatalf("%s legacy wait after Recover: outbox=%q/%q intent=%q/%q", name, outboxStatus, outboxDetail, intentState, intentDetail)
		}
	}

	for name, wantStatus := range map[string]string{"sending": "uncertain", "sent": "sent", "uncertain": "uncertain"} {
		item := notices[name]
		var status, detail, receiptChannel, receiptAccount, receiptConversation, receiptMessageID string
		if err := s.db.QueryRowContext(ctx, `SELECT status,detail,receipt_channel,receipt_account,receipt_conversation,receipt_message_id
			FROM personal_outbox WHERE id=?`, item.outboxID).
			Scan(&status, &detail, &receiptChannel, &receiptAccount, &receiptConversation, &receiptMessageID); err != nil {
			t.Fatal(err)
		}
		if status != wantStatus {
			t.Fatalf("%s legacy wait status=%q, want %q", name, status, wantStatus)
		}
		if name == "sending" && detail != "outcome unknown after restart" {
			t.Fatalf("recovered sending wait detail=%q, want uncertain outcome", detail)
		}
		if name == "sent" && (receiptChannel != sentReceipt.Channel || receiptAccount != sentReceipt.Account ||
			receiptConversation != sentReceipt.Conversation || receiptMessageID != sentReceipt.MessageID) {
			t.Fatalf("sent receipt after Recover=(%q,%q,%q,%q), want %+v", receiptChannel, receiptAccount, receiptConversation, receiptMessageID, sentReceipt)
		}
		if name == "uncertain" && (receiptChannel != uncertainReceipt.Channel || receiptAccount != uncertainReceipt.Account ||
			receiptConversation != uncertainReceipt.Conversation || receiptMessageID != "") {
			t.Fatalf("uncertain receipt after Recover=(%q,%q,%q,%q), want %+v", receiptChannel, receiptAccount, receiptConversation, receiptMessageID, uncertainReceipt)
		}
	}

	for _, item := range notices {
		var waitNotified int
		var outboxID, historyID int64
		if err := s.db.QueryRowContext(ctx, `SELECT wait_notified,wait_outbox_id,wait_history_id FROM personal_reply_groups WHERE id=?`, item.eventID).
			Scan(&waitNotified, &outboxID, &historyID); err != nil {
			t.Fatal(err)
		}
		var historyDelivery int64
		var historyText string
		if err := s.db.QueryRowContext(ctx, `SELECT delivery_id,text FROM personal_history WHERE id=?`, historyID).Scan(&historyDelivery, &historyText); err != nil {
			t.Fatal(err)
		}
		if waitNotified != 1 || outboxID != item.outboxID || historyDelivery != item.deliveryID || historyText != "Legacy automatic waiting notice" {
			t.Fatalf("%s legacy wait references/history changed: notified=%d outbox=%d history=%d/%d text=%q", item.name, waitNotified, outboxID, historyDelivery, item.deliveryID, historyText)
		}
	}

	for label, outboxID := range map[string]int64{"unrelated result": func() int64 {
		var id int64
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_outbox WHERE delivery_id=?`, resultDeliveryID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}(), "unrelated status": statusOutboxID} {
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, outboxID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "pending" {
			t.Fatalf("%s outbox status=%q after Recover, want pending", label, status)
		}
	}
}

func TestCancelledTaskContinuationStartsNewReply(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := openUserEvent(t, s, 1, 1, "https://example.com/page")
	task, _, err := s.CreateTask(ctx, first.ID, "read", "阅读", personal.TaskInput{Goal: "阅读页面"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, first, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
	cancel := openUserEvent(t, s, 2, 2, "取消刚才的工作")
	if _, err := s.CancelTask(ctx, cancel.ID, task.ID, task.Revision); err != nil {
		t.Fatal(err)
	}
	// A reply about several cancelled tasks has no single output TaskID.
	finishEvent(t, s, cancel, personal.TurnOutput{Text: "已取消。"}, []string{"已取消。"})
	next := openUserEvent(t, s, 3, 3, "继续刚才的阅读")
	if _, _, err := s.ContinueTask(ctx, next.ID, "resume", task.ID, task.Revision, "继续读取页面", false); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, next, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
	var groupID int64
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, next.ID).Scan(&groupID); err != nil || groupID != next.ID {
		t.Fatalf("new request merged into cancelled reply: group=%d error=%v", groupID, err)
	}
}

func TestRecordReplySuppressesPendingWaitAndClosesGroup(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "帮我处理")
	legacyWaitFixture(t, s, event.ID)
	finishEvent(t, s, event, personal.TurnOutput{Text: "处理完成"}, []string{"处理完成"})
	var deliveries, histories int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&deliveries); err != nil || deliveries != 2 {
		t.Fatalf("outbox count = %d, %v", deliveries, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=? AND role='assistant'`, event.ID).Scan(&histories); err != nil || histories != 2 {
		t.Fatalf("assistant history count = %d, %v", histories, err)
	}
	var notified, hasContent, closed int
	var waitOutbox, waitHistory int64
	if err := s.db.QueryRowContext(ctx, `SELECT wait_notified,has_content,closed,COALESCE(wait_outbox_id,0),COALESCE(wait_history_id,0)
		FROM personal_reply_groups WHERE id=?`, event.ID).Scan(&notified, &hasContent, &closed, &waitOutbox, &waitHistory); err != nil {
		t.Fatal(err)
	}
	var waitStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, waitOutbox).Scan(&waitStatus); err != nil {
		t.Fatal(err)
	}
	if notified != 1 || hasContent != 1 || closed != 1 || waitOutbox == 0 || waitHistory == 0 || waitStatus != "suppressed" {
		t.Fatalf("reply group state = notified:%d content:%d closed:%d wait:%d/%d", notified, hasContent, closed, waitOutbox, waitHistory)
	}
}

func TestRecordReplyKeepsSendingWait(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "帮我处理")
	legacyWaitFixture(t, s, event.ID)
	waiting, err := s.ClaimDelivery(ctx)
	if err != nil || waiting == nil {
		t.Fatalf("ClaimDelivery() = (%+v,%v)", waiting, err)
	}
	finishEvent(t, s, event, personal.TurnOutput{Text: "处理完成"}, []string{"处理完成"})
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, waiting.ID).Scan(&status); err != nil || status != "sending" {
		t.Fatalf("sending wait status = %q, %v", status, err)
	}
}

func TestMergeReplyInputsKeepsOriginalEvents(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := openUserEvent(t, s, 1, 1, "第一条")
	second := openUserEvent(t, s, 2, 2, "补充")
	firstTask, _, err := s.CreateTask(ctx, first.ID, "create-first", "第一项", personal.TaskInput{Goal: "核对第一项"})
	if err != nil {
		t.Fatal(err)
	}
	secondTask, _, err := s.CreateTask(ctx, second.ID, "create-second", "第二项", personal.TaskInput{Goal: "核对第二项"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindReplyTaskTx(ctx, tx, first.ID, firstTask.ID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := bindReplyTaskTx(ctx, tx, second.ID, secondTask.ID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	legacyWaitFixture(t, s, first.ID)
	legacyWaitFixture(t, s, second.ID)
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mergeReplyInputsTx(ctx, tx, second.ID, []int64{first.ID}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var firstGroup, secondGroup int64
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, first.ID).Scan(&firstGroup); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, second.ID).Scan(&secondGroup); err != nil {
		t.Fatal(err)
	}
	var events, history, groups, tasks, waits int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_events WHERE id IN (?,?)`, first.ID, second.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id IN (?,?)`, first.ID, second.ID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_reply_groups WHERE id IN (?,?)`, first.ID, second.ID).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_reply_tasks WHERE group_id=?`, firstGroup).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id IN (?,?)`, first.ID, second.ID).Scan(&waits); err != nil {
		t.Fatal(err)
	}
	if firstGroup != secondGroup || firstGroup != first.ID || events != 2 || history != 4 || groups != 1 || tasks != 2 || waits != 2 {
		t.Fatalf("merge: groups=%d/%d events=%d history=%d reply groups=%d tasks=%d waits=%d", firstGroup, secondGroup, events, history, groups, tasks, waits)
	}
}

func TestMergeReplyGroupsKeepsSendingWaitForRetrySuppression(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := openUserEvent(t, s, 1, 1, "第一项")
	second := openUserEvent(t, s, 2, 2, "第二项")
	firstTask, _, err := s.CreateTask(ctx, first.ID, "create-first", "第一项", personal.TaskInput{Goal: "处理第一项"})
	if err != nil {
		t.Fatal(err)
	}
	secondTask, _, err := s.CreateTask(ctx, second.ID, "create-second", "第二项", personal.TaskInput{Goal: "处理第二项"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindReplyTaskTx(ctx, tx, first.ID, firstTask.ID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := bindReplyTaskTx(ctx, tx, second.ID, secondTask.ID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	legacyWaitFixture(t, s, first.ID)
	legacyWaitFixture(t, s, second.ID)
	firstWait, err := s.ClaimDelivery(ctx)
	if err != nil || firstWait == nil || firstWait.EventID != first.ID {
		t.Fatalf("first ClaimDelivery() = (%+v,%v)", firstWait, err)
	}
	if err := s.UpdateDelivery(ctx, firstWait.ID, "sent", channel.Receipt{MessageID: "sent"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	secondWait, err := s.ClaimDelivery(ctx)
	if err != nil || secondWait == nil || secondWait.EventID != second.ID {
		t.Fatalf("second ClaimDelivery() = (%+v,%v)", secondWait, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE personal_reply_groups SET has_content=1 WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mergeReplyInputsTx(ctx, tx, second.ID, []int64{first.ID}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var preservedWait int64
	if err := s.db.QueryRowContext(ctx, `SELECT wait_outbox_id FROM personal_reply_groups WHERE id=?`, first.ID).Scan(&preservedWait); err != nil || preservedWait != secondWait.ID {
		t.Fatalf("merged wait outbox = %d, %v; want sending delivery %d", preservedWait, err, secondWait.ID)
	}
	if err := s.UpdateDelivery(ctx, secondWait.ID, "waiting", channel.Receipt{}, "retry later", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var retryRows, sentRows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE id=? AND status='suppressed'`, secondWait.ID).Scan(&retryRows); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE id=? AND status='sent'`, firstWait.ID).Scan(&sentRows); err != nil {
		t.Fatal(err)
	}
	if retryRows != 1 || sentRows != 1 {
		t.Fatalf("merged wait rows: retry=%d sent=%d", retryRows, sentRows)
	}
}

func TestBindReplyTaskMergesGroupsAndPreservesInputs(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := openUserEvent(t, s, 1, 1, "第一条")
	second := openUserEvent(t, s, 2, 2, "补充")
	task, _, err := s.CreateTask(ctx, first.ID, "create", "研究", personal.TaskInput{Goal: "核对材料"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindReplyTaskTx(ctx, tx, first.ID, task.ID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := bindReplyTaskTx(ctx, tx, second.ID, task.ID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var firstGroup, secondGroup int64
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, first.ID).Scan(&firstGroup); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, second.ID).Scan(&secondGroup); err != nil {
		t.Fatal(err)
	}
	var tasks, events, history int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_reply_tasks WHERE group_id=? AND task_id=?`, firstGroup, task.ID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_events WHERE id IN (?,?)`, first.ID, second.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id IN (?,?)`, first.ID, second.ID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if firstGroup != secondGroup || firstGroup != first.ID || tasks != 1 || events != 2 || history != 2 {
		t.Fatalf("binding: groups=%d/%d tasks=%d events=%d history=%d", firstGroup, secondGroup, tasks, events, history)
	}
}

func TestGetTaskReplyDoesNotKeepInquiryGroupOpen(t *testing.T) {
	for _, waitQueued := range []bool{false, true} {
		t.Run(map[bool]string{false: "before wait", true: "pending wait"}[waitQueued], func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			taskEvent := openUserEvent(t, s, 1, 1, "请研究")
			task, _, err := s.CreateTask(ctx, taskEvent.ID, "create", "研究", personal.TaskInput{Goal: "核对资料"})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := bindReplyTaskTx(ctx, tx, taskEvent.ID, task.ID); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, taskEvent, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
			if waitQueued {
				legacyWaitFixture(t, s, taskEvent.ID)
			}
			inquiry := openUserEvent(t, s, 2, 2, "目前确认了什么？")
			answer := "已确认资料中的关键进展，我还会继续核对。"
			finishEvent(t, s, inquiry, personal.TurnOutput{TaskID: task.ID, Text: answer}, []string{answer})
			var inquiryGroupClosed, originalGroupContent, originalGroupClosed, inquiryTaskLinks int
			if err := s.db.QueryRowContext(ctx, `SELECT closed FROM personal_reply_groups WHERE id=?`, inquiry.ID).Scan(&inquiryGroupClosed); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT has_content,closed FROM personal_reply_groups WHERE id=?`, taskEvent.ID).Scan(&originalGroupContent, &originalGroupClosed); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_reply_tasks WHERE group_id=?`, inquiry.ID).Scan(&inquiryTaskLinks); err != nil {
				t.Fatal(err)
			}
			if inquiryGroupClosed != 1 || originalGroupContent != 1 || originalGroupClosed != 0 || inquiryTaskLinks != 0 {
				t.Fatalf("reply groups: inquiry closed=%d, original content=%d closed=%d, inquiry task links=%d", inquiryGroupClosed, originalGroupContent, originalGroupClosed, inquiryTaskLinks)
			}
			if waitQueued {
				var waits, histories, suppressed int
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, taskEvent.ID).Scan(&waits); err != nil {
					t.Fatal(err)
				}
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=? AND status='suppressed'`, taskEvent.ID).Scan(&suppressed); err != nil {
					t.Fatal(err)
				}
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=? AND role='assistant'`, taskEvent.ID).Scan(&histories); err != nil {
					t.Fatal(err)
				}
				if waits != 1 || histories != 1 || suppressed != 1 {
					t.Fatalf("pending wait after progress reply: outbox=%d history=%d", waits, histories)
				}
			}
			cancelEvent := openUserEvent(t, s, 3, 3, "取消研究")
			if _, err := s.CancelTask(ctx, cancelEvent.ID, task.ID, task.Revision); err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, cancelEvent, personal.TurnOutput{TaskID: task.ID, Text: "已取消。"}, []string{"已取消。"})
			if err := s.db.QueryRowContext(ctx, `SELECT closed FROM personal_reply_groups WHERE id=?`, taskEvent.ID).Scan(&originalGroupClosed); err != nil {
				t.Fatal(err)
			}
			if originalGroupClosed != 1 {
				t.Fatalf("original task group closed=%d after task cancellation, want 1", originalGroupClosed)
			}
		})
	}
}

func TestCanDeferReplyRequiresAutonomousTaskAndNoMemoryMutation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "请研究并记住：我偏好红茶")
	if allowed, err := s.CanDeferReply(ctx, event.ID); err != nil || allowed {
		t.Fatalf("without task CanDeferReply() = (%v,%v), want false", allowed, err)
	}
	task, _, err := s.CreateTask(ctx, event.ID, "create", "研究", personal.TaskInput{Goal: "核对资料"})
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.CanDeferReply(ctx, event.ID); err != nil || !allowed {
		t.Fatalf("queued task CanDeferReply() = (%v,%v), want true", allowed, err)
	}
	if _, err := s.WriteMemory(ctx, event.ID, "remember", personal.MemoryChange{Operation: "put", Key: "tea", Value: "红茶", Evidence: "我偏好红茶"}); err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.CanDeferReply(ctx, event.ID); err != nil || allowed {
		t.Fatalf("with memory mutation CanDeferReply() = (%v,%v), want false", allowed, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE personal_tasks SET status='waiting_input' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.CanDeferReply(ctx, event.ID); err != nil || allowed {
		t.Fatalf("non-autonomous task CanDeferReply() = (%v,%v), want false", allowed, err)
	}
}

func TestSuppressStaleWaitAfterTaskStops(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "请研究")
	task, _, err := s.CreateTask(ctx, event.ID, "create", "研究", personal.TaskInput{Goal: "核对材料"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindReplyTaskTx(ctx, tx, event.ID, task.ID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	legacyWaitFixture(t, s, event.ID)
	if _, err := s.db.ExecContext(ctx, `UPDATE personal_tasks SET status='cancelled' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := suppressStaleWaitRepliesTx(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var waits, assistantHistory, notified int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&waits); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=? AND role='assistant'`, event.ID).Scan(&assistantHistory); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT wait_notified FROM personal_reply_groups WHERE id=?`, event.ID).Scan(&notified); err != nil {
		t.Fatal(err)
	}
	var waitStatus string
	var waitOutbox, waitHistory int64
	if err := s.db.QueryRowContext(ctx, `SELECT wait_outbox_id,wait_history_id FROM personal_reply_groups WHERE id=?`, event.ID).Scan(&waitOutbox, &waitHistory); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, waitOutbox).Scan(&waitStatus); err != nil {
		t.Fatal(err)
	}
	if waits != 1 || assistantHistory != 1 || notified != 1 || waitOutbox == 0 || waitHistory == 0 || waitStatus != "suppressed" {
		t.Fatalf("stale wait: outbox=%d history=%d wait_notified=%d", waits, assistantHistory, notified)
	}
}

func TestAppendDeliveryPartsUsesNextPart(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "你好")
	for i, chunks := range [][]string{{"第一段", "第二段"}, {" ", "第三段"}} {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := insertDeliveryIntentTx(ctx, tx, deliveryIntentInsert{
			eventID: event.ID, purpose: personal.DeliveryResult,
			draft: personal.DeliveryDraft{Text: "完整回复", Attachments: []string{}},
		}, time.Now().UTC())
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		part, err := appendDeliveryPartsTx(ctx, tx, event.ID, intent.ID, "完整回复", nil, chunks, 0, 0)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := updateDeliveryIntentRenderedTx(ctx, tx, intent.ID, ""); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		want := 2
		if i == 1 {
			want = 3
		}
		if part != want {
			t.Fatalf("append %d returned part %d, want %d", i, part, want)
		}
	}
	var parts, histories int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&parts); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=? AND role='assistant'`, event.ID).Scan(&histories); err != nil {
		t.Fatal(err)
	}
	if parts != 3 || histories != 2 {
		t.Fatalf("outbox parts=%d assistant history=%d, want 3 and 2", parts, histories)
	}
}
