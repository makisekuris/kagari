package personal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/channel"
)

func TestQueueWaitingReplyThresholdAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "replies.db")
	s := testStore(t, path, time.Minute)
	event := openUserEvent(t, s, 1, 1, "帮我处理这件事")
	due := event.CreatedAt.Add(30 * time.Second)
	if queued, err := s.QueueWaitingReply(ctx, due.Add(-time.Second), 30*time.Second); err != nil || queued {
		t.Fatalf("before delay QueueWaitingReply() = (%v,%v), want false", queued, err)
	}
	if queued, err := s.QueueWaitingReply(ctx, due, 30*time.Second); err != nil || !queued {
		t.Fatalf("at delay QueueWaitingReply() = (%v,%v), want true", queued, err)
	}
	var message string
	if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&message); err != nil || message != "我正在处理这条消息，稍后回复。" {
		t.Fatalf("waiting message = %q, %v", message, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, path, time.Minute)
	if queued, err := s.QueueWaitingReply(ctx, due.Add(time.Minute), 30*time.Second); err != nil || queued {
		t.Fatalf("after restart QueueWaitingReply() = (%v,%v), want false", queued, err)
	}
	var reminders int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&reminders); err != nil || reminders != 1 {
		t.Fatalf("reminder count after restart = %d, %v", reminders, err)
	}
}

func TestCancelledTaskContinuationStartsNewReply(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := openUserEvent(t, s, 1, 1, "https://example.com/page")
	task, _, err := s.CreateTask(ctx, first.ID, "read", "阅读", TaskInput{Goal: "阅读页面"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, first, TurnOutput{TaskID: task.ID, Silent: true}, nil)
	cancel := openUserEvent(t, s, 2, 2, "取消刚才的工作")
	if _, err := s.CancelTask(ctx, cancel.ID, task.ID, task.Revision); err != nil {
		t.Fatal(err)
	}
	// A reply about several cancelled tasks has no single output TaskID.
	finishEvent(t, s, cancel, TurnOutput{Text: "已取消。"}, []string{"已取消。"})
	next := openUserEvent(t, s, 3, 3, "继续刚才的阅读")
	if _, _, err := s.ContinueTask(ctx, next.ID, "resume", task.ID, task.Revision, "继续读取页面", false); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, next, TurnOutput{TaskID: task.ID, Silent: true}, nil)
	var groupID int64
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, next.ID).Scan(&groupID); err != nil || groupID != next.ID {
		t.Fatalf("new request merged into cancelled reply: group=%d error=%v", groupID, err)
	}
}

func TestQueueWaitingReplyOnlyHintsEarliestUnclassifiedInput(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := openUserEvent(t, s, 1, 1, "第一条")
	secondID := acceptInput(t, s, "2", sourceInput("2", "第二条"))
	now := first.CreatedAt.Add(time.Minute)
	if queued, err := s.QueueWaitingReply(ctx, now, 30*time.Second); err != nil || !queued {
		t.Fatalf("first QueueWaitingReply() = (%v,%v), want true", queued, err)
	}
	if queued, err := s.QueueWaitingReply(ctx, now.Add(time.Minute), 30*time.Second); err != nil || queued {
		t.Fatalf("second QueueWaitingReply() = (%v,%v), want false", queued, err)
	}
	var reminders int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id IN (?,?)`, first.ID, secondID).Scan(&reminders); err != nil || reminders != 1 {
		t.Fatalf("reminder count = %d, %v", reminders, err)
	}
}

func TestQueueWaitingReplyDescribesQueuedUnclassifiedInput(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	id := acceptInput(t, s, "1", sourceInput("1", "稍后处理"))
	if queued, err := s.QueueWaitingReply(ctx, time.Now().Add(time.Minute), 30*time.Second); err != nil || !queued {
		t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
	}
	var message string
	if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_outbox WHERE event_id=?`, id).Scan(&message); err != nil || message != "消息已收到，正在等候处理。" {
		t.Fatalf("waiting message = %q, %v", message, err)
	}
}

