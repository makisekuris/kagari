package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func TestQueueWaitingReplyDeliveryScopesAllLinkedTasks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "请同时处理两项")
	first, firstRun, err := s.CreateTask(ctx, event.ID, "first", "第一项", personal.TaskInput{Goal: "核对第一项"})
	if err != nil {
		t.Fatal(err)
	}
	second, secondRun, err := s.CreateTask(ctx, event.ID, "second", "第二项", personal.TaskInput{Goal: "核对第二项"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
	if queued, err := s.QueueWaitingReply(ctx, event.CreatedAt.Add(time.Minute), 30*time.Second); err != nil || !queued {
		t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
	}

	var deliveryID int64
	var purpose personal.DeliveryPurpose
	var state personal.DeliveryIntentState
	var draft string
	if err := s.db.QueryRowContext(ctx, `SELECT i.id,i.purpose,i.state,i.draft_text FROM personal_delivery_intents i
		JOIN personal_outbox d ON d.delivery_id=i.id WHERE d.event_id=?`, event.ID).Scan(&deliveryID, &purpose, &state, &draft); err != nil {
		t.Fatal(err)
	}
	if purpose != personal.DeliveryStatus || state != personal.DeliveryIntentRendered || draft != "任务已排队，有结果后我会继续回复。" {
		t.Fatalf("wait intent = (%q,%q,%q), want rendered status draft", purpose, state, draft)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT task_id,revision,COALESCE(run_id,0) FROM personal_delivery_scopes WHERE delivery_id=? ORDER BY task_id`, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	type scope struct{ taskID, revision, runID int64 }
	var scopes []scope
	for rows.Next() {
		var item scope
		if err := rows.Scan(&item.taskID, &item.revision, &item.runID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		scopes = append(scopes, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 || scopes[0] != (scope{first.ID, first.Revision, firstRun.ID}) || scopes[1] != (scope{second.ID, second.Revision, secondRun.ID}) {
		t.Fatalf("wait scopes = %+v, want both linked task revisions and runs", scopes)
	}
	var historyDeliveryID, outboxDeliveryID int64
	if err := s.db.QueryRowContext(ctx, `SELECT delivery_id FROM personal_history WHERE event_id=? AND role='assistant'`, event.ID).Scan(&historyDeliveryID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT delivery_id FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&outboxDeliveryID); err != nil {
		t.Fatal(err)
	}
	if historyDeliveryID != deliveryID || outboxDeliveryID != deliveryID {
		t.Fatalf("wait delivery links: intent=%d history=%d outbox=%d", deliveryID, historyDeliveryID, outboxDeliveryID)
	}
}

func TestPendingWaitSuppressionPreservesDeliveryChildrenAndStartedWait(t *testing.T) {
	for _, finalStatus := range []string{"pending", "sending", "sent", "uncertain"} {
		t.Run(finalStatus, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			event := openUserEvent(t, s, 1, 1, "请研究")
			task, _, err := s.CreateTask(ctx, event.ID, "create", "研究", personal.TaskInput{Goal: "核对材料"})
			if err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
			if queued, err := s.QueueWaitingReply(ctx, event.CreatedAt.Add(time.Minute), 30*time.Second); err != nil || !queued {
				t.Fatalf("QueueWaitingReply() = (%v,%v), want true", queued, err)
			}
			var deliveryID, outboxID int64
			if err := s.db.QueryRowContext(ctx, `SELECT delivery_id,id FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&deliveryID, &outboxID); err != nil {
				t.Fatal(err)
			}
			if finalStatus != "pending" {
				if _, err := s.db.ExecContext(ctx, `UPDATE personal_outbox SET next_attempt_at=? WHERE id=?`, timestamp(time.Now().Add(-time.Second)), outboxID); err != nil {
					t.Fatal(err)
				}
				claimed, err := s.ClaimDelivery(ctx)
				if err != nil || claimed == nil || claimed.ID != outboxID {
					t.Fatalf("ClaimDelivery() = (%+v,%v)", claimed, err)
				}
				if finalStatus == "sent" {
					receipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "wait-receipt"}
					if err := s.UpdateDelivery(ctx, outboxID, "sent", receipt, "", time.Time{}); err != nil {
						t.Fatal(err)
					}
				} else if finalStatus == "uncertain" {
					if err := s.UpdateDelivery(ctx, outboxID, "uncertain", channel.Receipt{}, "remote outcome unknown", time.Time{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE personal_tasks SET status='cancelled' WHERE id=?`, task.ID); err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, nil)
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

			var intents, outbox, history, scopes int
			for query, target := range map[string]*int{
				`SELECT COUNT(*) FROM personal_delivery_intents WHERE id=?`:         &intents,
				`SELECT COUNT(*) FROM personal_outbox WHERE delivery_id=?`:          &outbox,
				`SELECT COUNT(*) FROM personal_history WHERE delivery_id=?`:         &history,
				`SELECT COUNT(*) FROM personal_delivery_scopes WHERE delivery_id=?`: &scopes,
			} {
				if err := s.db.QueryRowContext(ctx, query, deliveryID).Scan(target); err != nil {
					t.Fatal(err)
				}
			}
			var waitOutbox, waitHistory sql.NullInt64
			if err := s.db.QueryRowContext(ctx, `SELECT wait_outbox_id,wait_history_id FROM personal_reply_groups WHERE id=?`, event.ID).Scan(&waitOutbox, &waitHistory); err != nil {
				t.Fatal(err)
			}
			var status string
			var state personal.DeliveryIntentState
			var detail string
			if err := s.db.QueryRowContext(ctx, `SELECT i.state,i.detail,d.status FROM personal_delivery_intents i
				JOIN personal_outbox d ON d.delivery_id=i.id WHERE i.id=?`, deliveryID).Scan(&state, &detail, &status); err != nil {
				t.Fatal(err)
			}
			if finalStatus != "pending" {
				if intents != 1 || outbox != 1 || history != 1 || scopes != 1 || status != finalStatus || state != personal.DeliveryIntentRendered || !waitOutbox.Valid || !waitHistory.Valid {
					t.Fatalf("started wait changed: intent/outbox/history/scopes=%d/%d/%d/%d state=%q status=%q pointers=%v/%v", intents, outbox, history, scopes, state, status, waitOutbox, waitHistory)
				}
			} else if intents != 1 || outbox != 1 || history != 1 || scopes != 1 || status != "suppressed" || state != personal.DeliveryIntentSuppressed || !strings.Contains(detail, "reply_wait_superseded") || !waitOutbox.Valid || !waitHistory.Valid {
				t.Fatalf("pending wait was not durably suppressed: intent/outbox/history/scopes=%d/%d/%d/%d state=%q status=%q detail=%q pointers=%v/%v", intents, outbox, history, scopes, state, status, detail, waitOutbox, waitHistory)
			}
		})
	}
}

func TestSchedulePauseNoticeUsesControlDelivery(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	schedule := dispatchTestSchedule(t, s, personal.WindowNone)
	if _, err := s.db.ExecContext(ctx, `UPDATE personal_schedules SET cron='not a cron expression' WHERE id=?`, schedule.ID); err != nil {
		t.Fatal(err)
	}
	if occurrence, err := s.DispatchDueSchedule(ctx, *schedule.NextRunAt); err != nil || occurrence == nil || occurrence.Outcome != "blocked" {
		t.Fatalf("DispatchDueSchedule() = (%+v,%v), want blocked", occurrence, err)
	}
	var eventID, deliveryID int64
	var purpose personal.DeliveryPurpose
	var intentScheduleID int64
	if err := s.db.QueryRowContext(ctx, `SELECT e.id,i.id,i.purpose,i.schedule_id FROM personal_events e
		JOIN personal_delivery_intents i ON i.event_id=e.id WHERE i.schedule_id=?`, schedule.ID).
		Scan(&eventID, &deliveryID, &purpose, &intentScheduleID); err != nil {
		t.Fatal(err)
	}
	if purpose != personal.DeliveryControl || intentScheduleID != schedule.ID {
		t.Fatalf("pause delivery = purpose %q schedule %d", purpose, intentScheduleID)
	}
	var historyID int64
	var historyText, draft string
	if err := s.db.QueryRowContext(ctx, `SELECT h.id,h.text,i.draft_text FROM personal_history h
		JOIN personal_delivery_intents i ON i.id=h.delivery_id WHERE h.event_id=? AND h.delivery_id=?`, eventID, deliveryID).
		Scan(&historyID, &historyText, &draft); err != nil {
		t.Fatal(err)
	}
	if historyID == 0 || historyText != draft {
		t.Fatalf("pause history/draft = %q/%q", historyText, draft)
	}
	var linked int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=? AND delivery_id=?`, eventID, deliveryID).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("pause delivery outbox parts = %d, %v", linked, err)
	}
}

func TestScheduleSuggestionNoticeHasItsOwnControlHistory(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, personal.WindowNone)
	callback := scheduleSuggestionCallback(t, s, schedule.ID)
	suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "notice", callback.TaskID, scheduleSuggestionProposal(schedule, memory), scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishScheduleSuggestionCallback(t, s, callback, false)

	var deliveryID int64
	var purpose personal.DeliveryPurpose
	var intentScheduleID int64
	var state personal.DeliveryIntentState
	var draft string
	if err := s.db.QueryRowContext(ctx, `SELECT i.id,i.purpose,i.schedule_id,i.state,i.draft_text
		FROM personal_delivery_intents i JOIN personal_outbox d ON d.delivery_id=i.id
		WHERE d.suggestion_id=? LIMIT 1`, suggestion.ID).Scan(&deliveryID, &purpose, &intentScheduleID, &state, &draft); err != nil {
		t.Fatal(err)
	}
	if purpose != personal.DeliveryControl || intentScheduleID != schedule.ID || state != personal.DeliveryIntentRendered || !strings.Contains(draft, "建议 #") {
		t.Fatalf("suggestion intent = (%q,%d,%q,%q)", purpose, intentScheduleID, state, draft)
	}
	var histories, parts, badCombined int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE delivery_id=?`, deliveryID).Scan(&histories); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE delivery_id=? AND suggestion_id=?`, deliveryID, suggestion.ID).Scan(&parts); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=? AND delivery_id<>? AND instr(text,'建议 #')>0`, callback.ID, deliveryID).Scan(&badCombined); err != nil {
		t.Fatal(err)
	}
	if histories != 1 || parts == 0 || badCombined != 0 {
		t.Fatalf("suggestion delivery: histories=%d parts=%d combined_notice_histories=%d", histories, parts, badCombined)
	}
}
