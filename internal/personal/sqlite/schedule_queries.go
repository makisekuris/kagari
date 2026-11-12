package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"kagari/internal/personal"
)

// Schedule and occurrence queries.
func (s *Store) Schedule(ctx context.Context, id int64) (*personal.Schedule, error) {
	if id <= 0 {
		return nil, nil
	}
	schedule, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return schedule, err
}

func (s *Store) Schedules(ctx context.Context, includeCancelled bool, beforeID int64, limit int) ([]personal.Schedule, error) {
	if beforeID < 0 || limit < 0 {
		return nil, errors.New("personal: schedule cursor and limit cannot be negative")
	}
	if limit == 0 {
		limit = personal.DefaultSchedulePageSize
	}
	if limit > personal.MaxSchedulePageSize {
		limit = personal.MaxSchedulePageSize
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
	schedules := make([]personal.Schedule, 0, limit)
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

func (s *Store) ScheduleOccurrences(ctx context.Context, id, beforeID int64, limit int) ([]personal.ScheduleOccurrence, error) {
	if id <= 0 || beforeID < 0 || limit < 0 {
		return nil, errors.New("personal: schedule ID must be positive and cursor and limit non-negative")
	}
	if limit == 0 {
		limit = personal.DefaultSchedulePageSize
	}
	if limit > personal.MaxSchedulePageSize {
		limit = personal.MaxSchedulePageSize
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query := `SELECT ` + occurrenceColumns + ` FROM personal_schedule_occurrences WHERE schedule_id=?`
	if beforeID > 0 {
		query += ` AND id<?`
	}
	query += ` ORDER BY id DESC LIMIT ?`
	var rows *sql.Rows
	if beforeID > 0 {
		rows, err = tx.QueryContext(ctx, query, id, beforeID, limit)
	} else {
		rows, err = tx.QueryContext(ctx, query, id, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	occurrences := make([]personal.ScheduleOccurrence, 0, limit)
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
		if err := s.occurrenceTaskState(ctx, tx, &occurrences[i]); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return occurrences, nil
}

func (s *Store) ScheduleForTask(ctx context.Context, taskID int64) (*personal.ScheduleContext, error) {
	if taskID <= 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	occurrence, err := scanScheduleOccurrence(tx.QueryRowContext(ctx, `SELECT `+occurrenceColumns+` FROM personal_schedule_occurrences WHERE task_id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, occurrence.ScheduleID))
	if err != nil {
		return nil, err
	}
	if err := s.occurrenceTaskState(ctx, tx, occurrence); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &personal.ScheduleContext{Schedule: *schedule, Occurrence: *occurrence}, nil
}

func (s *Store) occurrenceTaskState(ctx context.Context, tx *sql.Tx, o *personal.ScheduleOccurrence) error {
	if o.TaskID == 0 {
		return nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT status,summary FROM personal_tasks WHERE id=?`, o.TaskID).Scan(&o.TaskStatus, &o.TaskSummary); err != nil {
		return err
	}
	states, err := loadTaskDeliveryStatesTx(ctx, tx, o.TaskID)
	o.DeliveryStates = states
	return err
}
