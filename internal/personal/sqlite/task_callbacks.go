package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"kagari/internal/personal"
)

type taskCallbackIdentity struct {
	taskID       int64
	runID        int64
	revision     int64
	taskRevision int64
	taskStatus   personal.TaskStatus
	runStatus    personal.RunStatus
	savedElapsed int64
	eventStatus  personal.EventStatus
}

// taskCallbackStateTx evaluates a callback's persisted source identity and its
// relation to the task's latest run. It returns authorization and database
// failures as errors; only superseded identities are represented as stale.
func taskCallbackStateTx(ctx context.Context, tx *sql.Tx, eventID int64) (personal.TaskCallbackState, taskCallbackIdentity, error) {
	var identity taskCallbackIdentity
	var kind personal.EventKind
	var eventTaskID, eventRunID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,task_id,run_id,elapsed_ms FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &identity.eventStatus, &eventTaskID, &eventRunID, &identity.savedElapsed); err != nil {
		return "", identity, err
	}
	if kind != personal.EventTask {
		return "", identity, errors.New("personal: task callback state requires a task callback event")
	}
	if !eventTaskID.Valid || eventTaskID.Int64 <= 0 || !eventRunID.Valid || eventRunID.Int64 <= 0 {
		return "", identity, errors.New("personal: task callback has no persisted task/run identity")
	}
	identity.taskID, identity.runID = eventTaskID.Int64, eventRunID.Int64

	var runTaskID sql.NullInt64
	var runKind personal.RunKind
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,status,kind FROM personal_runs WHERE id=?`, identity.runID).
		Scan(&runTaskID, &identity.revision, &identity.runStatus, &runKind); err != nil {
		return "", identity, err
	}
	if runKind != personal.RunTask || !runTaskID.Valid || runTaskID.Int64 != identity.taskID {
		return "", identity, fmt.Errorf("personal: task callback run %d does not belong to task %d", identity.runID, identity.taskID)
	}

	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, identity.taskID).
		Scan(&identity.taskRevision, &identity.taskStatus); err != nil {
		return "", identity, err
	}

	var latestID, latestRevision, latestEventID int64
	if err := tx.QueryRowContext(ctx, `SELECT id,revision,event_id FROM personal_runs WHERE task_id=? ORDER BY id DESC LIMIT 1`, identity.taskID).
		Scan(&latestID, &latestRevision, &latestEventID); err != nil {
		return "", identity, fmt.Errorf("personal: task %d has no latest run: %w", identity.taskID, err)
	}

	if identity.taskStatus == personal.TaskCancelled || identity.runStatus == personal.RunCancelled || identity.revision != identity.taskRevision {
		return personal.TaskCallbackStale, identity, nil
	}
	if identity.runStatus != personal.RunCompleted && identity.runStatus != personal.RunFailed {
		return "", identity, fmt.Errorf("personal: task callback source run %d is not terminal: %q", identity.runID, identity.runStatus)
	}
	if latestID != identity.runID {
		if latestEventID == eventID && latestRevision == identity.revision {
			return personal.TaskCallbackContinued, identity, nil
		}
		return personal.TaskCallbackStale, identity, nil
	}
	if identity.taskStatus == personal.TaskQueued || identity.taskStatus == personal.TaskRunning {
		return "", identity, fmt.Errorf("personal: task callback source run %d is terminal while task %d is %q", identity.runID, identity.taskID, identity.taskStatus)
	}
	return personal.TaskCallbackCurrent, identity, nil
}

func (s *Store) TaskCallbackState(ctx context.Context, eventID int64) (personal.TaskCallbackState, error) {
	if eventID <= 0 {
		return "", errors.New("personal: event ID must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	state, _, err := taskCallbackStateTx(ctx, tx, eventID)
	if err != nil {
		return "", recordNotFound(err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return state, nil
}
