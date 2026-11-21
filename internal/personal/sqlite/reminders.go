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

	"go.uber.org/zap"

	"kagari/internal/personal"
)

type reminderRequestPayload struct {
	ReminderID int64                   `json:"reminder_id,omitempty"`
	Input      *personal.ReminderInput `json:"input,omitempty"`
}

func encodeReminderRequest(payload reminderRequestPayload) (string, error) {
	raw, err := json.Marshal(payload)
	return string(raw), err
}

func validateReminderInput(input personal.ReminderInput) error {
	if strings.TrimSpace(input.Title) == "" || !utf8.ValidString(input.Title) || utf8.RuneCountInString(input.Title) > personal.MaxReminderTitleChars {
		return fmt.Errorf("personal: reminder title is required and limited to %d characters", personal.MaxReminderTitleChars)
	}
	if len(input.Steps) == 0 || len(input.Steps) > personal.MaxReminderSteps {
		return fmt.Errorf("personal: a reminder must have between 1 and %d steps", personal.MaxReminderSteps)
	}
	if input.Cron != "" {
		if input.StartAt != nil {
			return errors.New("personal: calendar reminders cannot set start_at")
		}
		if input.Repeat {
			return errors.New("personal: calendar reminder recurrence is defined by cron, not repeat")
		}
		if len(input.Steps) != 1 || input.Steps[0].AfterMinutes != 0 {
			return errors.New("personal: a calendar reminder requires one fixed-text step with after_minutes=0")
		}
		if strings.TrimSpace(input.Steps[0].Text) == "" || !utf8.ValidString(input.Steps[0].Text) || utf8.RuneCountInString(input.Steps[0].Text) > personal.MaxReminderTextChars {
			return fmt.Errorf("personal: calendar reminder text is required and limited to %d characters", personal.MaxReminderTextChars)
		}
		if _, err := personal.ParseSchedule(input.Cron, input.Timezone); err != nil {
			return err
		}
		return nil
	}
	if input.Timezone != "" {
		return errors.New("personal: timezone requires a calendar reminder cron")
	}
	var totalMinutes int64
	maxMinutes := int64(math.MaxInt64 / int64(time.Minute))
	for index, step := range input.Steps {
		if step.AfterMinutes <= 0 {
			return fmt.Errorf("personal: reminder step %d must have a positive after_minutes", index+1)
		}
		if int64(step.AfterMinutes) > maxMinutes-totalMinutes {
			return errors.New("personal: reminder intervals exceed the supported time range")
		}
		totalMinutes += int64(step.AfterMinutes)
		if strings.TrimSpace(step.Text) == "" || !utf8.ValidString(step.Text) || utf8.RuneCountInString(step.Text) > personal.MaxReminderTextChars {
			return fmt.Errorf("personal: reminder step %d text is required and limited to %d characters", index+1, personal.MaxReminderTextChars)
		}
	}
	if input.StartAt != nil && input.StartAt.IsZero() {
		return errors.New("personal: reminder start_at cannot be zero")
	}
	return nil
}

func reminderRequestResult(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string) (personal.Reminder, bool, error) {
	var storedOperation, storedPayload, result string
	var reminderID int64
	err := tx.QueryRowContext(ctx, `SELECT operation,payload,reminder_id,result_json FROM personal_reminder_requests WHERE event_id=? AND request_key=?`, eventID, key).
		Scan(&storedOperation, &storedPayload, &reminderID, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return personal.Reminder{}, false, nil
	}
	if err != nil {
		return personal.Reminder{}, false, err
	}
	if storedOperation != operation || storedPayload != payload {
		return personal.Reminder{}, false, errors.New("personal: request key reused with different input")
	}
	var reminder personal.Reminder
	if err := json.Unmarshal([]byte(result), &reminder); err != nil {
		return personal.Reminder{}, false, fmt.Errorf("personal: decode saved reminder request result: %w", err)
	}
	if reminder.ID != reminderID {
		return personal.Reminder{}, false, errors.New("personal: saved reminder request result has an inconsistent reminder ID")
	}
	if reminder.Steps == nil {
		reminder.Steps = []personal.ReminderStep{}
	}
	return reminder, true, nil
}

