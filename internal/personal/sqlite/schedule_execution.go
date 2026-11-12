package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"kagari/internal/personal"
)

func scheduledTaskTrigger(ctx context.Context, tx *sql.Tx, taskID int64) (*personal.ScheduleTrigger, error) {
	o, err := scanScheduleOccurrence(tx.QueryRowContext(ctx, `SELECT `+occurrenceColumns+` FROM personal_schedule_occurrences WHERE task_id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &personal.ScheduleTrigger{ScheduleID: o.ScheduleID, ScheduleRevision: o.ScheduleRevision, ScopeRevision: o.ScopeRevision,
		OccurrenceID: o.ID, ScheduledAt: o.ScheduledAt, EnqueuedAt: o.CheckedAt, StartedAt: o.StartedAt, Window: o.Window,
		Timezone: o.Definition.Timezone, RequiredSources: o.Definition.RequiredSources, CoverageEligible: o.CoverageEligible}, nil
}

// BeginScheduledRun is called after the worker owns the task's execution lock.
func (s *Store) BeginScheduledRun(ctx context.Context, runID int64, now time.Time) (*personal.Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, runID))
	if err != nil {
		return nil, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, run.TaskID))
	if err != nil {
		return nil, err
	}
	if run.Kind != personal.RunTask || run.Status != personal.RunRunning || task.Revision != run.Revision || task.Status != personal.TaskRunning {
		return nil, nil
	}
	trigger, err := scheduledTaskTrigger(ctx, tx, task.ID)
	if err != nil {
		return nil, err
	}
	if trigger != nil && trigger.StartedAt == nil {
		schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, trigger.ScheduleID))
		if err != nil {
			return nil, err
		}
		if schedule.Status != personal.ScheduleActive || schedule.ScopeRevision != trigger.ScopeRevision {
			if err := cancelUnstartedScheduleTasks(ctx, tx, schedule.ID, now); err != nil {
				return nil, err
			}
			return nil, tx.Commit()
		}
		if _, err := personal.ParseSchedule(schedule.Cron, schedule.Timezone); err != nil {
			if err := pauseScheduleTx(ctx, tx, *schedule, "invalid_schedule", now); err != nil {
				return nil, err
			}
			return nil, tx.Commit()
		}
		// Validate the copied material, even when the current schedule has changed since enqueue.
		var definition personal.ScheduleDefinition
		var rawDefinition string
		if err := tx.QueryRowContext(ctx, `SELECT definition_json FROM personal_schedule_occurrences WHERE id=?`, trigger.OccurrenceID).Scan(&rawDefinition); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(rawDefinition), &definition); err != nil {
			return nil, err
		}
		copy := *schedule
		copy.Attachments = definition.Attachments
		if err := s.checkScheduleAttachments(ctx, tx, &copy); err != nil {
			if !errors.Is(err, personal.ErrAttachmentUnavailable) {
				return nil, err
			}
			if err := pauseScheduleTx(ctx, tx, *schedule, "attachment_unavailable", now); err != nil {
				return nil, err
			}
			return nil, tx.Commit()
		}
		started := now.UTC()
		trigger.StartedAt = &started
		var start, end any
		if schedule.WindowMode == personal.WindowSinceSuccess {
			from := schedule.BaselineAt
			if schedule.CoveredThrough != nil {
				from = schedule.CoveredThrough
			}
			if from == nil || !from.Before(started) {
				return nil, errors.New("personal: scheduled check start must precede its first execution")
			}
			trigger.Window = &personal.CheckWindow{Start: *from, End: started}
			start, end = timestamp(*from), timestamp(started)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE personal_schedule_occurrences SET started_at=?,window_start=?,window_end=? WHERE id=? AND started_at IS NULL`, timestamp(started), start, end, trigger.OccurrenceID); err != nil {
			return nil, err
		}
	}
	if trigger != nil {
		var input personal.TaskInput
		if err := json.Unmarshal([]byte(run.Input), &input); err != nil {
			return nil, err
		}
		input.Trigger = trigger
		run.Input, err = encodeTaskInput(input)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET input=? WHERE id=?`, run.Input, run.ID); err != nil {
			return nil, err
		}
	}
	if err := s.refreshRunAttachments(ctx, tx, run); err != nil {
		return nil, err
	}
	run.RemainingMS = remainingMS(task.BudgetMS, task.UsedMS)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

func settleScheduledTask(ctx context.Context, tx *sql.Tx, task personal.Task, status personal.TaskStatus, check *personal.CheckAssessment, now time.Time) error {
	o, err := scanScheduleOccurrence(tx.QueryRowContext(ctx, `SELECT `+occurrenceColumns+` FROM personal_schedule_occurrences WHERE task_id=?`, task.ID))
	if errors.Is(err, sql.ErrNoRows) {
		if check != nil {
			return errors.New("personal: ordinary tasks do not accept a schedule check assessment")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if o.Definition.WindowMode == personal.WindowNone {
		if check != nil {
			return errors.New("personal: this schedule has no continuous check window")
		}
		return nil
	}
	if status == personal.TaskCompleted && o.CoverageEligible {
		if check == nil || !check.Complete || len(check.Limitations) > 0 || o.Window == nil {
			return errors.New("personal: completing a scheduled check requires a complete assessment of its frozen window")
		}
		if len(check.RequiredSources) != len(o.Definition.RequiredSources) {
			return errors.New("personal: required source assessment set does not match the schedule")
		}
		seen := map[string]bool{}
		for _, a := range check.RequiredSources {
			if seen[a.ID] || !a.Complete || strings.TrimSpace(a.Evidence) == "" {
				return errors.New("personal: each required source needs a distinct complete assessment and evidence")
			}
			seen[a.ID] = true
		}
		for _, required := range o.Definition.RequiredSources {
			if !seen[required.ID] {
				return fmt.Errorf("personal: missing required source %q", required.ID)
			}
		}
		var saved bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM personal_runs WHERE task_id=? AND revision=? AND kind='task' AND status IN ('completed','failed') AND result<>'')`, task.ID, task.Revision).Scan(&saved); err != nil {
			return err
		}
		if !saved {
			return errors.New("personal: scheduled check has no saved stage result")
		}
	}
	if check == nil || o.CreditedAt != nil {
		return nil
	}
	raw, err := json.Marshal(check)
	if err != nil {
		return err
	}
	if len(raw) > 128_000 {
		return errors.New("personal: check assessment exceeds its size limit")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_schedule_occurrences SET check_result_json=? WHERE id=?`, string(raw), o.ID); err != nil {
		return err
	}
	if status != personal.TaskCompleted || !o.CoverageEligible {
		return nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE personal_schedules SET covered_through=? WHERE id=? AND scope_revision=?
		AND window_mode='since_success' AND status<>'cancelled' AND COALESCE(covered_through,baseline_at)>=? AND COALESCE(covered_through,baseline_at)<?`,
		timestamp(o.Window.End), o.ScheduleID, o.ScopeRevision, timestamp(o.Window.Start), timestamp(o.Window.End))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE personal_schedule_occurrences SET credited_task_revision=?,credited_at=? WHERE id=? AND credited_at IS NULL`, task.Revision, timestamp(now), o.ID)
	return err
}