func TestQueueWaitingReplyUsesTaskState(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "请做一份研究")
	task, _, err := s.CreateTask(ctx, event.ID, "create", "研究", TaskInput{Goal: "核对材料"})
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
	if queued, err := s.QueueWaitingReply(ctx, time.Now(), 0); err != nil || !queued {
		t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
	}
	var message string
	if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&message); err != nil || message != "「研究」已排队，有结果后我会继续回复。" {
		t.Fatalf("waiting message = %q, %v", message, err)
	}
}

func TestRecordReplyRemovesPendingWaitAndClosesGroup(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "帮我处理")
	if queued, err := s.QueueWaitingReply(ctx, event.CreatedAt.Add(time.Minute), 30*time.Second); err != nil || !queued {
		t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
	}
	finishEvent(t, s, event, TurnOutput{Text: "处理完成"}, []string{"处理完成"})
	var deliveries, histories int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("outbox count = %d, %v", deliveries, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=? AND role='assistant'`, event.ID).Scan(&histories); err != nil || histories != 1 {
		t.Fatalf("assistant history count = %d, %v", histories, err)
	}
	var notified, hasContent, closed int
	var waitOutbox, waitHistory int64
	if err := s.db.QueryRowContext(ctx, `SELECT wait_notified,has_content,closed,COALESCE(wait_outbox_id,0),COALESCE(wait_history_id,0)
		FROM personal_reply_groups WHERE id=?`, event.ID).Scan(&notified, &hasContent, &closed, &waitOutbox, &waitHistory); err != nil {
		t.Fatal(err)
	}
	if notified != 1 || hasContent != 1 || closed != 1 || waitOutbox != 0 || waitHistory != 0 {
		t.Fatalf("reply group state = notified:%d content:%d closed:%d wait:%d/%d", notified, hasContent, closed, waitOutbox, waitHistory)
	}
}

func TestRecordReplyKeepsSendingWait(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "帮我处理")
	if queued, err := s.QueueWaitingReply(ctx, time.Now(), 0); err != nil || !queued {
		t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
	}
	waiting, err := s.ClaimDelivery(ctx)
	if err != nil || waiting == nil {
		t.Fatalf("ClaimDelivery() = (%+v,%v)", waiting, err)
	}
	finishEvent(t, s, event, TurnOutput{Text: "处理完成"}, []string{"处理完成"})
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
	firstTask, _, err := s.CreateTask(ctx, first.ID, "create-first", "第一项", TaskInput{Goal: "核对第一项"})
	if err != nil {
		t.Fatal(err)
	}
	secondTask, _, err := s.CreateTask(ctx, second.ID, "create-second", "第二项", TaskInput{Goal: "核对第二项"})
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
	for range 2 {
		if queued, err := s.QueueWaitingReply(ctx, second.CreatedAt.Add(time.Minute), 30*time.Second); err != nil || !queued {
			t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
		}
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
	if firstGroup != secondGroup || firstGroup != first.ID || events != 2 || history != 3 || groups != 1 || tasks != 2 || waits != 1 {
		t.Fatalf("merge: groups=%d/%d events=%d history=%d reply groups=%d tasks=%d waits=%d", firstGroup, secondGroup, events, history, groups, tasks, waits)
	}
}

func TestMergeReplyGroupsKeepsSendingWaitForRetrySuppression(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := openUserEvent(t, s, 1, 1, "第一项")
	second := openUserEvent(t, s, 2, 2, "第二项")
	firstTask, _, err := s.CreateTask(ctx, first.ID, "create-first", "第一项", TaskInput{Goal: "处理第一项"})
	if err != nil {
		t.Fatal(err)
	}
	secondTask, _, err := s.CreateTask(ctx, second.ID, "create-second", "第二项", TaskInput{Goal: "处理第二项"})
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
	for range 2 {
		if queued, err := s.QueueWaitingReply(ctx, time.Now(), 0); err != nil || !queued {
			t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
		}
	}
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
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE id=?`, secondWait.ID).Scan(&retryRows); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE id=? AND status='sent'`, firstWait.ID).Scan(&sentRows); err != nil {
		t.Fatal(err)
	}
	if retryRows != 0 || sentRows != 1 {
		t.Fatalf("merged wait rows: retry=%d sent=%d", retryRows, sentRows)
	}
}

func TestBindReplyTaskMergesGroupsAndPreservesInputs(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := openUserEvent(t, s, 1, 1, "第一条")
	second := openUserEvent(t, s, 2, 2, "补充")
	task, _, err := s.CreateTask(ctx, first.ID, "create", "研究", TaskInput{Goal: "核对材料"})
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
			task, _, err := s.CreateTask(ctx, taskEvent.ID, "create", "研究", TaskInput{Goal: "核对资料"})
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
			finishEvent(t, s, taskEvent, TurnOutput{TaskID: task.ID, Silent: true}, nil)
			if waitQueued {
				if queued, err := s.QueueWaitingReply(ctx, taskEvent.CreatedAt.Add(time.Minute), 30*time.Second); err != nil || !queued {
					t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
				}
			}
			inquiry := openUserEvent(t, s, 2, 2, "目前确认了什么？")
			answer := "已确认资料中的关键进展，我还会继续核对。"
			finishEvent(t, s, inquiry, TurnOutput{TaskID: task.ID, Text: answer}, []string{answer})
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
				var waits, histories int
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, taskEvent.ID).Scan(&waits); err != nil {
					t.Fatal(err)
				}
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=? AND role='assistant'`, taskEvent.ID).Scan(&histories); err != nil {
					t.Fatal(err)
				}
				if waits != 0 || histories != 0 {
					t.Fatalf("pending wait after progress reply: outbox=%d history=%d", waits, histories)
				}
			} else if queued, err := s.QueueWaitingReply(ctx, taskEvent.CreatedAt.Add(time.Minute), 30*time.Second); err != nil || queued {
				t.Fatalf("QueueWaitingReply() after progress reply = (%v,%v), want false", queued, err)
			}
			cancelEvent := openUserEvent(t, s, 3, 3, "取消研究")
			if _, err := s.CancelTask(ctx, cancelEvent.ID, task.ID, task.Revision); err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, cancelEvent, TurnOutput{TaskID: task.ID, Text: "已取消。"}, []string{"已取消。"})
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
	task, _, err := s.CreateTask(ctx, event.ID, "create", "研究", TaskInput{Goal: "核对资料"})
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.CanDeferReply(ctx, event.ID); err != nil || !allowed {
		t.Fatalf("queued task CanDeferReply() = (%v,%v), want true", allowed, err)
	}
	if _, err := s.WriteMemory(ctx, event.ID, "remember", MemoryChange{Operation: "put", Key: "tea", Value: "红茶", Evidence: "我偏好红茶"}); err != nil {
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
	task, _, err := s.CreateTask(ctx, event.ID, "create", "研究", TaskInput{Goal: "核对材料"})
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
	if queued, err := s.QueueWaitingReply(ctx, event.CreatedAt.Add(time.Minute), 30*time.Second); err != nil || !queued {
		t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
	}
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
	if waits != 0 || assistantHistory != 0 || notified != 1 {
		t.Fatalf("stale wait: outbox=%d history=%d wait_notified=%d", waits, assistantHistory, notified)
	}
}

func TestAppendReplyPartsUsesNextPart(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "你好")
	for i, chunks := range [][]string{{"第一段", "第二段"}, {" ", "第三段"}} {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		part, err := appendReplyPartsTx(ctx, tx, event.ID, "完整回复", nil, chunks, 0)
		if err != nil {
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
