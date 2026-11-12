package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"kagari/internal/logging"
	"kagari/internal/personal"
)

// Event completion records replies, schedules follow-up work, and recovers interrupted state.
func (s *Store) CompleteEvent(ctx context.Context, eventID int64, output personal.TurnOutput, chunks []string, prepared ...personal.RenderedDelivery) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind personal.EventKind
	var status personal.EventStatus
	if err := tx.QueryRowContext(ctx, `SELECT kind,status FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &status); err != nil {
		return err
	}
	if status != personal.EventRunning {
		return fmt.Errorf("personal: cannot complete event %d in state %q", eventID, status)
	}
	intents, err := loadPendingSettlementIntentsTx(ctx, tx, eventID)
	if err != nil {
		return err
	}
	rendered, err := validatePreparedDeliveries(intents, prepared)
	if err != nil {
		return err
	}
	discardedStaleCallback := false
	var callbackTaskID, callbackRunID, callbackRevision int64
	if kind == personal.EventTask {
		state, identity, err := taskCallbackStateTx(ctx, tx, eventID)
		if err != nil {
			return err
		}
		callbackTaskID, callbackRunID = identity.taskID, identity.runID
		callbackRevision = identity.revision
		if state == personal.TaskCallbackStale {
			output, chunks = personal.TurnOutput{Silent: true}, nil
			discardedStaleCallback = true
		} else if len(intents) == 0 && output.TaskID != 0 && output.TaskID != callbackTaskID {
			return errors.New("personal: task callback cannot link its reply to another task")
		}
	}
	taskID := int64(0)
	if kind == personal.EventTask {
		taskID = callbackTaskID
	} else if len(intents) > 0 {
		if one, ok := oneDeliveryTask(intents); ok {
			taskID = one
		}
	} else {
		taskID = output.TaskID
	}
	if taskID != 0 {
		if _, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID)); err != nil {
			return fmt.Errorf("personal: event task %d: %w", taskID, err)
		}
	}
	scheduleID := int64(0)
	if kind == personal.EventTask {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(schedule_id,0) FROM personal_events WHERE id=?`, eventID).Scan(&scheduleID); err != nil {
			return err
		}
	} else if len(intents) > 0 {
		if one, ok := oneDeliverySchedule(intents); ok {
			scheduleID = one
		}
	} else {
		scheduleID = output.ScheduleID
	}
	if len(intents) == 0 && kind != personal.EventTask && taskID > 0 && output.TaskID > 0 {
		var linked sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT schedule_id FROM personal_schedule_occurrences WHERE task_id=?`, taskID).Scan(&linked)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if scheduleID != 0 && scheduleID != linked.Int64 {
			return errors.New("personal: reply schedule does not match its task")
		}
		scheduleID = linked.Int64
	}
	if len(intents) > 0 {
		if err := recordSettlementRepliesTx(ctx, tx, eventID, intents, taskID); err != nil {
			return err
		}
	} else if !discardedStaleCallback {
		if err := recordReplyTx(ctx, tx, eventID, output); err != nil {
			return err
		}
	}
	persistedTaskID := nullableID(taskID)
	if kind == personal.EventTask && callbackTaskID > 0 {
		// Keep the callback's source task available for diagnostics even when its
		// output is discarded or intentionally carries no task link.
		persistedTaskID = callbackTaskID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_events SET status='completed',task_id=?,schedule_id=? WHERE id=? AND status='running'`, persistedTaskID, nullableID(scheduleID), eventID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_history SET schedule_id=? WHERE event_id=? AND role<>'assistant'`, nullableID(scheduleID), eventID); err != nil {
		return err
	}
	var notices []scheduleNotice
	hasDelivery := len(intents) > 0 || !output.Silent && (output.Text != "" || len(output.Attachments) > 0)
	deliveryCount := len(intents)
	if (kind == personal.EventTask || kind == personal.EventUser) && hasDelivery && !discardedStaleCallback {
		notices, err = scheduleNotices(ctx, tx, eventID, kind)
		if err != nil {
			return err
		}
	}
	replyParts := 0
	if len(intents) > 0 {
		for _, intent := range intents {
			render := rendered[intent.ID]
			attachments, detail, err := s.prepareIntentAttachmentsTx(ctx, tx, intent, render.AttachmentFailure)
			if err != nil {
				return err
			}
			if err := validatePreparedText(fmt.Sprintf("delivery %d", intent.ID), render.Text, render.Chunks); err != nil {
				return err
			}
			if render.Text == "" && len(attachments) == 0 {
				return fmt.Errorf("personal: rendered delivery %d has no content", intent.ID)
			}
			lastPart, err := appendDeliveryPartsTx(ctx, tx, eventID, intent.ID, render.Text, attachments, render.Chunks, intent.ScheduleID, 0)
			if err != nil {
				return err
			}
			replyParts = lastPart
			if err := updateDeliveryIntentRenderedTx(ctx, tx, intent.ID, detail); err != nil {
				return err
			}
		}
	} else if !output.Silent && (output.Text != "" || len(output.Attachments) > 0) {
		purpose := output.Purpose
		if purpose == "" {
			purpose = personal.DeliveryResult
		}
		if !validDeliveryPurpose(purpose) {
			return fmt.Errorf("personal: invalid delivery purpose %q", purpose)
		}
		if err := validatePreparedText("reply", output.Text, chunks); err != nil {
			return err
		}
		attachments, err := s.validateOrdinaryAttachmentsTx(ctx, tx, output.Attachments)
		if err != nil {
			return err
		}
		ids := append([]string(nil), output.Attachments...)
		scopes, err := deliveryScopesForEventTx(ctx, tx, eventID, kind, taskID, callbackRunID, callbackRevision)
		if err != nil {
			return err
		}
		intent, err := insertDeliveryIntentTx(ctx, tx, deliveryIntentInsert{
			eventID: eventID, scheduleID: scheduleID, purpose: purpose,
			draft: personal.DeliveryDraft{Text: output.Text, Attachments: ids}, scopes: scopes,
		}, time.Now().UTC())
		if err != nil {
			return err
		}
		replyParts, err = appendDeliveryPartsTx(ctx, tx, eventID, intent.ID, output.Text, attachments, chunks, scheduleID, 0)
		if err != nil {
			return err
		}
		deliveryCount++
		if err := updateDeliveryIntentRenderedTx(ctx, tx, intent.ID, ""); err != nil {
			return err
		}
	}
	if len(notices) > 0 {
		replyParts, err = enqueueScheduleNotices(ctx, tx, eventID, notices, timestamp(time.Now()))
		if err != nil {
			return err
		}
		deliveryCount += len(notices)
	}
	if kind == personal.EventUser {
		raw := fmt.Sprintf(`{"event_id":%d}`, eventID)
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_runs(event_id,kind,input,status,created_at)
			VALUES(?,? ,?,'queued',?) ON CONFLICT(event_id) WHERE kind='memory_review' DO NOTHING`, eventID, personal.RunMemoryReview, raw, timestamp(time.Now())); err != nil {
			return err
		}
	}
	outcome := "completed"
	outcomeTaskID := taskID
	if discardedStaleCallback {
		outcome = "discarded_stale_callback"
		if len(intents) > 0 {
			outcome = "stale_callback_saved_delivery"
		} else {
			outcomeTaskID = 0
		}
	}
	if err := commitStoreOutcome(ctx, tx, "event.complete", outcome, nil,
		zap.Int64("event_id", eventID), zap.String("event_kind", string(kind)),
		zap.Int64("task_id", outcomeTaskID), zap.Int64("run_id", callbackRunID),
		zap.String("event_status", string(personal.EventCompleted)), zap.Bool("silent", !hasDelivery), zap.Int("reply_part_count", replyParts)); err != nil {
		return err
	}
	if replyParts > 0 {
		logging.FromContext(ctx).Info("conversation.reply_queued", zap.Int64("event_id", eventID), zap.Int64("task_id", taskID),
			zap.Int64("run_id", callbackRunID), zap.Int("parts", replyParts), zap.Int("delivery_count", deliveryCount),
			zap.Any("payload", output.Text), zap.Any("attachments", output.Attachments), zap.Int64s("delivery_ids", deliveryIDs(intents)))
	}
	return nil
}

