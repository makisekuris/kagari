package personal

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

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"kagari/internal/channel"
)

const scheduleColumns = `id,revision,scope_revision,title,goal,context,attachments,window_mode,required_sources,cron,timezone,status,next_run_at,pause_reason,task_budget_ms,baseline_at,covered_through,created_event_id,authorized_event_id,created_at,updated_at`
const occurrenceColumns = `id,schedule_id,schedule_revision,scope_revision,scheduled_at,checked_at,coalesced_until,outcome,reason,task_id,event_id,definition_json,started_at,window_start,window_end,coverage_eligible,check_result_json,credited_task_revision,credited_at`

const maxSchedulePageSize = 100
const defaultSchedulePageSize = 20
const maxScheduleTaskBudgetMS = int64(math.MaxInt64 / int64(time.Millisecond))
const maxScheduleSources = 100

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return timestamp(*value)
}

func scanOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	t, err := parseTimestamp(value.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func scanSchedule(row scanner) (*Schedule, error) {
	var schedule Schedule
	var attachments, sources string
	var nextRun, baseline, covered sql.NullString
	var created, updated string
	if err := row.Scan(&schedule.ID, &schedule.Revision, &schedule.ScopeRevision, &schedule.Title, &schedule.Goal, &schedule.Context,
		&attachments, &schedule.WindowMode, &sources, &schedule.Cron, &schedule.Timezone, &schedule.Status, &nextRun,
		&schedule.PauseReason, &schedule.TaskBudgetMS, &baseline, &covered, &schedule.CreatedEventID,
		&schedule.AuthorizedEventID, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(attachments), &schedule.Attachments); err != nil {
		return nil, fmt.Errorf("personal: decode schedule attachments: %w", err)
	}
	if err := json.Unmarshal([]byte(sources), &schedule.RequiredSources); err != nil {
		return nil, fmt.Errorf("personal: decode schedule sources: %w", err)
	}
	if schedule.Attachments == nil {
		schedule.Attachments = []channel.Attachment{}
	}
	if schedule.RequiredSources == nil {
		schedule.RequiredSources = []SourceRequirement{}
	}
	var err error
	if schedule.NextRunAt, err = scanOptionalTime(nextRun); err != nil {
		return nil, err
	}
	if schedule.BaselineAt, err = scanOptionalTime(baseline); err != nil {
		return nil, err
	}
	if schedule.CoveredThrough, err = scanOptionalTime(covered); err != nil {
		return nil, err
	}
	if schedule.CreatedAt, err = parseTimestamp(created); err != nil {
		return nil, err
	}
	if schedule.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return nil, err
	}
	return &schedule, nil
}

