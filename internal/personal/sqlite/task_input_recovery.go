package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"kagari/internal/personal"
)

// Task-input completion, ownership, and recovery.
// PendingTaskInputs returns user events still awaiting an explicit processed report.
func (s *Store) PendingTaskInputs(ctx context.Context, taskID int64) ([]personal.Event, error) {
	if taskID <= 0 {
		return nil, errors.New("personal: task ID must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.`+strings.ReplaceAll(eventColumns, ",", ",e.")+`
		FROM personal_task_inputs i JOIN personal_events e ON e.id=i.event_id WHERE i.task_id=? AND i.status IN ('pending','assigned') ORDER BY e.id`, taskID)
	if err != nil {
		return nil, err
	}
	events := make([]personal.Event, 0)
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		events = append(events, *event)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range events {
		if err := s.refreshAttachments(ctx, s.db, events[i].Attachments); err != nil {
			return nil, err
		}
	}
	return events, nil
}

// validateTaskInputsSettledTx prevents completing a task with pending or assigned inputs.
func validateTaskInputsSettledTx(ctx context.Context, tx *sql.Tx, taskID int64) error {
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_task_inputs WHERE task_id=? AND status IN ('pending','assigned')`, taskID).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT event_id,status,run_id FROM personal_task_inputs
			WHERE task_id=? AND status IN ('pending','assigned') ORDER BY event_id LIMIT ?`, taskID, personal.MaxPendingInputs)
		if err != nil {
			return err
		}
		ids := make([]string, 0, personal.MaxPendingInputs)
		for rows.Next() {
			var eventID int64
			var status string
			var runID sql.NullInt64
			if err := rows.Scan(&eventID, &status, &runID); err != nil {
				_ = rows.Close()
				return err
			}
			id := strconv.FormatInt(eventID, 10) + ":" + status
			if runID.Valid {
				id += ":run=" + strconv.FormatInt(runID.Int64, 10)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		return fmt.Errorf("personal: task has %d unprocessed input(s); event_id:status[:run]=%s", pending, strings.Join(ids, ","))
	}
	return nil
}

// ReadTaskInput returns an immutable user event only when the active run owns its assignment.
func (s *Store) ReadTaskInput(ctx context.Context, runID, eventID int64) (personal.Event, error) {
	if runID <= 0 || eventID <= 0 {
		return personal.Event{}, taskInputAccessErrorf("personal: run and input event IDs must be positive")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return personal.Event{}, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, runID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return personal.Event{}, taskInputAccessErrorf("personal: task run %d does not exist", runID)
		}
		return personal.Event{}, err
	}
	if run.Kind != personal.RunTask || run.TaskID <= 0 || run.Status != personal.RunRunning {
		return personal.Event{}, taskInputAccessErrorf("personal: task input reads require the active task run")
	}
	var taskRevision int64
	var taskStatus personal.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, run.TaskID).Scan(&taskRevision, &taskStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return personal.Event{}, taskInputAccessErrorf("personal: task %d does not exist", run.TaskID)
		}
		return personal.Event{}, err
	}
	if taskRevision != run.Revision || taskStatus != personal.TaskRunning {
		return personal.Event{}, taskInputAccessErrorf("personal: task input belongs to a stale run")
	}
	refs, err := taskInputRefsFromRun(run.Input)
	if err != nil {
		return personal.Event{}, err
	}
	if _, ok := refs[eventID]; !ok {
		return personal.Event{}, taskInputAccessErrorf("personal: event %d is not in run %d input", eventID, runID)
	}
	var status string
	var revision int64
	var assignedRun sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT status,revision,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, run.TaskID, eventID).
		Scan(&status, &revision, &assignedRun); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return personal.Event{}, taskInputAccessErrorf("personal: event %d is not registered for task %d", eventID, run.TaskID)
		}
		return personal.Event{}, err
	}
	if status != "assigned" || revision != run.Revision || !assignedRun.Valid || assignedRun.Int64 != runID {
		return personal.Event{}, taskInputAccessErrorf("personal: event %d is not assigned to run %d", eventID, runID)
	}
	event, err := readEventTx(ctx, s, tx, eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, personal.ErrAttachmentUnavailable) {
			return personal.Event{}, taskInputAccessErrorf("personal: event %d is unavailable", eventID)
		}
		return personal.Event{}, err
	}
	if event.Kind != personal.EventUser {
		return personal.Event{}, taskInputAccessErrorf("personal: task input event %d is not a user message", eventID)
	}
	if err := tx.Commit(); err != nil {
		return personal.Event{}, err
	}
	return event, nil
}

// rebaseTaskInputsRevisionTx releases unprocessed assignments when the task goal changes.
func rebaseTaskInputsRevisionTx(ctx context.Context, tx *sql.Tx, taskID, revision int64) error {
	if taskID <= 0 || revision <= 0 {
		return errors.New("personal: task ID and revision must be positive")
	}
	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM personal_tasks WHERE id=?`, taskID).Scan(&currentRevision); err != nil {
		return err
	}
	if currentRevision != revision {
		return errors.New("personal: task input revision does not match the task")
	}
	_, err := tx.ExecContext(ctx, `UPDATE personal_task_inputs SET revision=?,status='pending',run_id=NULL
		WHERE task_id=? AND status IN ('pending','assigned') AND revision<>?`, revision, taskID, revision)
	return err
}

