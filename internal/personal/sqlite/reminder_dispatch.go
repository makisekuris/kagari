package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"go.uber.org/zap"

	"kagari/internal/personal"
)

// DispatchDueReminder writes one latest-due step to the delivery outbox and
// advances the reminder in the same transaction. A delayed scheduler therefore
// never emits every missed step, and a restart cannot enqueue the same step twice.
func (s *Store) DispatchDueReminder(ctx context.Context, now time.Time) (bool, error) {
	if now.IsZero() {
		return false, errors.New("personal: reminder dispatch time is required")
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	reminder, err := scanReminder(tx.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM personal_reminders
		WHERE status='active' AND next_run_at<=? AND EXISTS(SELECT 1 FROM personal_events e
		WHERE e.id=created_event_id AND e.kind='user' AND e.status='completed')
		AND NOT EXISTS(SELECT 1 FROM personal_outbox pending WHERE pending.delivery_id=last_delivery_id AND pending.status='sending')
		ORDER BY next_run_at,id LIMIT 1`, timestamp(now)))
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if reminder.NextRunAt == nil || reminder.NextStep < 0 || reminder.NextStep >= len(reminder.Steps) {
		return false, errors.New("personal: due reminder has invalid persisted progress")
	}
	if reminder.LastDeliveryID > 0 {
		var pending, sending int
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status='sending' THEN 1 ELSE 0 END),0)
			FROM personal_outbox WHERE delivery_id=?`, reminder.LastDeliveryID).Scan(&pending, &sending); err != nil {
			return false, err
		}
		if sending > 0 {
			if err := tx.Commit(); err != nil {
				return false, err
			}
			return false, nil
		}
		if pending > 0 {
			if err := suppressDeliveryTx(ctx, tx, reminder.LastDeliveryID, "reminder_superseded"); err != nil {
				return false, err
			}
		}
	}
	dueAt := *reminder.NextRunAt
	var stepIndex, nextStep int
	var stepAt, nextAt time.Time
	var completed bool
	if reminder.Cron != "" {
		stepAt, nextAt, err = latestDueCalendarReminder(*reminder, now)
	} else {
		stepIndex, stepAt, nextStep, nextAt, completed, err = latestDueReminderStep(*reminder, now)
	}
	if err != nil {
		return false, err
	}
	step := reminder.Steps[stepIndex]
	intent, err := insertDeliveryIntentTx(ctx, tx, deliveryIntentInsert{
		eventID: reminder.CreatedEventID, purpose: personal.DeliveryStatus,
		draft:  personal.DeliveryDraft{Text: step.Text, Attachments: []string{}},
		detail: "reminder",
	}, now)
	if err != nil {
		return false, err
	}
	if _, err := appendDeliveryPartsTx(ctx, tx, reminder.CreatedEventID, intent.ID, step.Text, nil, []string{step.Text}, 0, 0); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_outbox SET next_attempt_at=? WHERE delivery_id=?`, timestamp(now), intent.ID); err != nil {
		return false, err
	}
	if err := updateDeliveryIntentRenderedTx(ctx, tx, intent.ID, ""); err != nil {
		return false, err
	}
	status := "active"
	var nextRun any = timestamp(nextAt)
	if completed {
		status = "completed"
		nextRun = nil
		nextAt = time.Time{}
		nextStep = stepIndex
	}
	updated, err := tx.ExecContext(ctx, `UPDATE personal_reminders SET status=?,next_run_at=?,next_step=?,last_delivery_id=?,updated_at=? WHERE id=? AND status='active' AND next_run_at=? AND next_step=?`,
		status, nextRun, nextStep, intent.ID, timestamp(now), reminder.ID, timestamp(dueAt), reminder.NextStep)
	if err != nil {
		return false, err
	}
	if changed, err := updated.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return false, err
		}
		return false, errors.New("personal: due reminder changed during dispatch")
	}
	if err := commitStoreOutcome(ctx, tx, "reminder.dispatch", "enqueued", map[string]any{
		"reminder_id": reminder.ID, "step": stepIndex, "scheduled_at": stepAt, "delivery_id": intent.ID, "status": status,
	}, zap.Int64("reminder_id", reminder.ID), zap.Int64("event_id", reminder.CreatedEventID), zap.Int64("delivery_id", intent.ID)); err != nil {
		return false, err
	}
	return true, nil
}

func latestDueCalendarReminder(reminder personal.Reminder, now time.Time) (scheduledAt, nextAt time.Time, err error) {
	schedule, err := personal.ParseSchedule(reminder.Cron, reminder.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	scheduledAt, err = personal.PreviousScheduleTime(schedule, now.Add(time.Nanosecond))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	nextAt, err = personal.NextScheduleTime(reminder.Cron, reminder.Timezone, now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return scheduledAt, nextAt, nil
}

func latestDueReminderStep(reminder personal.Reminder, now time.Time) (stepIndex int, stepAt time.Time, nextStep int, nextAt time.Time, completed bool, err error) {
	if reminder.NextRunAt == nil || reminder.NextStep < 0 || reminder.NextStep >= len(reminder.Steps) || now.Before(*reminder.NextRunAt) {
		return 0, time.Time{}, 0, time.Time{}, false, errors.New("personal: reminder is not due")
	}
	stepIndex, stepAt = reminder.NextStep, *reminder.NextRunAt
	if !reminder.Repeat {
		for {
			nextStep = stepIndex + 1
			if nextStep >= len(reminder.Steps) {
				return stepIndex, stepAt, stepIndex, time.Time{}, true, nil
			}
			nextAt = stepAt.Add(time.Duration(reminder.Steps[nextStep].AfterMinutes) * time.Minute)
			if nextAt.After(now) {
				return stepIndex, stepAt, nextStep, nextAt, false, nil
			}
			stepIndex, stepAt = nextStep, nextAt
		}
	}

	periodMinutes := int64(0)
	for _, step := range reminder.Steps {
		periodMinutes += int64(step.AfterMinutes)
	}
	period := time.Duration(periodMinutes) * time.Minute
	if period <= 0 {
		return 0, time.Time{}, 0, time.Time{}, false, errors.New("personal: repeating reminder has an invalid period")
	}
	elapsed := now.Sub(stepAt)
	cycles := elapsed / period
	remaining := elapsed % period
	stepAt = stepAt.Add(cycles * period)
	for range reminder.Steps {
		candidateStep := (stepIndex + 1) % len(reminder.Steps)
		delta := time.Duration(reminder.Steps[candidateStep].AfterMinutes) * time.Minute
		if delta > remaining {
			break
		}
		stepAt = stepAt.Add(delta)
		remaining -= delta
		stepIndex = candidateStep
	}
	nextStep = (stepIndex + 1) % len(reminder.Steps)
	nextAt = stepAt.Add(time.Duration(reminder.Steps[nextStep].AfterMinutes) * time.Minute)
	return stepIndex, stepAt, nextStep, nextAt, false, nil
}