func scanScheduleOccurrence(row scanner) (*ScheduleOccurrence, error) {
	var occurrence ScheduleOccurrence
	var coalesced, started, windowStart, windowEnd, result, creditedAt sql.NullString
	var taskID, eventID, creditedRevision sql.NullInt64
	var definition string
	var coverage int
	var scheduled, checked string
	if err := row.Scan(&occurrence.ID, &occurrence.ScheduleID, &occurrence.ScheduleRevision, &occurrence.ScopeRevision,
		&scheduled, &checked, &coalesced, &occurrence.Outcome, &occurrence.Reason, &taskID, &eventID, &definition,
		&started, &windowStart, &windowEnd, &coverage, &result, &creditedRevision, &creditedAt); err != nil {
		return nil, err
	}
	var err error
	if occurrence.ScheduledAt, err = parseTimestamp(scheduled); err != nil {
		return nil, err
	}
	if occurrence.CheckedAt, err = parseTimestamp(checked); err != nil {
		return nil, err
	}
	if occurrence.CoalescedUntil, err = scanOptionalTime(coalesced); err != nil {
		return nil, err
	}
	if occurrence.StartedAt, err = scanOptionalTime(started); err != nil {
		return nil, err
	}
	start, err := scanOptionalTime(windowStart)
	if err != nil {
		return nil, err
	}
	end, err := scanOptionalTime(windowEnd)
	if err != nil {
		return nil, err
	}
	if (start == nil) != (end == nil) {
		return nil, errors.New("personal: schedule occurrence has an incomplete check window")
	}
	if start != nil {
		occurrence.Window = &CheckWindow{Start: *start, End: *end}
	}
	if err := json.Unmarshal([]byte(definition), &occurrence.Definition); err != nil {
		return nil, fmt.Errorf("personal: decode schedule definition: %w", err)
	}
	if occurrence.Definition.Attachments == nil {
		occurrence.Definition.Attachments = []channel.Attachment{}
	}
	if occurrence.Definition.RequiredSources == nil {
		occurrence.Definition.RequiredSources = []SourceRequirement{}
	}
	if result.Valid && result.String != "" && result.String != "null" {
		var assessment CheckAssessment
		if err := json.Unmarshal([]byte(result.String), &assessment); err != nil {
			return nil, fmt.Errorf("personal: decode schedule check result: %w", err)
		}
		occurrence.CheckResult = &assessment
	}
	occurrence.TaskID, occurrence.EventID = taskID.Int64, eventID.Int64
	occurrence.CoverageEligible = coverage != 0
	occurrence.CreditedTaskRevision = creditedRevision.Int64
	if occurrence.CreditedAt, err = scanOptionalTime(creditedAt); err != nil {
		return nil, err
	}
	return &occurrence, nil
}

type scheduleRequestPayload struct {
	ScheduleID int64          `json:"schedule_id"`
	Revision   int64          `json:"revision"`
	Input      *ScheduleInput `json:"input,omitempty"`
	Patch      *SchedulePatch `json:"patch,omitempty"`
}

func encodeScheduleRequest(payload scheduleRequestPayload) (string, error) {
	raw, err := json.Marshal(payload)
	return string(raw), err
}

func scheduleRequestResult(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string) (Schedule, bool, error) {
	var storedOperation, storedPayload string
	var scheduleID sql.NullInt64
	var result sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT operation,payload,schedule_id,result_json FROM personal_requests WHERE event_id=? AND request_key=?`, eventID, key).
		Scan(&storedOperation, &storedPayload, &scheduleID, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, false, nil
	}
	if err != nil {
		return Schedule{}, false, err
	}
	if storedOperation != operation || storedPayload != payload {
		return Schedule{}, false, errors.New("personal: request key reused with different input")
	}
	if !scheduleID.Valid || !result.Valid {
		return Schedule{}, false, errors.New("personal: saved schedule request result is incomplete")
	}
	var schedule Schedule
	if err := json.Unmarshal([]byte(result.String), &schedule); err != nil {
		return Schedule{}, false, fmt.Errorf("personal: decode saved schedule request result: %w", err)
	}
	if !scheduleID.Valid || schedule.ID != scheduleID.Int64 {
		return Schedule{}, false, errors.New("personal: saved schedule request result has an inconsistent schedule ID")
	}
	if schedule.Attachments == nil {
		schedule.Attachments = []channel.Attachment{}
	}
	if schedule.RequiredSources == nil {
		schedule.RequiredSources = []SourceRequirement{}
	}
	return schedule, true, nil
}

func insertScheduleRequest(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string, schedule Schedule) error {
	result, err := json.Marshal(schedule)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_requests(event_id,request_key,operation,payload,schedule_id,result_json) VALUES(?,?,?,?,?,?)`,
		eventID, key, operation, payload, schedule.ID, string(result))
	return err
}