// recoverTaskInputsTx retains valid queued/running assignments and releases stale ones.
func recoverTaskInputsTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE personal_task_inputs SET revision=(SELECT revision FROM personal_tasks WHERE id=task_id),status='pending',run_id=NULL
		WHERE status IN ('pending','assigned') AND revision<>(SELECT revision FROM personal_tasks WHERE id=task_id)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_task_inputs SET status='pending',run_id=NULL
		WHERE status='assigned' AND EXISTS(
			SELECT 1 FROM personal_runs r WHERE r.id=personal_task_inputs.run_id AND r.task_id=personal_task_inputs.task_id
			AND r.kind='task' AND r.revision=personal_task_inputs.revision AND r.status='running'
		)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE personal_task_inputs SET status='pending',run_id=NULL
		WHERE status='assigned' AND NOT EXISTS(
			SELECT 1 FROM personal_runs r JOIN personal_tasks t ON t.id=personal_task_inputs.task_id
			WHERE r.id=personal_task_inputs.run_id AND r.task_id=personal_task_inputs.task_id AND r.kind='task'
			AND r.revision=personal_task_inputs.revision AND r.status='queued'
			AND t.revision=personal_task_inputs.revision AND t.status='queued' AND t.used_ms<t.budget_ms
		)`)
	return err
}

// claimTaskInputsTx restores only the immutable input references frozen into this run.
func claimTaskInputsTx(ctx context.Context, tx *sql.Tx, runID int64) error {
	if runID <= 0 {
		return errors.New("personal: run ID must be positive")
	}
	var taskID, revision int64
	var kind personal.RunKind
	var status personal.RunStatus
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,kind,status,input FROM personal_runs WHERE id=?`, runID).
		Scan(&taskID, &revision, &kind, &status, &encoded); err != nil {
		return err
	}
	if kind != personal.RunTask || status != personal.RunRunning {
		return errors.New("personal: task input claim requires the active task run")
	}
	var taskRevision int64
	var taskStatus personal.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&taskRevision, &taskStatus); err != nil {
		return err
	}
	if taskRevision != revision || taskStatus != personal.TaskRunning {
		return errors.New("personal: cannot claim inputs for a stale task run")
	}
	refs, err := taskInputRefsFromRun(encoded)
	if err != nil {
		return err
	}
	for eventID := range refs {
		var inputStatus string
		var inputRevision int64
		var assignedRun sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT status,revision,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, taskID, eventID).
			Scan(&inputStatus, &inputRevision, &assignedRun); err != nil {
			return fmt.Errorf("personal: task input %d is not registered: %w", eventID, err)
		}
		if inputRevision != revision {
			return fmt.Errorf("personal: task input %d belongs to revision %d, not %d", eventID, inputRevision, revision)
		}
		if inputStatus == "assigned" && assignedRun.Valid && assignedRun.Int64 == runID {
			continue
		}
		if inputStatus != "pending" || assignedRun.Valid {
			return fmt.Errorf("personal: task input %d cannot be assigned to run %d", eventID, runID)
		}
		res, err := tx.ExecContext(ctx, `UPDATE personal_task_inputs SET status='assigned',run_id=?
			WHERE task_id=? AND event_id=? AND revision=? AND status='pending' AND run_id IS NULL`, runID, taskID, eventID, revision)
		if err != nil {
			return err
		}
		if affected, err := res.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return err
			}
			return fmt.Errorf("personal: task input %d changed during run claim", eventID)
		}
	}
	return nil
}

