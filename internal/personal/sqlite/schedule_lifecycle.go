package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"kagari/internal/personal"
)

// Schedule creation and revision transitions.
func (s *Store) CreateSchedule(ctx context.Context, eventID int64, key string, input personal.ScheduleInput, now time.Time) (personal.Schedule, bool, error) {
	if !requestKeyValid(key) {
		return personal.Schedule{}, false, errors.New("personal: request key is required")
	}
	if input.Timezone == "" {
		input.Timezone = personal.DefaultScheduleTimezone
	}
	if input.WindowMode == "" {
		input.WindowMode = personal.WindowNone
	}
	if err := validateScheduleDefinition(input.Title, input.Goal, input.Context, input.WindowMode, input.RequiredSources); err != nil {
		return personal.Schedule{}, false, err
	}
	if err := validateScheduleAttachmentIDs(input.Attachments); err != nil {
		return personal.Schedule{}, false, err
	}
	if err := validateInitialCheckFrom(input.InitialCheckFrom); err != nil {
		return personal.Schedule{}, false, err
	}
	payload, err := encodeScheduleRequest(scheduleRequestPayload{Input: &input})
	if err != nil {
		return personal.Schedule{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Schedule{}, false, err
	}
	if schedule, found, err := scheduleRequestResult(ctx, tx, eventID, key, "create_schedule", payload); err != nil {
		return personal.Schedule{}, false, err
	} else if found {
		if err := commitStoreOutcome(ctx, tx, "schedule.create", "replayed", schedule,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", schedule.ID), zap.Int64("revision", schedule.Revision)); err != nil {
			return personal.Schedule{}, false, err
		}
		return schedule, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.Schedule{}, false, err
	}
	budget := s.taskBudgetMS
	if input.TaskBudgetMS != nil {
		budget = *input.TaskBudgetMS
	}
	if budget <= 0 || budget > maxScheduleTaskBudgetMS {
		return personal.Schedule{}, false, errors.New("personal: schedule task budget is outside the supported range")
	}
	now = now.UTC()
	firstRun, err := personal.NextScheduleTime(input.Cron, input.Timezone, now)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	spec, err := personal.ParseSchedule(input.Cron, input.Timezone)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	baseline, err := newScheduleBaseline(input.WindowMode, input.InitialCheckFrom, spec, firstRun)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	attachments, err := s.canonicalScheduleAttachments(ctx, tx, input.Attachments)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	attachmentRaw, err := json.Marshal(attachments)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	sources := input.RequiredSources
	if sources == nil {
		sources = []personal.SourceRequirement{}
	}
	sourceRaw, err := json.Marshal(sources)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	candidate := personal.Schedule{ScheduleDefinition: personal.ScheduleDefinition{Title: input.Title, Goal: input.Goal, Context: input.Context,
		Attachments: attachments, Cron: input.Cron, Timezone: input.Timezone, WindowMode: input.WindowMode,
		RequiredSources: sources, TaskBudgetMS: budget}}
	if err := s.checkScheduleAttachments(ctx, tx, &candidate); err != nil {
		return personal.Schedule{}, false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_schedules(revision,scope_revision,title,goal,context,attachments,window_mode,required_sources,cron,timezone,status,next_run_at,task_budget_ms,baseline_at,created_event_id,authorized_event_id,created_at,updated_at)
		VALUES(1,1,?,?,?,?,?,?,?,?, 'active',?,?,?,?,?,?,?)`,
		input.Title, input.Goal, input.Context, string(attachmentRaw), input.WindowMode, string(sourceRaw), input.Cron, input.Timezone,
		timestamp(firstRun), budget, nullableTime(baseline), eventID, eventID, timestamp(now), timestamp(now))
	if err != nil {
		return personal.Schedule{}, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return personal.Schedule{}, false, err
	}
	schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, id))
	if err != nil {
		return personal.Schedule{}, false, err
	}
	if err := insertScheduleRequest(ctx, tx, eventID, key, "create_schedule", payload, *schedule); err != nil {
		return personal.Schedule{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.create", "created", schedule,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", schedule.ID), zap.Int64("revision", schedule.Revision)); err != nil {
		return personal.Schedule{}, false, err
	}
	return *schedule, false, nil
}

func validateSchedulePatch(patch personal.SchedulePatch) error {
	if err := validateInitialCheckFrom(patch.InitialCheckFrom); err != nil {
		return err
	}
	if patch.Title != nil && (strings.TrimSpace(*patch.Title) == "" || utf8.RuneCountInString(*patch.Title) > maxTitleChars) {
		return errors.New("personal: schedule title is required and limited to 200 characters")
	}
	if patch.Goal != nil && (strings.TrimSpace(*patch.Goal) == "" || utf8.RuneCountInString(*patch.Goal) > personal.MaxTaskChars) {
		return errors.New("personal: schedule goal is required and limited to 32000 characters")
	}
	if patch.Context != nil && utf8.RuneCountInString(*patch.Context) > personal.MaxTaskChars {
		return errors.New("personal: schedule context exceeds its size limit")
	}
	if patch.Attachments != nil {
		if err := validateScheduleAttachmentIDs(*patch.Attachments); err != nil {
			return err
		}
	}
	if patch.WindowMode != nil && *patch.WindowMode != personal.WindowNone && *patch.WindowMode != personal.WindowSinceSuccess {
		return fmt.Errorf("personal: unsupported schedule window mode %q", *patch.WindowMode)
	}
	if patch.RequiredSources != nil {
		if err := validateScheduleDefinition("schedule", "goal", "", personal.WindowNone, *patch.RequiredSources); err != nil {
			return err
		}
	}
	if patch.TaskBudgetMS != nil && (*patch.TaskBudgetMS <= 0 || *patch.TaskBudgetMS > maxScheduleTaskBudgetMS) {
		return errors.New("personal: schedule task budget is outside the supported range")
	}
	if patch.Timezone != nil && *patch.Timezone == "" {
		return errors.New("personal: schedule timezone is required")
	}
	if patch.Status != nil && *patch.Status != personal.ScheduleActive && *patch.Status != personal.SchedulePaused {
		return errors.New("personal: schedule status may only be active or paused")
	}
	return nil
}

func schedulePatchRequestPayload(id, revision int64, patch personal.SchedulePatch) (string, error) {
	return encodeScheduleRequest(scheduleRequestPayload{ScheduleID: id, Revision: revision, Patch: &patch})
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *Store) updateScheduleTx(ctx context.Context, tx *sql.Tx, eventID, id, expectedRevision int64, patch personal.SchedulePatch, now time.Time) (personal.Schedule, error) {
	if err := validateSchedulePatch(patch); err != nil {
		return personal.Schedule{}, err
	}
	current, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, id))
	if err != nil {
		return personal.Schedule{}, err
	}
	if current.Revision != expectedRevision {
		return personal.Schedule{}, fmt.Errorf("personal: schedule %d revision is %d, expected %d", id, current.Revision, expectedRevision)
	}
	if current.Status == personal.ScheduleCancelled {
		return personal.Schedule{}, errors.New("personal: cancelled schedules cannot be updated")
	}
	if current.Revision == math.MaxInt64 {
		return personal.Schedule{}, errors.New("personal: schedule revision exhausted")
	}
	now = now.UTC()
	next := *current
	if patch.Title != nil {
		next.Title = *patch.Title
	}
	if patch.Goal != nil {
		next.Goal = *patch.Goal
	}
	if patch.Context != nil {
		next.Context = *patch.Context
	}
	if patch.Attachments != nil {
		attachments, err := s.canonicalScheduleAttachments(ctx, tx, *patch.Attachments)
		if err != nil {
			return personal.Schedule{}, err
		}
		next.Attachments = attachments
	}
	if patch.Cron != nil {
		next.Cron = *patch.Cron
	}
	if patch.Timezone != nil {
		next.Timezone = *patch.Timezone
	}
	if patch.WindowMode != nil {
		next.WindowMode = *patch.WindowMode
	}
	if patch.RequiredSources != nil {
		next.RequiredSources = append([]personal.SourceRequirement{}, (*patch.RequiredSources)...)
	}
	if patch.TaskBudgetMS != nil {
		next.TaskBudgetMS = *patch.TaskBudgetMS
	}
	if patch.InitialCheckFrom != nil && next.WindowMode == personal.WindowNone {
		return personal.Schedule{}, errors.New("personal: initial check time requires since_success mode")
	}
	contentChanged := next.Goal != current.Goal || next.Context != current.Context ||
		next.WindowMode != current.WindowMode || !reflect.DeepEqual(next.Attachments, current.Attachments) ||
		!reflect.DeepEqual(next.RequiredSources, current.RequiredSources) ||
		(patch.InitialCheckFrom != nil && (current.BaselineAt == nil || !patch.InitialCheckFrom.Equal(*current.BaselineAt) || current.CoveredThrough != nil))
	cronChanged := next.Cron != current.Cron || next.Timezone != current.Timezone
	manualPause := patch.Status != nil && *patch.Status == personal.SchedulePaused &&
		(current.Status != personal.SchedulePaused || current.PauseReason != "manual")
	manualResume := current.Status == personal.SchedulePaused && current.PauseReason == "manual" && patch.Status != nil && *patch.Status == personal.ScheduleActive
	resume := current.Status == personal.SchedulePaused && patch.Status != nil && *patch.Status == personal.ScheduleActive
	newStatus := current.Status
	newPauseReason := current.PauseReason
	if manualPause {
		newStatus, newPauseReason = personal.SchedulePaused, "manual"
	} else if resume {
		newStatus, newPauseReason = personal.ScheduleActive, ""
	}
	stateChanged := next.Title != current.Title || contentChanged || cronChanged || next.TaskBudgetMS != current.TaskBudgetMS ||
		newStatus != current.Status || newPauseReason != current.PauseReason
	if !stateChanged {
		return *current, nil
	}
	if cronChanged {
		if _, err := personal.NextScheduleTime(next.Cron, next.Timezone, now); err != nil {
			return personal.Schedule{}, err
		}
	}
	if resume || patch.Attachments != nil {
		if err := s.checkScheduleAttachments(ctx, tx, &next); err != nil {
			return personal.Schedule{}, err
		}
	}
	if current.ScopeRevision == math.MaxInt64 && (contentChanged || manualResume) {
		return personal.Schedule{}, errors.New("personal: schedule scope revision exhausted")
	}
	advanceNextRun := resume || cronChanged ||
		(contentChanged && (current.NextRunAt == nil || !current.NextRunAt.After(now)))
	next.Revision = current.Revision + 1
	next.ScopeRevision = current.ScopeRevision
	next.Status = newStatus
	next.PauseReason = newPauseReason
	next.AuthorizedEventID = eventID
	next.UpdatedAt = now
	if contentChanged || manualResume {
		next.ScopeRevision++
		next.CoveredThrough = nil
	}
	var firstActiveRun *time.Time
	if next.Status == personal.ScheduleActive {
		if current.Status != personal.ScheduleActive || cronChanged || current.NextRunAt == nil || !current.NextRunAt.After(now) {
			first, err := personal.NextScheduleTime(next.Cron, next.Timezone, now)
			if err != nil {
				return personal.Schedule{}, err
			}
			firstActiveRun = &first
		} else {
			firstActiveRun = current.NextRunAt
		}
	}
	if next.WindowMode == personal.WindowNone {
		next.BaselineAt, next.CoveredThrough = nil, nil
	} else if manualResume {
		baseline := now
		if patch.InitialCheckFrom != nil {
			baseline = maxTime(baseline, patch.InitialCheckFrom.UTC())
		}
		next.BaselineAt, next.CoveredThrough = &baseline, nil
	} else if contentChanged {
		if patch.InitialCheckFrom != nil {
			baseline := patch.InitialCheckFrom.UTC()
			next.BaselineAt = &baseline
		} else if next.Status == personal.SchedulePaused {
			next.BaselineAt = nil
		} else {
			spec, err := personal.ParseSchedule(next.Cron, next.Timezone)
			if err != nil {
				return personal.Schedule{}, err
			}
			next.BaselineAt, err = newScheduleBaseline(next.WindowMode, nil, spec, *firstActiveRun)
			if err != nil {
				return personal.Schedule{}, err
			}
		}
	} else if next.WindowMode == personal.WindowSinceSuccess && next.BaselineAt == nil && resume {
		spec, err := personal.ParseSchedule(next.Cron, next.Timezone)
		if err != nil {
			return personal.Schedule{}, err
		}
		next.BaselineAt, err = newScheduleBaseline(next.WindowMode, nil, spec, *firstActiveRun)
		if err != nil {
			return personal.Schedule{}, err
		}
	}
	if patch.InitialCheckFrom != nil && next.WindowMode == personal.WindowSinceSuccess {
		if next.BaselineAt == nil || *patch.InitialCheckFrom != *next.BaselineAt {
			baseline := patch.InitialCheckFrom.UTC()
			if manualResume {
				baseline = maxTime(baseline, now)
			}
			next.BaselineAt = &baseline
		}
	}
	if next.WindowMode == personal.WindowSinceSuccess && next.BaselineAt != nil {
		limit := next.NextRunAt
		if next.Status == personal.ScheduleActive {
			limit = firstActiveRun
		}
		if limit != nil && !next.BaselineAt.Before(*limit) {
			return personal.Schedule{}, errors.New("personal: initial check time must precede the next scheduled run")
		}
	}
	if manualPause {
		next.NextRunAt = nil
	} else if next.Status == personal.ScheduleActive && advanceNextRun {
		next.NextRunAt = firstActiveRun
	}
	if next.Status == personal.SchedulePaused {
		next.NextRunAt = nil
	}
	attachmentsRaw, err := json.Marshal(next.Attachments)
	if err != nil {
		return personal.Schedule{}, err
	}
	sourcesRaw, err := json.Marshal(next.RequiredSources)
	if err != nil {
		return personal.Schedule{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE personal_schedules SET revision=?,scope_revision=?,title=?,goal=?,context=?,attachments=?,window_mode=?,required_sources=?,cron=?,timezone=?,status=?,next_run_at=?,pause_reason=?,task_budget_ms=?,baseline_at=?,covered_through=?,authorized_event_id=?,updated_at=? WHERE id=? AND revision=?`,
		next.Revision, next.ScopeRevision, next.Title, next.Goal, next.Context, string(attachmentsRaw), next.WindowMode, string(sourcesRaw),
		next.Cron, next.Timezone, next.Status, nullableTime(next.NextRunAt), next.PauseReason, next.TaskBudgetMS,
		nullableTime(next.BaselineAt), nullableTime(next.CoveredThrough), eventID, timestamp(now), id, expectedRevision)
	if err != nil {
		return personal.Schedule{}, err
	}
	if changed, err := res.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return personal.Schedule{}, err
		}
		return personal.Schedule{}, errors.New("personal: schedule changed while updating")
	}
	if contentChanged || manualPause {
		if err := cancelUnstartedScheduleTasks(ctx, tx, id, now); err != nil {
			return personal.Schedule{}, err
		}
	}
	return next, nil
}