func insertReminderRequest(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string, reminder personal.Reminder) error {
	result, err := json.Marshal(reminder)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_reminder_requests(event_id,request_key,operation,reminder_id,result_json,payload) VALUES(?,?,?,?,?,?)`,
		eventID, key, operation, reminder.ID, string(result), payload)
	return err
}

func (s *Store) CreateReminder(ctx context.Context, eventID int64, key string, input personal.ReminderInput, now time.Time) (personal.Reminder, bool, error) {
	if !requestKeyValid(key) {
		return personal.Reminder{}, false, errors.New("personal: request key is required")
	}
	if input.Cron != "" && input.Timezone == "" {
		input.Timezone = personal.DefaultScheduleTimezone
	}
	if err := validateReminderInput(input); err != nil {
		return personal.Reminder{}, false, err
	}
	payload, err := encodeReminderRequest(reminderRequestPayload{Input: &input})
	if err != nil {
		return personal.Reminder{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Reminder{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Reminder{}, false, err
	}
	if reminder, found, err := reminderRequestResult(ctx, tx, eventID, key, "create_reminder", payload); err != nil {
		return personal.Reminder{}, false, err
	} else if found {
		if err := commitStoreOutcome(ctx, tx, "reminder.create", "replayed", reminder,
			zap.Int64("event_id", eventID), zap.Int64("reminder_id", reminder.ID)); err != nil {
			return personal.Reminder{}, false, err
		}
		return reminder, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.Reminder{}, false, err
	}
	if now.IsZero() {
		return personal.Reminder{}, false, errors.New("personal: reminder creation time is required")
	}
	now = now.UTC()
	startAt := now
	if input.StartAt != nil {
		startAt = input.StartAt.UTC()
	}
	nextRunAt := startAt.Add(time.Duration(input.Steps[0].AfterMinutes) * time.Minute)
	if input.Cron != "" {
		nextRunAt, err = personal.NextScheduleTime(input.Cron, input.Timezone, now)
		if err != nil {
			return personal.Reminder{}, false, err
		}
	}
	stepsRaw, err := json.Marshal(input.Steps)
	if err != nil {
		return personal.Reminder{}, false, err
	}
	repeat := 0
	if input.Repeat {
		repeat = 1
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_reminders(title,steps_json,repeat,start_at,cron,timezone,status,next_run_at,next_step,created_event_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,'active',?,0,?,?,?)`, input.Title, string(stepsRaw), repeat, timestamp(startAt), input.Cron, input.Timezone, timestamp(nextRunAt), eventID, timestamp(now), timestamp(now))
	if err != nil {
		return personal.Reminder{}, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return personal.Reminder{}, false, err
	}
	reminder, err := scanReminder(tx.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM personal_reminders WHERE id=?`, id))
	if err != nil {
		return personal.Reminder{}, false, err
	}
	if err := insertReminderRequest(ctx, tx, eventID, key, "create_reminder", payload, *reminder); err != nil {
		return personal.Reminder{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "reminder.create", "created", reminder,
		zap.Int64("event_id", eventID), zap.Int64("reminder_id", reminder.ID)); err != nil {
		return personal.Reminder{}, false, err
	}
	return *reminder, false, nil
}

func (s *Store) CancelReminder(ctx context.Context, eventID int64, key string, id int64, now time.Time) (personal.Reminder, bool, error) {
	if !requestKeyValid(key) || id <= 0 {
		return personal.Reminder{}, false, errors.New("personal: request key and positive reminder ID are required")
	}
	payload, err := encodeReminderRequest(reminderRequestPayload{ReminderID: id})
	if err != nil {
		return personal.Reminder{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Reminder{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Reminder{}, false, err
	}
	if reminder, found, err := reminderRequestResult(ctx, tx, eventID, key, "cancel_reminder", payload); err != nil {
		return personal.Reminder{}, false, err
	} else if found {
		if err := commitStoreOutcome(ctx, tx, "reminder.cancel", "replayed", reminder,
			zap.Int64("event_id", eventID), zap.Int64("reminder_id", reminder.ID)); err != nil {
			return personal.Reminder{}, false, err
		}
		return reminder, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.Reminder{}, false, err
	}
	if now.IsZero() {
		return personal.Reminder{}, false, errors.New("personal: reminder cancellation time is required")
	}
	reminder, err := scanReminder(tx.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM personal_reminders WHERE id=?`, id))
	if err != nil {
		return personal.Reminder{}, false, err
	}
	if reminder.LastDeliveryID > 0 {
		if err := suppressDeliveryTx(ctx, tx, reminder.LastDeliveryID, "reminder_cancelled"); err != nil {
			return personal.Reminder{}, false, err
		}
	}
	if reminder.Status == "active" {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_reminders SET status='cancelled',next_run_at=NULL,updated_at=? WHERE id=? AND status='active'`, timestamp(now), id); err != nil {
			return personal.Reminder{}, false, err
		}
		reminder, err = scanReminder(tx.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM personal_reminders WHERE id=?`, id))
		if err != nil {
			return personal.Reminder{}, false, err
		}
	}
	if err := insertReminderRequest(ctx, tx, eventID, key, "cancel_reminder", payload, *reminder); err != nil {
		return personal.Reminder{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "reminder.cancel", "saved", reminder,
		zap.Int64("event_id", eventID), zap.Int64("reminder_id", reminder.ID)); err != nil {
		return personal.Reminder{}, false, err
	}
	return *reminder, false, nil
}