// finishTaskInputsTx commits reported inputs with the stage result or releases them for retry.
func finishTaskInputsTx(ctx context.Context, tx *sql.Tx, runID, revision int64, ids []int64, failure string) error {
	if runID <= 0 || revision <= 0 {
		return errors.New("personal: run ID and revision must be positive")
	}
	var taskID, runRevision int64
	var kind personal.RunKind
	var status personal.RunStatus
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,kind,status,input FROM personal_runs WHERE id=?`, runID).
		Scan(&taskID, &runRevision, &kind, &status, &encoded); err != nil {
		return err
	}
	if kind != personal.RunTask || runRevision != revision || status != personal.RunRunning {
		return errors.New("personal: task input result does not match the active run revision")
	}
	var taskRevision int64
	var taskStatus personal.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&taskRevision, &taskStatus); err != nil {
		return err
	}
	if taskRevision != revision || taskStatus != personal.TaskRunning {
		return errors.New("personal: task input result belongs to a stale revision")
	}
	refs, err := taskInputRefsFromRun(encoded)
	if err != nil {
		return err
	}
	if failure == "" {
		seen := make(map[int64]bool, len(ids))
		for _, id := range ids {
			if id <= 0 || seen[id] {
				return errors.New("personal: applied task input IDs must be positive and unique")
			}
			if _, ok := refs[id]; !ok {
				return fmt.Errorf("personal: event %d is not in run %d input", id, runID)
			}
			var inputStatus string
			var inputRevision int64
			var assignedRun sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT status,revision,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, taskID, id).
				Scan(&inputStatus, &inputRevision, &assignedRun); err != nil {
				return err
			}
			if inputStatus != "assigned" || inputRevision != revision || !assignedRun.Valid || assignedRun.Int64 != runID {
				return fmt.Errorf("personal: event %d is not assigned to run %d at revision %d", id, runID, revision)
			}
			seen[id] = true
		}
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE personal_task_inputs SET status='applied'
				WHERE task_id=? AND event_id=? AND revision=? AND run_id=? AND status='assigned'`, taskID, id, revision, runID)
			if err != nil {
				return err
			}
			if affected, err := res.RowsAffected(); err != nil || affected != 1 {
				if err != nil {
					return err
				}
				return fmt.Errorf("personal: task input %d changed before stage commit", id)
			}
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE personal_task_inputs SET status='pending',run_id=NULL
		WHERE task_id=? AND revision=? AND run_id=? AND status='assigned'`, taskID, revision, runID)
	return err
}

func readEventTx(ctx context.Context, s *Store, tx *sql.Tx, eventID int64) (personal.Event, error) {
	event, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM personal_events WHERE id=?`, eventID))
	if err != nil {
		return personal.Event{}, err
	}
	if err := s.refreshAttachments(ctx, tx, event.Attachments); err != nil {
		return personal.Event{}, err
	}
	return *event, nil
}