func (s *Store) UpdateSchedule(ctx context.Context, eventID int64, key string, id, expectedRevision int64, patch personal.SchedulePatch, now time.Time) (personal.Schedule, bool, error) {
	if !requestKeyValid(key) || id <= 0 || expectedRevision <= 0 {
		return personal.Schedule{}, false, errors.New("personal: request key, schedule ID and positive revision are required")
	}
	if err := validateSchedulePatch(patch); err != nil {
		return personal.Schedule{}, false, err
	}
	payload, err := schedulePatchRequestPayload(id, expectedRevision, patch)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Schedule{}, false, err
	}
	if schedule, found, err := scheduleRequestResult(ctx, tx, eventID, key, "update_schedule", payload); err != nil {
		return personal.Schedule{}, false, err
	} else if found {
		if err := commitStoreOutcome(ctx, tx, "schedule.update", "replayed", schedule,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", schedule.ID), zap.Int64("revision", schedule.Revision)); err != nil {
			return personal.Schedule{}, false, err
		}
		return schedule, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.Schedule{}, false, err
	}
	schedule, err := s.updateScheduleTx(ctx, tx, eventID, id, expectedRevision, patch, now)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	if err := insertScheduleRequest(ctx, tx, eventID, key, "update_schedule", payload, schedule); err != nil {
		return personal.Schedule{}, false, err
	}
	outcome := "updated"
	if schedule.Revision == expectedRevision {
		outcome = "unchanged"
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.update", outcome, schedule,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", id), zap.Int64("revision", schedule.Revision)); err != nil {
		return personal.Schedule{}, false, err
	}
	return schedule, false, nil
}