func (s *Store) Reminders(ctx context.Context, activeOnly bool, beforeID int64, limit int) ([]personal.Reminder, error) {
	if beforeID < 0 || limit < 0 {
		return nil, errors.New("personal: reminder cursor and limit cannot be negative")
	}
	if limit == 0 {
		limit = personal.DefaultReminderPageSize
	}
	if limit > personal.MaxReminderPageSize {
		limit = personal.MaxReminderPageSize
	}
	query := `SELECT ` + reminderColumns + ` FROM personal_reminders WHERE 1=1`
	if activeOnly {
		query += ` AND status='active'`
	}
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
	reminders := make([]personal.Reminder, 0, limit)
	for rows.Next() {
		reminder, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		reminders = append(reminders, *reminder)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return reminders, nil
}

const reminderColumns = `id,title,steps_json,repeat,start_at,cron,timezone,status,next_run_at,next_step,created_event_id,last_delivery_id,created_at,updated_at`

func scanReminder(row scanner) (*personal.Reminder, error) {
	var reminder personal.Reminder
	var stepsJSON, startAt, createdAt, updatedAt string
	var repeat int
	var cron, timezone string
	var nextRunAt sql.NullString
	var nextStep int
	var createdEventID int64
	var lastDeliveryID sql.NullInt64
	if err := row.Scan(&reminder.ID, &reminder.Title, &stepsJSON, &repeat, &startAt, &cron, &timezone, &reminder.Status, &nextRunAt, &nextStep, &createdEventID, &lastDeliveryID, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if repeat != 0 && repeat != 1 {
		return nil, errors.New("personal: stored reminder has an invalid repeat flag")
	}
	reminder.Repeat = repeat == 1
	reminder.CreatedEventID = createdEventID
	reminder.LastDeliveryID = lastDeliveryID.Int64
	reminder.Cron, reminder.Timezone = cron, timezone
	if err := json.Unmarshal([]byte(stepsJSON), &reminder.Steps); err != nil {
		return nil, fmt.Errorf("personal: decode stored reminder steps: %w", err)
	}
	if reminder.Steps == nil {
		reminder.Steps = []personal.ReminderStep{}
	}
	var err error
	reminder.StartAt, err = parseTimestamp(startAt)
	if err != nil {
		return nil, err
	}
	reminder.CreatedAt, err = parseTimestamp(createdAt)
	if err != nil {
		return nil, err
	}
	reminder.UpdatedAt, err = parseTimestamp(updatedAt)
	if err != nil {
		return nil, err
	}
	if nextRunAt.Valid {
		runAt, err := parseTimestamp(nextRunAt.String)
		if err != nil {
			return nil, err
		}
		reminder.NextRunAt = &runAt
	}
	if reminder.Status == "active" && (nextStep < 0 || nextStep >= len(reminder.Steps)) {
		return nil, errors.New("personal: stored active reminder has an invalid next step")
	}
	reminder.NextStep = nextStep
	return &reminder, nil
}