func (s *Store) Recover(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recoverTaskInputsTx(ctx, tx); err != nil {
		return err
	}
	var summary recoveryOutcomePayload
	res, err := tx.ExecContext(ctx, `UPDATE personal_events SET status='queued',elapsed_ms=0,interrupted=1 WHERE status='running'`)
	if err != nil {
		return err
	}
	if summary.RequeuedEvents, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,task_id,run_id,schedule_id,status,created_at)
		SELECT 'task','任务预算在中断前已用完；已有阶段成果已保留。',r.task_id,r.id,(SELECT schedule_id FROM personal_schedule_occurrences WHERE task_id=r.task_id),'queued',?
		FROM personal_runs r JOIN personal_tasks t ON t.id=r.task_id
		WHERE r.kind='task' AND r.status='running' AND t.status='running' AND r.revision=t.revision AND t.used_ms>=t.budget_ms
		AND NOT EXISTS(SELECT 1 FROM personal_events e WHERE e.run_id=r.id)`, timestamp(time.Now()))
	if err != nil {
		return err
	}
	if summary.BudgetEvents, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_runs SET status='failed',error=CASE WHEN error='' THEN 'task budget exhausted during interrupted run' ELSE error END
		WHERE kind='task' AND status='running' AND EXISTS(SELECT 1 FROM personal_tasks t WHERE t.id=personal_runs.task_id
		AND t.status='running' AND t.revision=personal_runs.revision AND t.used_ms>=t.budget_ms)`)
	if err != nil {
		return err
	}
	if summary.FailedRuns, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_tasks SET status='waiting_budget',updated_at=?
		WHERE status='running' AND used_ms>=budget_ms`, timestamp(time.Now()))
	if err != nil {
		return err
	}
	if summary.WaitingTasks, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_runs SET status='queued' WHERE status='running'`)
	if err != nil {
		return err
	}
	if summary.RequeuedRuns, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_tasks SET status='queued',updated_at=? WHERE status='running'`, timestamp(time.Now()))
	if err != nil {
		return err
	}
	if summary.QueuedTasks, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_outbox SET status='uncertain',detail='outcome unknown after restart' WHERE status='sending'`)
	if err != nil {
		return err
	}
	if summary.UncertainSends, err = changedRows(res); err != nil {
		return err
	}
	return commitStoreOutcome(ctx, tx, "store.recover", "recovered", summary,
		zap.Int64("requeued_events", summary.RequeuedEvents), zap.Int64("requeued_runs", summary.RequeuedRuns), zap.Int64("uncertain_sends", summary.UncertainSends))
}