func validateScheduleDefinition(title, goal, contextText string, mode WindowMode, sources []SourceRequirement) error {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(goal) == "" {
		return errors.New("personal: schedule title and goal are required")
	}
	if runeLen(title) > maxTitleChars || runeLen(goal) > maxTaskChars || runeLen(contextText) > maxTaskChars {
		return errors.New("personal: schedule input exceeds its size limit")
	}
	if mode != WindowNone && mode != WindowSinceSuccess {
		return fmt.Errorf("personal: unsupported schedule window mode %q", mode)
	}
	if len(sources) > maxScheduleSources {
		return fmt.Errorf("personal: a schedule may have at most %d required sources", maxScheduleSources)
	}
	seen := make(map[string]bool, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.ID) == "" || strings.TrimSpace(source.Label) == "" || runeLen(source.ID) > 128 ||
			runeLen(source.Label) > 200 || runeLen(source.URL) > 2048 || seen[source.ID] {
			return errors.New("personal: schedule sources require unique IDs (128 characters), labels (200 characters), and URLs (2048 characters)")
		}
		seen[source.ID] = true
	}
	return nil
}

func validateInitialCheckFrom(value *time.Time) error {
	if value != nil && value.IsZero() {
		return errors.New("personal: initial check time must not be zero")
	}
	return nil
}

func validateScheduleAttachmentIDs(ids []string) error {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			return errors.New("personal: schedule attachment IDs must be non-empty and unique")
		}
		seen[id] = true
	}
	return nil
}

func (s *Store) canonicalScheduleAttachments(ctx context.Context, tx *sql.Tx, ids []string) ([]channel.Attachment, error) {
	attachments := make([]channel.Attachment, 0, len(ids))
	for _, id := range ids {
		item, err := s.validateStoredAttachment(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, item)
	}
	return attachments, nil
}

func newScheduleBaseline(mode WindowMode, explicit *time.Time, schedule cron.Schedule, firstRun time.Time) (*time.Time, error) {
	if mode == WindowNone {
		if explicit != nil {
			return nil, errors.New("personal: initial check time requires since_success mode")
		}
		return nil, nil
	}
	var baseline time.Time
	if explicit != nil {
		baseline = explicit.UTC()
	} else {
		var err error
		baseline, err = previousScheduleTime(schedule, firstRun)
		if err != nil {
			return nil, err
		}
	}
	if !baseline.Before(firstRun) {
		return nil, errors.New("personal: initial check time must precede the next scheduled run")
	}
	return &baseline, nil
}

