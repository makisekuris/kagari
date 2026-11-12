package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robfig/cron/v3"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

const scheduleColumns = `id,revision,scope_revision,title,goal,context,attachments,window_mode,required_sources,cron,timezone,status,next_run_at,pause_reason,task_budget_ms,baseline_at,covered_through,created_event_id,authorized_event_id,created_at,updated_at`
const occurrenceColumns = `id,schedule_id,schedule_revision,scope_revision,scheduled_at,checked_at,coalesced_until,outcome,reason,task_id,event_id,definition_json,started_at,window_start,window_end,coverage_eligible,check_result_json,credited_task_revision,credited_at`

const maxScheduleTaskBudgetMS = int64(math.MaxInt64 / int64(time.Millisecond))
const maxScheduleSources = 100

// Schedule decoding, request identity, and input validation.
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

func scanSchedule(row scanner) (*personal.Schedule, error) {
	var schedule personal.Schedule
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
		schedule.RequiredSources = []personal.SourceRequirement{}
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

func scanScheduleOccurrence(row scanner) (*personal.ScheduleOccurrence, error) {
	var occurrence personal.ScheduleOccurrence
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
		occurrence.Window = &personal.CheckWindow{Start: *start, End: *end}
	}
	if err := json.Unmarshal([]byte(definition), &occurrence.Definition); err != nil {
		return nil, fmt.Errorf("personal: decode schedule definition: %w", err)
	}
	if occurrence.Definition.Attachments == nil {
		occurrence.Definition.Attachments = []channel.Attachment{}
	}
	if occurrence.Definition.RequiredSources == nil {
		occurrence.Definition.RequiredSources = []personal.SourceRequirement{}
	}
	if result.Valid && result.String != "" && result.String != "null" {
		var assessment personal.CheckAssessment
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
	ScheduleID int64                   `json:"schedule_id"`
	Revision   int64                   `json:"revision"`
	Input      *personal.ScheduleInput `json:"input,omitempty"`
	Patch      *personal.SchedulePatch `json:"patch,omitempty"`
}

func encodeScheduleRequest(payload scheduleRequestPayload) (string, error) {
	raw, err := json.Marshal(payload)
	return string(raw), err
}

func scheduleRequestResult(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string) (personal.Schedule, bool, error) {
	var storedOperation, storedPayload string
	var scheduleID sql.NullInt64
	var result sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT operation,payload,schedule_id,result_json FROM personal_requests WHERE event_id=? AND request_key=?`, eventID, key).
		Scan(&storedOperation, &storedPayload, &scheduleID, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return personal.Schedule{}, false, nil
	}
	if err != nil {
		return personal.Schedule{}, false, err
	}
	if storedOperation != operation || storedPayload != payload {
		return personal.Schedule{}, false, errors.New("personal: request key reused with different input")
	}
	if !scheduleID.Valid || !result.Valid {
		return personal.Schedule{}, false, errors.New("personal: saved schedule request result is incomplete")
	}
	var schedule personal.Schedule
	if err := json.Unmarshal([]byte(result.String), &schedule); err != nil {
		return personal.Schedule{}, false, fmt.Errorf("personal: decode saved schedule request result: %w", err)
	}
	if !scheduleID.Valid || schedule.ID != scheduleID.Int64 {
		return personal.Schedule{}, false, errors.New("personal: saved schedule request result has an inconsistent schedule ID")
	}
	if schedule.Attachments == nil {
		schedule.Attachments = []channel.Attachment{}
	}
	if schedule.RequiredSources == nil {
		schedule.RequiredSources = []personal.SourceRequirement{}
	}
	return schedule, true, nil
}

func insertScheduleRequest(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string, schedule personal.Schedule) error {
	result, err := json.Marshal(schedule)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_requests(event_id,request_key,operation,payload,schedule_id,result_json) VALUES(?,?,?,?,?,?)`,
		eventID, key, operation, payload, schedule.ID, string(result))
	return err
}

func validateScheduleDefinition(title, goal, contextText string, mode personal.WindowMode, sources []personal.SourceRequirement) error {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(goal) == "" {
		return errors.New("personal: schedule title and goal are required")
	}
	if utf8.RuneCountInString(title) > maxTitleChars || utf8.RuneCountInString(goal) > personal.MaxTaskChars || utf8.RuneCountInString(contextText) > personal.MaxTaskChars {
		return errors.New("personal: schedule input exceeds its size limit")
	}
	if mode != personal.WindowNone && mode != personal.WindowSinceSuccess {
		return fmt.Errorf("personal: unsupported schedule window mode %q", mode)
	}
	if len(sources) > maxScheduleSources {
		return fmt.Errorf("personal: a schedule may have at most %d required sources", maxScheduleSources)
	}
	seen := make(map[string]bool, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.ID) == "" || strings.TrimSpace(source.Label) == "" || utf8.RuneCountInString(source.ID) > 128 ||
			utf8.RuneCountInString(source.Label) > 200 || utf8.RuneCountInString(source.URL) > 2048 || seen[source.ID] {
			return errors.New("personal: schedule sources require unique IDs (128 characters), labels (200 characters), and URLs (2048 characters)")
		}
		id := strings.ToLower(strings.TrimSpace(source.ID))
		if strings.TrimSpace(source.URL) == "" && (strings.HasPrefix(id, "http://") || strings.HasPrefix(id, "https://")) {
			return errors.New("personal: HTTP(S) source URLs must be supplied in url, not only in id")
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

func newScheduleBaseline(mode personal.WindowMode, explicit *time.Time, schedule cron.Schedule, firstRun time.Time) (*time.Time, error) {
	if mode == personal.WindowNone {
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
		baseline, err = personal.PreviousScheduleTime(schedule, firstRun)
		if err != nil {
			return nil, err
		}
	}
	if !baseline.Before(firstRun) {
		return nil, errors.New("personal: initial check time must precede the next scheduled run")
	}
	return &baseline, nil
}

// Schedule creation and revision transitions.
