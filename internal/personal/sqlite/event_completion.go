package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/logging"
	"kagari/internal/personal"
)

// Event completion records replies, schedules follow-up work, and recovers interrupted state.
func (s *Store) CompleteEvent(ctx context.Context, eventID int64, output personal.TurnOutput, chunks []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind personal.EventKind
	var status personal.EventStatus
	var callbackTaskID, callbackRunID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,task_id,run_id FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &status, &callbackTaskID, &callbackRunID); err != nil {
		return err
	}
	if status != personal.EventRunning {
		return fmt.Errorf("personal: cannot complete event %d in state %q", eventID, status)
	}
	discardedStaleCallback := false
	if kind == personal.EventTask {
		current, err := currentTaskCallback(ctx, tx, eventID, callbackTaskID.Int64, callbackRunID.Int64)
		if err != nil {
			return err
		}
		if !current {
			output, chunks = personal.TurnOutput{Silent: true}, nil
			discardedStaleCallback = true
		} else if output.TaskID != 0 && output.TaskID != callbackTaskID.Int64 {
			return errors.New("personal: task callback cannot link its reply to another task")
		}
	}
	attachments := make([]channel.Attachment, 0, len(output.Attachments))
	if !output.Silent {
		for _, id := range output.Attachments {
			attachment, err := s.validateStoredAttachment(ctx, tx, id)
			if err != nil {
				return err
			}
			attachments = append(attachments, attachment)
		}
	} else {
		output.Attachments = nil
	}
	taskID := output.TaskID
	if taskID != 0 {
		if _, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID)); err != nil {
			return fmt.Errorf("personal: event task %d: %w", taskID, err)
		}
	}
	scheduleID := output.ScheduleID
	if taskID > 0 {
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
	if kind == personal.EventTask && scheduleID == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(schedule_id,0) FROM personal_events WHERE id=?`, eventID).Scan(&scheduleID); err != nil {
			return err
		}
	}
	if !discardedStaleCallback {
		if err := recordReplyTx(ctx, tx, eventID, output); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_events SET status='completed',task_id=?,schedule_id=? WHERE id=? AND status='running'`, nullableID(taskID), nullableID(scheduleID), eventID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_history SET schedule_id=? WHERE event_id=?`, nullableID(scheduleID), eventID); err != nil {
		return err
	}
	var notices []scheduleNotice
	if (kind == personal.EventTask || kind == personal.EventUser) && !output.Silent {
		notices, err = scheduleNotices(ctx, tx, eventID, kind)
		if err != nil {
			return err
		}
		for _, notice := range notices {
			output.Text += "\n\n" + notice.text
		}
	}
	replyParts := 0
	if !output.Silent {
		replyParts, err = appendReplyPartsTx(ctx, tx, eventID, output.Text, attachments, chunks, scheduleID)
		if err != nil {
			return err
		}
	}
	if !output.Silent && len(notices) > 0 {
		replyParts, err = enqueueScheduleNotices(ctx, tx, eventID, replyParts, notices, timestamp(time.Now()))
		if err != nil {
			return err
		}
	}
	if kind == personal.EventUser {
		raw := fmt.Sprintf(`{"event_id":%d}`, eventID)
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_runs(event_id,kind,input,status,created_at)
			VALUES(?,? ,?,'queued',?) ON CONFLICT(event_id) WHERE kind='memory_review' DO NOTHING`, eventID, personal.RunMemoryReview, raw, timestamp(time.Now())); err != nil {
			return err
		}
	}
	outcome := "completed"
	if discardedStaleCallback {
		outcome = "discarded_stale_callback"
	}
	if err := commitStoreOutcome(ctx, tx, "event.complete", outcome, nil,
		zap.Int64("event_id", eventID), zap.String("event_kind", string(kind)),
		zap.Int64("task_id", taskID), zap.Int64("run_id", callbackRunID.Int64),
		zap.String("event_status", string(personal.EventCompleted)), zap.Bool("silent", output.Silent), zap.Int("reply_part_count", replyParts)); err != nil {
		return err
	}
	if replyParts > 0 {
		logging.FromContext(ctx).Info("conversation.reply_queued", zap.Int64("event_id", eventID), zap.Int64("task_id", taskID),
			zap.Int64("run_id", callbackRunID.Int64), zap.Int("parts", replyParts), zap.Any("payload", output.Text), zap.Any("attachments", output.Attachments))
	}
	return nil
}

func currentTaskCallback(ctx context.Context, tx *sql.Tx, eventID, taskID, runID int64) (bool, error) {
	if taskID <= 0 || runID <= 0 {
		return false, nil
	}
	var runTaskID, runRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision FROM personal_runs WHERE id=? AND kind='task'`, runID).Scan(&runTaskID, &runRevision); err != nil {
		return false, err
	}
	var taskRevision int64
	var taskStatus personal.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&taskRevision, &taskStatus); err != nil {
		return false, err
	}
	if runTaskID != taskID || runRevision != taskRevision || taskStatus == personal.TaskCancelled {
		return false, nil
	}
	var latestID, latestRevision, latestEventID int64
	if err := tx.QueryRowContext(ctx, `SELECT id,revision,event_id FROM personal_runs WHERE task_id=? ORDER BY id DESC LIMIT 1`, taskID).Scan(&latestID, &latestRevision, &latestEventID); err != nil {
		return false, err
	}
	return latestRevision == taskRevision && (latestID == runID || latestEventID == eventID), nil
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