func (s *Store) CreateSchedule(ctx context.Context, eventID int64, key string, input ScheduleInput, now time.Time) (Schedule, bool, error) {
	if !requestKeyValid(key) {
		return Schedule{}, false, errors.New("personal: request key is required")
	}
	if input.Timezone == "" {
		input.Timezone = defaultScheduleTimezone
	}
	if input.WindowMode == "" {
		input.WindowMode = WindowNone
	}
	if err := validateScheduleDefinition(input.Title, input.Goal, input.Context, input.WindowMode, input.RequiredSources); err != nil {
		return Schedule{}, false, err
	}
	if err := validateScheduleAttachmentIDs(input.Attachments); err != nil {
		return Schedule{}, false, err
	}
	if err := validateInitialCheckFrom(input.InitialCheckFrom); err != nil {
		return Schedule{}, false, err
	}
	payload, err := encodeScheduleRequest(scheduleRequestPayload{Input: &input})
	if err != nil {
		return Schedule{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Schedule{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return Schedule{}, false, err
	}
	if schedule, found, err := scheduleRequestResult(ctx, tx, eventID, key, "create_schedule", payload); err != nil {
		return Schedule{}, false, err
	} else if found {
		if err := commitStoreOutcome(ctx, tx, "schedule.create", "replayed", schedule,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", schedule.ID), zap.Int64("revision", schedule.Revision)); err != nil {
			return Schedule{}, false, err
		}
		return schedule, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return Schedule{}, false, err
	}
	budget := s.taskBudgetMS
	if input.TaskBudgetMS != nil {
		budget = *input.TaskBudgetMS
	}
	if budget <= 0 || budget > maxScheduleTaskBudgetMS {
		return Schedule{}, false, errors.New("personal: schedule task budget is outside the supported range")
	}
	now = now.UTC()
	firstRun, err := nextScheduleTime(input.Cron, input.Timezone, now)
	if err != nil {
		return Schedule{}, false, err
	}
	spec, err := parseSchedule(input.Cron, input.Timezone)
	if err != nil {
		return Schedule{}, false, err
	}
	baseline, err := newScheduleBaseline(input.WindowMode, input.InitialCheckFrom, spec, firstRun)
	if err != nil {
		return Schedule{}, false, err
	}
	attachments, err := s.canonicalScheduleAttachments(ctx, tx, input.Attachments)
	if err != nil {
		return Schedule{}, false, err
	}
	attachmentRaw, err := json.Marshal(attachments)
	if err != nil {
		return Schedule{}, false, err
	}
	sources := input.RequiredSources
	if sources == nil {
		sources = []SourceRequirement{}
	}
	sourceRaw, err := json.Marshal(sources)
	if err != nil {
		return Schedule{}, false, err
	}
	candidate := Schedule{ScheduleDefinition: ScheduleDefinition{Title: input.Title, Goal: input.Goal, Context: input.Context,
		Attachments: attachments, Cron: input.Cron, Timezone: input.Timezone, WindowMode: input.WindowMode,
		RequiredSources: sources, TaskBudgetMS: budget}}
	if err := s.checkScheduleAttachments(ctx, tx, &candidate); err != nil {
		return Schedule{}, false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_schedules(revision,scope_revision,title,goal,context,attachments,window_mode,required_sources,cron,timezone,status,next_run_at,task_budget_ms,baseline_at,created_event_id,authorized_event_id,created_at,updated_at)
		VALUES(1,1,?,?,?,?,?,?,?,?, 'active',?,?,?,?,?,?,?)`,
		input.Title, input.Goal, input.Context, string(attachmentRaw), input.WindowMode, string(sourceRaw), input.Cron, input.Timezone,
		timestamp(firstRun), budget, nullableTime(baseline), eventID, eventID, timestamp(now), timestamp(now))
	if err != nil {
		return Schedule{}, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Schedule{}, false, err
	}
	schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, id))
	if err != nil {
		return Schedule{}, false, err
	}
	if err := insertScheduleRequest(ctx, tx, eventID, key, "create_schedule", payload, *schedule); err != nil {
		return Schedule{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.create", "created", schedule,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", schedule.ID), zap.Int64("revision", schedule.Revision)); err != nil {
		return Schedule{}, false, err
	}
	return *schedule, false, nil
}

func validateSchedulePatch(patch SchedulePatch) error {
	if err := validateInitialCheckFrom(patch.InitialCheckFrom); err != nil {
		return err
	}
	if patch.Title != nil && (strings.TrimSpace(*patch.Title) == "" || runeLen(*patch.Title) > maxTitleChars) {
		return errors.New("personal: schedule title is required and limited to 200 characters")
	}
	if patch.Goal != nil && (strings.TrimSpace(*patch.Goal) == "" || runeLen(*patch.Goal) > maxTaskChars) {
		return errors.New("personal: schedule goal is required and limited to 32000 characters")
	}
	if patch.Context != nil && runeLen(*patch.Context) > maxTaskChars {
		return errors.New("personal: schedule context exceeds its size limit")
	}
	if patch.Attachments != nil {
		if err := validateScheduleAttachmentIDs(*patch.Attachments); err != nil {
			return err
		}
	}
	if patch.WindowMode != nil && *patch.WindowMode != WindowNone && *patch.WindowMode != WindowSinceSuccess {
		return fmt.Errorf("personal: unsupported schedule window mode %q", *patch.WindowMode)
	}
	if patch.RequiredSources != nil {
		if err := validateScheduleDefinition("schedule", "goal", "", WindowNone, *patch.RequiredSources); err != nil {
			return err
		}
	}
	if patch.TaskBudgetMS != nil && (*patch.TaskBudgetMS <= 0 || *patch.TaskBudgetMS > maxScheduleTaskBudgetMS) {
		return errors.New("personal: schedule task budget is outside the supported range")
	}
	if patch.Timezone != nil && *patch.Timezone == "" {
		return errors.New("personal: schedule timezone is required")
	}
	if patch.Status != nil && *patch.Status != ScheduleActive && *patch.Status != SchedulePaused {
		return errors.New("personal: schedule status may only be active or paused")
	}
	return nil
}

func schedulePatchRequestPayload(id, revision int64, patch SchedulePatch) (string, error) {
	return encodeScheduleRequest(scheduleRequestPayload{ScheduleID: id, Revision: revision, Patch: &patch})
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *Store) updateScheduleTx(ctx context.Context, tx *sql.Tx, eventID, id, expectedRevision int64, patch SchedulePatch, now time.Time) (Schedule, error) {
	if err := validateSchedulePatch(patch); err != nil {
		return Schedule{}, err
	}
	current, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, id))
	if err != nil {
		return Schedule{}, err
	}
	if current.Revision != expectedRevision {
		return Schedule{}, fmt.Errorf("personal: schedule %d revision is %d, expected %d", id, current.Revision, expectedRevision)
	}
	if current.Status == ScheduleCancelled {
		return Schedule{}, errors.New("personal: cancelled schedules cannot be updated")
	}
	if current.Revision == math.MaxInt64 {
		return Schedule{}, errors.New("personal: schedule revision exhausted")
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
			return Schedule{}, err
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
		next.RequiredSources = append([]SourceRequirement{}, (*patch.RequiredSources)...)
	}
	if patch.TaskBudgetMS != nil {
		next.TaskBudgetMS = *patch.TaskBudgetMS
	}
	if patch.InitialCheckFrom != nil && next.WindowMode == WindowNone {
		return Schedule{}, errors.New("personal: initial check time requires since_success mode")
	}
	contentChanged := next.Goal != current.Goal || next.Context != current.Context ||
		next.WindowMode != current.WindowMode || !reflect.DeepEqual(next.Attachments, current.Attachments) ||
		!reflect.DeepEqual(next.RequiredSources, current.RequiredSources) ||
		(patch.InitialCheckFrom != nil && (current.BaselineAt == nil || !patch.InitialCheckFrom.Equal(*current.BaselineAt) || current.CoveredThrough != nil))
	cronChanged := next.Cron != current.Cron || next.Timezone != current.Timezone
	manualPause := patch.Status != nil && *patch.Status == SchedulePaused &&
		(current.Status != SchedulePaused || current.PauseReason != "manual")
	manualResume := current.Status == SchedulePaused && current.PauseReason == "manual" && patch.Status != nil && *patch.Status == ScheduleActive
	resume := current.Status == SchedulePaused && patch.Status != nil && *patch.Status == ScheduleActive
	newStatus := current.Status
	newPauseReason := current.PauseReason
	if manualPause {
		newStatus, newPauseReason = SchedulePaused, "manual"
	} else if resume {
		newStatus, newPauseReason = ScheduleActive, ""
	}
	stateChanged := next.Title != current.Title || contentChanged || cronChanged || next.TaskBudgetMS != current.TaskBudgetMS ||
		newStatus != current.Status || newPauseReason != current.PauseReason
	if !stateChanged {
		return *current, nil
	}
	if cronChanged {
		if _, err := nextScheduleTime(next.Cron, next.Timezone, now); err != nil {
			return Schedule{}, err
		}
	}
	if resume || patch.Attachments != nil {
		if err := s.checkScheduleAttachments(ctx, tx, &next); err != nil {
			return Schedule{}, err
		}
	}
	if current.ScopeRevision == math.MaxInt64 && (contentChanged || manualResume) {
		return Schedule{}, errors.New("personal: schedule scope revision exhausted")
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
	if next.Status == ScheduleActive {
		if current.Status != ScheduleActive || cronChanged || current.NextRunAt == nil || !current.NextRunAt.After(now) {
			first, err := nextScheduleTime(next.Cron, next.Timezone, now)
			if err != nil {
				return Schedule{}, err
			}
			firstActiveRun = &first
		} else {
			firstActiveRun = current.NextRunAt
		}
	}
	if next.WindowMode == WindowNone {
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
		} else if next.Status == SchedulePaused {
			next.BaselineAt = nil
		} else {
			spec, err := parseSchedule(next.Cron, next.Timezone)
			if err != nil {
				return Schedule{}, err
			}
			next.BaselineAt, err = newScheduleBaseline(next.WindowMode, nil, spec, *firstActiveRun)
			if err != nil {
				return Schedule{}, err
			}
		}
	} else if next.WindowMode == WindowSinceSuccess && next.BaselineAt == nil && resume {
		spec, err := parseSchedule(next.Cron, next.Timezone)
		if err != nil {
			return Schedule{}, err
		}
		next.BaselineAt, err = newScheduleBaseline(next.WindowMode, nil, spec, *firstActiveRun)
		if err != nil {
			return Schedule{}, err
		}
	}
	if patch.InitialCheckFrom != nil && next.WindowMode == WindowSinceSuccess {
		if next.BaselineAt == nil || *patch.InitialCheckFrom != *next.BaselineAt {
			baseline := patch.InitialCheckFrom.UTC()
			if manualResume {
				baseline = maxTime(baseline, now)
			}
			next.BaselineAt = &baseline
		}
	}
	if next.WindowMode == WindowSinceSuccess && next.BaselineAt != nil {
		limit := next.NextRunAt
		if next.Status == ScheduleActive {
			limit = firstActiveRun
		}
		if limit != nil && !next.BaselineAt.Before(*limit) {
			return Schedule{}, errors.New("personal: initial check time must precede the next scheduled run")
		}
	}
	if manualPause {
		next.NextRunAt = nil
	} else if next.Status == ScheduleActive && advanceNextRun {
		next.NextRunAt = firstActiveRun
	}
	if next.Status == SchedulePaused {
		next.NextRunAt = nil
	}
	attachmentsRaw, err := json.Marshal(next.Attachments)
	if err != nil {
		return Schedule{}, err
	}
	sourcesRaw, err := json.Marshal(next.RequiredSources)
	if err != nil {
		return Schedule{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE personal_schedules SET revision=?,scope_revision=?,title=?,goal=?,context=?,attachments=?,window_mode=?,required_sources=?,cron=?,timezone=?,status=?,next_run_at=?,pause_reason=?,task_budget_ms=?,baseline_at=?,covered_through=?,authorized_event_id=?,updated_at=? WHERE id=? AND revision=?`,
		next.Revision, next.ScopeRevision, next.Title, next.Goal, next.Context, string(attachmentsRaw), next.WindowMode, string(sourcesRaw),
		next.Cron, next.Timezone, next.Status, nullableTime(next.NextRunAt), next.PauseReason, next.TaskBudgetMS,
		nullableTime(next.BaselineAt), nullableTime(next.CoveredThrough), eventID, timestamp(now), id, expectedRevision)
	if err != nil {
		return Schedule{}, err
	}
	if changed, err := res.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return Schedule{}, err
		}
		return Schedule{}, errors.New("personal: schedule changed while updating")
	}
	if contentChanged || manualPause {
		if err := cancelUnstartedScheduleTasks(ctx, tx, id, now); err != nil {
			return Schedule{}, err
		}
	}
	return next, nil
}