func (s *Store) CancelSchedule(ctx context.Context, eventID int64, key string, id, expectedRevision int64, now time.Time) (personal.Schedule, bool, error) {
	if !requestKeyValid(key) || id <= 0 || expectedRevision <= 0 {
		return personal.Schedule{}, false, errors.New("personal: request key, schedule ID and positive revision are required")
	}
	payload, err := encodeScheduleRequest(scheduleRequestPayload{ScheduleID: id, Revision: expectedRevision})
	if err != nil {
		return personal.Schedule{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Schedule{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Schedule{}, false, err
	}
	if schedule, found, err := scheduleRequestResult(ctx, tx, eventID, key, "cancel_schedule", payload); err != nil {
		return personal.Schedule{}, false, err
	} else if found {
		if err := commitStoreOutcome(ctx, tx, "schedule.cancel", "replayed", schedule,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", schedule.ID), zap.Int64("revision", schedule.Revision)); err != nil {
			return personal.Schedule{}, false, err
		}
		return schedule, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.Schedule{}, false, err
	}
	schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, id))
	if err != nil {
		return personal.Schedule{}, false, err
	}
	if schedule.Revision != expectedRevision {
		return personal.Schedule{}, false, fmt.Errorf("personal: schedule %d revision is %d, expected %d", id, schedule.Revision, expectedRevision)
	}
	outcome := "unchanged"
	if schedule.Status != personal.ScheduleCancelled {
		if schedule.Revision == math.MaxInt64 {
			return personal.Schedule{}, false, errors.New("personal: schedule revision exhausted")
		}
		now = now.UTC()
		schedule.Revision++
		schedule.Status, schedule.NextRunAt, schedule.PauseReason = personal.ScheduleCancelled, nil, ""
		schedule.AuthorizedEventID, schedule.UpdatedAt = eventID, now
		res, err := tx.ExecContext(ctx, `UPDATE personal_schedules SET revision=?,status='cancelled',next_run_at=NULL,pause_reason='',authorized_event_id=?,updated_at=? WHERE id=? AND revision=?`,
			schedule.Revision, eventID, timestamp(now), id, expectedRevision)
		if err != nil {
			return personal.Schedule{}, false, err
		}
		if changed, err := res.RowsAffected(); err != nil || changed != 1 {
			if err != nil {
				return personal.Schedule{}, false, err
			}
			return personal.Schedule{}, false, errors.New("personal: schedule changed while cancelling")
		}
		if err := cancelUnstartedScheduleTasks(ctx, tx, id, now); err != nil {
			return personal.Schedule{}, false, err
		}
		outcome = "cancelled"
	}
	if err := insertScheduleRequest(ctx, tx, eventID, key, "cancel_schedule", payload, *schedule); err != nil {
		return personal.Schedule{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.cancel", outcome, schedule,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", id), zap.Int64("revision", schedule.Revision)); err != nil {
		return personal.Schedule{}, false, err
	}
	return *schedule, false, nil
}

// Schedule and occurrence queries.
