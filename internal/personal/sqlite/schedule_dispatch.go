package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	"kagari/internal/personal"
)

func (s *Store) DispatchDueSchedule(ctx context.Context, now time.Time) (*personal.ScheduleOccurrence, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules
		WHERE status='active' AND next_run_at<=? AND EXISTS(SELECT 1 FROM personal_events e
		WHERE e.id=authorized_event_id AND e.kind='user' AND e.status='completed') ORDER BY next_run_at,id LIMIT 1`, timestamp(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o := personal.ScheduleOccurrence{ScheduleID: schedule.ID, ScheduleRevision: schedule.Revision, ScopeRevision: schedule.ScopeRevision,
		ScheduledAt: *schedule.NextRunAt, CheckedAt: now.UTC(), Definition: schedule.ScheduleDefinition, CoverageEligible: true}
	next, timeErr := personal.NextScheduleTime(schedule.Cron, schedule.Timezone, now)
	if timeErr == nil {
		following, err := personal.NextScheduleTime(schedule.Cron, schedule.Timezone, o.ScheduledAt)
		if err != nil {
			timeErr = err
		} else if !following.After(now) {
			t := now.UTC()
			o.CoalescedUntil = &t
		}
	}
	if timeErr != nil {
		o.Outcome, o.Reason = "blocked", "invalid_schedule"
	} else {
		var busy bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM personal_schedule_occurrences o
			JOIN personal_tasks t ON t.id=o.task_id WHERE o.schedule_id=? AND (
			t.status IN ('queued','running','awaiting_review') OR
			(t.status IN ('failed','waiting_input') AND t.used_ms<t.budget_ms AND EXISTS(SELECT 1 FROM personal_events e
			JOIN personal_runs r ON r.id=e.run_id WHERE e.task_id=t.id AND e.kind='task' AND e.status IN ('queued','running')
			AND (t.status='failed' OR (e.status='running' AND e.interrupted=0))
			AND r.revision=t.revision AND r.id=(SELECT MAX(id) FROM personal_runs WHERE task_id=t.id)))))`, schedule.ID).Scan(&busy)
		if err != nil {
			return nil, err
		}
		if busy {
			o.Outcome, o.Reason = "skipped_busy", "previous task or callback is still active"
		} else if err := s.checkScheduleAttachments(ctx, tx, schedule); err != nil {
			if !errors.Is(err, personal.ErrAttachmentUnavailable) {
				return nil, err
			}
			o.Outcome, o.Reason = "blocked", "attachment_unavailable"
		} else {
			o.Outcome = "enqueued"
			res, err := tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,schedule_id,status,created_at)
				VALUES('schedule',?,?,'completed',?)`, fmt.Sprintf("Schedule #%d: %s", schedule.ID, schedule.Title), schedule.ID, timestamp(now))
			if err != nil {
				return nil, err
			}
			o.EventID, err = res.LastInsertId()
			if err != nil {
				return nil, err
			}
			task, _, err := s.createTaskRunTx(ctx, tx, o.EventID, schedule.Title, personal.TaskInput{Goal: schedule.Goal, Context: schedule.Context, Attachments: schedule.Attachments}, schedule.TaskBudgetMS, now)
			if err != nil {
				return nil, err
			}
			o.TaskID = task.ID
			if _, err := tx.ExecContext(ctx, `UPDATE personal_events SET task_id=? WHERE id=?`, task.ID, o.EventID); err != nil {
				return nil, err
			}
		}
	}
	raw, err := json.Marshal(o.Definition)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_schedule_occurrences(schedule_id,schedule_revision,scope_revision,scheduled_at,checked_at,coalesced_until,outcome,reason,task_id,event_id,definition_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, o.ScheduleID, o.ScheduleRevision, o.ScopeRevision, timestamp(o.ScheduledAt), timestamp(now), nullableTime(o.CoalescedUntil), o.Outcome, o.Reason, nullableID(o.TaskID), nullableID(o.EventID), string(raw))
	if err != nil {
		return nil, err
	}
	o.ID, err = res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if o.Outcome == "blocked" {
		if err := pauseScheduleTx(ctx, tx, *schedule, o.Reason, now); err != nil {
			return nil, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE personal_schedules SET next_run_at=? WHERE id=?`, timestamp(next), schedule.ID); err != nil {
		return nil, err
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.dispatch", o.Outcome, o, zap.Int64("schedule_id", o.ScheduleID), zap.Int64("scope_revision", o.ScopeRevision), zap.Int64("occurrence_id", o.ID), zap.Int64("task_id", o.TaskID)); err != nil {
		return nil, err
	}
	return &o, nil
}

func (s *Store) checkScheduleAttachments(ctx context.Context, tx *sql.Tx, schedule *personal.Schedule) error {
	for _, item := range schedule.Attachments {
		if _, err := s.validateStoredAttachment(ctx, tx, item.ID); err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				return fmt.Errorf("%w: %s", personal.ErrAttachmentUnavailable, err)
			}
			return err
		}
		var path string
		if err := tx.QueryRowContext(ctx, `SELECT path FROM personal_attachments WHERE id=?`, item.ID).Scan(&path); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
				return fmt.Errorf("%w: %s", personal.ErrAttachmentUnavailable, err)
			}
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func cancelUnstartedScheduleTasks(ctx context.Context, tx *sql.Tx, scheduleID int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE status IN ('queued','running')
		AND task_id IN (SELECT task_id FROM personal_schedule_occurrences WHERE schedule_id=? AND started_at IS NULL)`, scheduleID); err != nil {
		return err
	}
	if err := releaseTerminalTaskInputsTx(ctx, tx); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status='cancelled',updated_at=? WHERE status<>'cancelled'
		AND id IN (SELECT task_id FROM personal_schedule_occurrences WHERE schedule_id=? AND started_at IS NULL)`, timestamp(now), scheduleID)
	return err
}

func pauseScheduleTx(ctx context.Context, tx *sql.Tx, schedule personal.Schedule, reason string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE personal_schedules SET status='paused',pause_reason=?,next_run_at=NULL,revision=revision+1,updated_at=? WHERE id=? AND status='active'`, reason, timestamp(now), schedule.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if err := cancelUnstartedScheduleTasks(ctx, tx, schedule.ID, now); err != nil {
		return err
	}
	text := fmt.Sprintf("计划 #%d「%s」已暂停：%s。修复后请明确恢复计划；检查进度保留。", schedule.ID, schedule.Title, reason)
	res, err = tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,schedule_id,status,created_at) VALUES('schedule',?,?,'completed',?)`, text, schedule.ID, timestamp(now))
	if err != nil {
		return err
	}
	eventID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO personal_history(event_id,role,text,schedule_id) VALUES(?,'assistant',?,?)`, eventID, text, schedule.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_outbox(event_id,part,text,status,next_attempt_at) VALUES(?,1,?,'pending',?)`, eventID, text, timestamp(now))
	return err
}