func (s *Store) UpdateSchedule(ctx context.Context, eventID int64, key string, id, expectedRevision int64, patch SchedulePatch, now time.Time) (Schedule, bool, error) {
	if !requestKeyValid(key) || id <= 0 || expectedRevision <= 0 {
		return Schedule{}, false, errors.New("personal: request key, schedule ID and positive revision are required")
	}
	if err := validateSchedulePatch(patch); err != nil {
		return Schedule{}, false, err
	}
	payload, err := schedulePatchRequestPayload(id, expectedRevision, patch)
	if err != nil {
		return Schedule{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Schedule{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return Schedule{}, false, err
	}
	if schedule, found, err := scheduleRequestResult(ctx, tx, eventID, key, "update_schedule", payload); err != nil {
		return Schedule{}, false, err
	} else if found {
		if err := commitStoreOutcome(ctx, tx, "schedule.update", "replayed", schedule,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", schedule.ID), zap.Int64("revision", schedule.Revision)); err != nil {
			return Schedule{}, false, err
		}
		return schedule, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return Schedule{}, false, err
	}
	schedule, err := s.updateScheduleTx(ctx, tx, eventID, id, expectedRevision, patch, now)
	if err != nil {
		return Schedule{}, false, err
	}
	if err := insertScheduleRequest(ctx, tx, eventID, key, "update_schedule", payload, schedule); err != nil {
		return Schedule{}, false, err
	}
	outcome := "updated"
	if schedule.Revision == expectedRevision {
		outcome = "unchanged"
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.update", outcome, schedule,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", id), zap.Int64("revision", schedule.Revision)); err != nil {
		return Schedule{}, false, err
	}
	return schedule, false, nil
}

func (s *Store) CancelSchedule(ctx context.Context, eventID int64, key string, id, expectedRevision int64, now time.Time) (Schedule, bool, error) {
	if !requestKeyValid(key) || id <= 0 || expectedRevision <= 0 {
		return Schedule{}, false, errors.New("personal: request key, schedule ID and positive revision are required")
	}
	payload, err := encodeScheduleRequest(scheduleRequestPayload{ScheduleID: id, Revision: expectedRevision})
	if err != nil {
		return Schedule{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Schedule{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return Schedule{}, false, err
	}
	if schedule, found, err := scheduleRequestResult(ctx, tx, eventID, key, "cancel_schedule", payload); err != nil {
		return Schedule{}, false, err
	} else if found {
		if err := commitStoreOutcome(ctx, tx, "schedule.cancel", "replayed", schedule,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", schedule.ID), zap.Int64("revision", schedule.Revision)); err != nil {
			return Schedule{}, false, err
		}
		return schedule, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return Schedule{}, false, err
	}
	schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, id))
	if err != nil {
		return Schedule{}, false, err
	}
	if schedule.Revision != expectedRevision {
		return Schedule{}, false, fmt.Errorf("personal: schedule %d revision is %d, expected %d", id, schedule.Revision, expectedRevision)
	}
	outcome := "unchanged"
	if schedule.Status != ScheduleCancelled {
		if schedule.Revision == math.MaxInt64 {
			return Schedule{}, false, errors.New("personal: schedule revision exhausted")
		}
		now = now.UTC()
		schedule.Revision++
		schedule.Status, schedule.NextRunAt, schedule.PauseReason = ScheduleCancelled, nil, ""
		schedule.AuthorizedEventID, schedule.UpdatedAt = eventID, now
		res, err := tx.ExecContext(ctx, `UPDATE personal_schedules SET revision=?,status='cancelled',next_run_at=NULL,pause_reason='',authorized_event_id=?,updated_at=? WHERE id=? AND revision=?`,
			schedule.Revision, eventID, timestamp(now), id, expectedRevision)
		if err != nil {
			return Schedule{}, false, err
		}
		if changed, err := res.RowsAffected(); err != nil || changed != 1 {
			if err != nil {
				return Schedule{}, false, err
			}
			return Schedule{}, false, errors.New("personal: schedule changed while cancelling")
		}
		if err := cancelUnstartedScheduleTasks(ctx, tx, id, now); err != nil {
			return Schedule{}, false, err
		}
		outcome = "cancelled"
	}
	if err := insertScheduleRequest(ctx, tx, eventID, key, "cancel_schedule", payload, *schedule); err != nil {
		return Schedule{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.cancel", outcome, schedule,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", id), zap.Int64("revision", schedule.Revision)); err != nil {
		return Schedule{}, false, err
	}
	return *schedule, false, nil
}

func (s *Store) Schedule(ctx context.Context, id int64) (*Schedule, error) {
	if id <= 0 {
		return nil, nil
	}
	schedule, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return schedule, err
}

func (s *Store) Schedules(ctx context.Context, includeCancelled bool, beforeID int64, limit int) ([]Schedule, error) {
	if beforeID < 0 || limit < 0 {
		return nil, errors.New("personal: schedule cursor and limit cannot be negative")
	}
	if limit == 0 {
		limit = defaultSchedulePageSize
	}
	if limit > maxSchedulePageSize {
		limit = maxSchedulePageSize
	}
	statusFilter := `status<>'cancelled'`
	if includeCancelled {
		statusFilter = `1=1`
	}
	query := `SELECT ` + scheduleColumns + ` FROM personal_schedules WHERE ` + statusFilter
	if beforeID > 0 {
		query += ` AND id<?`
	}
	query += ` ORDER BY id DESC LIMIT ?`
	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = s.db.QueryContext(ctx, query, beforeID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, query, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	schedules := make([]Schedule, 0, limit)
	for rows.Next() {
		schedule, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, *schedule)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return schedules, nil
}

func (s *Store) ScheduleOccurrences(ctx context.Context, id, beforeID int64, limit int) ([]ScheduleOccurrence, error) {
	if id <= 0 || beforeID < 0 || limit < 0 {
		return nil, errors.New("personal: schedule ID must be positive and cursor and limit non-negative")
	}
	if limit == 0 {
		limit = defaultSchedulePageSize
	}
	if limit > maxSchedulePageSize {
		limit = maxSchedulePageSize
	}
	query := `SELECT ` + occurrenceColumns + ` FROM personal_schedule_occurrences WHERE schedule_id=?`
	if beforeID > 0 {
		query += ` AND id<?`
	}
	query += ` ORDER BY id DESC LIMIT ?`
	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = s.db.QueryContext(ctx, query, id, beforeID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, query, id, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	occurrences := make([]ScheduleOccurrence, 0, limit)
	for rows.Next() {
		occurrence, err := scanScheduleOccurrence(rows)
		if err != nil {
			return nil, err
		}
		occurrences = append(occurrences, *occurrence)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range occurrences {
		if err := s.occurrenceTaskState(ctx, &occurrences[i]); err != nil {
			return nil, err
		}
	}
	return occurrences, nil
}

func (s *Store) ScheduleForTask(ctx context.Context, taskID int64) (*ScheduleContext, error) {
	if taskID <= 0 {
		return nil, nil
	}
	occurrence, err := scanScheduleOccurrence(s.db.QueryRowContext(ctx, `SELECT `+occurrenceColumns+` FROM personal_schedule_occurrences WHERE task_id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	schedule, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, occurrence.ScheduleID))
	if err != nil {
		return nil, err
	}
	if err := s.occurrenceTaskState(ctx, occurrence); err != nil {
		return nil, err
	}
	return &ScheduleContext{Schedule: *schedule, Occurrence: *occurrence}, nil
}

func (s *Store) occurrenceTaskState(ctx context.Context, o *ScheduleOccurrence) error {
	if o.TaskID == 0 {
		return nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT status,summary FROM personal_tasks WHERE id=?`, o.TaskID).Scan(&o.TaskStatus, &o.TaskSummary); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.status,COUNT(*) FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id WHERE e.task_id=? GROUP BY d.status`, o.TaskID)
	if err != nil {
		return err
	}
	defer rows.Close()
	o.DeliveryStates = make(map[string]int)
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return err
		}
		o.DeliveryStates[state] = count
	}
	return rows.Err()
}
