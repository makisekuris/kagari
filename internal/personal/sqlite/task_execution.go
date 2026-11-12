package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/textclip"
)

// Stage results and execution-time accounting.
func (s *Store) Progress(ctx context.Context, runID int64, text string) error {
	if runID <= 0 {
		return errors.New("personal: run ID must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskID, revision int64
	var status personal.RunStatus
	var kind personal.RunKind
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,status,kind FROM personal_runs WHERE id=?`, runID).Scan(&taskID, &revision, &status, &kind); err != nil {
		return err
	}
	if kind != personal.RunTask || status != personal.RunRunning {
		return errors.New("personal: progress requires the current active task run")
	}
	var currentRevision int64
	var taskStatus personal.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&currentRevision, &taskStatus); err != nil {
		return err
	}
	if currentRevision != revision || taskStatus != personal.TaskRunning {
		return errors.New("personal: progress belongs to a stale task run")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET summary=?,updated_at=? WHERE id=? AND revision=? AND status='running'`, text, timestamp(time.Now()), taskID, revision); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishStage(ctx context.Context, runID int64, result personal.StageResult, failure string) error {
	if runID <= 0 {
		return errors.New("personal: run ID must be positive")
	}
	if utf8.RuneCountInString(result.Summary) > personal.MaxTaskChars || utf8.RuneCountInString(failure) > personal.MaxTaskChars {
		return errors.New("personal: stage result exceeds its size limit")
	}
	if failure != "" {
		result.AppliedInputIDs = nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskID, revision int64
	var runStatus personal.RunStatus
	var kind personal.RunKind
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,status,kind FROM personal_runs WHERE id=?`, runID).Scan(&taskID, &revision, &runStatus, &kind); err != nil {
		return err
	}
	if runStatus != personal.RunRunning || kind != personal.RunTask {
		if runStatus == personal.RunCancelled || runStatus == personal.RunCompleted || runStatus == personal.RunFailed {
			taskStatus := ""
			if taskID > 0 {
				task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
				if err != nil {
					return err
				}
				taskStatus = string(task.Status)
			}
			return commitStoreOutcome(ctx, tx, "task.stage.finish", "ignored_terminal", nil,
				zap.Int64("task_id", taskID), zap.Int64("run_id", runID), zap.Int64("revision", revision),
				zap.String("run_status", string(runStatus)), zap.String("task_status", taskStatus))
		}
		return fmt.Errorf("personal: cannot finish run %d in state %q", runID, runStatus)
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return err
	}
	if task.Revision != revision || task.Status != personal.TaskRunning {
		_, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE id=? AND status='running'`, runID)
		if err != nil {
			return err
		}
		if err = releaseTerminalTaskInputsTx(ctx, tx); err != nil {
			return err
		}
		return commitStoreOutcome(ctx, tx, "task.stage.finish", "discarded_stale", nil,
			zap.Int64("task_id", taskID), zap.Int64("run_id", runID), zap.Int64("revision", revision),
			zap.String("run_status", string(personal.RunCancelled)), zap.String("task_status", string(task.Status)))
	}
	result.Attachments, err = s.canonicalTaskAttachments(ctx, tx, result.Attachments)
	if err != nil {
		return err
	}
	status := personal.RunCompleted
	for i, ref := range result.Sources {
		material, err := scanMaterial(tx.QueryRowContext(ctx, `SELECT `+materialColumns+` FROM personal_materials WHERE id=? AND kind='source'`, ref.ID))
		if err != nil {
			return err
		}
		if err = authorizeRead(ctx, tx, personal.ReadScope{RunID: runID}, material.RunID, false); err != nil {
			return err
		}
		result.Sources[i] = *material.Source
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if failure != "" {
		status = personal.RunFailed
	}
	taskStatus := personal.TaskAwaitingReview
	if status == personal.RunFailed {
		taskStatus = personal.TaskFailed
	}
	if remainingMS(task.BudgetMS, task.UsedMS) == 0 {
		taskStatus = personal.TaskWaitingBudget
	}
	now := timestamp(time.Now())
	allowed := map[string]bool{}
	for _, a := range task.Attachments {
		allowed[a.ID] = true
	}
	for _, a := range result.Attachments {
		allowed[a.ID] = true
	}
	if err := applyProgressTx(ctx, tx, personal.Run{ID: runID, TaskID: taskID, Revision: revision}, 0, -1, result.Progress, allowed); err != nil {
		return err
	}
	if err := finishTaskInputsTx(ctx, tx, runID, revision, result.AppliedInputIDs, failure); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status=?,result=?,error=? WHERE id=? AND status='running'`,
		status, string(raw), failure, runID); err != nil {
		return err
	}
	summary := task.Summary
	if result.Summary != "" {
		summary = result.Summary
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status=?,summary=?,checkpoint=?,updated_at=? WHERE id=? AND revision=? AND status='running'`,
		taskStatus, summary, stageCheckpoint(runID, result), now, taskID, revision); err != nil {
		return err
	}
	callbackText := result.Summary
	if failure != "" {
		callbackText = failure
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,task_id,run_id,schedule_id,status,created_at)
		VALUES('task',?,?,?,(SELECT schedule_id FROM personal_schedule_occurrences WHERE task_id=?),'queued',?)`, callbackText, taskID, runID, taskID, now); err != nil {
		return err
	}
	return commitStoreOutcome(ctx, tx, "task.stage.finish", string(status), stageOutcomePayload{Result: result, Failure: failure},
		zap.Int64("task_id", taskID), zap.Int64("run_id", runID), zap.Int64("revision", revision),
		zap.String("run_status", string(status)), zap.String("task_status", string(taskStatus)))
}

// Run.Result retains full stage data and material references; the handoff is bounded.
func stageCheckpoint(runID int64, result personal.StageResult) string {
	result.Attachments = nil
	// Progress and input processing have their own durable handoff; do not duplicate them here.
	result.Progress = nil
	result.AppliedInputIDs = nil
	checkpoint := struct {
		RunID int64 `json:"run_id"`
		personal.StageResult
	}{RunID: runID, StageResult: result}
	checkpoint.Summary = textclip.Excerpt(result.Summary, 12000)
	checkpoint.Sources = make([]domain.SourceRef, 0, len(result.Sources))
	for _, source := range result.Sources {
		checkpoint.Sources = append(checkpoint.Sources, source)
	}
	for {
		raw, _ := json.Marshal(checkpoint)
		if utf8.RuneCountInString(string(raw)) <= personal.MaxTaskChars {
			return string(raw)
		}
		if len(checkpoint.Files) > 0 {
			checkpoint.Files = checkpoint.Files[:len(checkpoint.Files)-1]
		} else if len(checkpoint.Sources) > 0 {
			checkpoint.Sources = checkpoint.Sources[:len(checkpoint.Sources)-1]
		} else {
			checkpoint.Summary = textclip.Excerpt(checkpoint.Summary, utf8.RuneCountInString(checkpoint.Summary)/2)
		}
	}
}

func (s *Store) AccountRun(ctx context.Context, runID int64, elapsed time.Duration) (time.Duration, error) {
	if runID <= 0 || elapsed < 0 {
		return 0, errors.New("personal: run ID and nonnegative elapsed time are required")
	}
	elapsedMS := int64(elapsed / time.Millisecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var taskID, revision, savedElapsed int64
	var status personal.RunStatus
	var kind personal.RunKind
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,elapsed_ms,status,kind FROM personal_runs WHERE id=?`, runID).
		Scan(&taskID, &revision, &savedElapsed, &status, &kind); err != nil {
		return 0, err
	}
	if kind != personal.RunTask {
		return 0, errors.New("personal: only task runs have a time budget")
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return 0, err
	}
	if elapsedMS < savedElapsed {
		elapsedMS = savedElapsed
	}
	if status == personal.RunRunning && task.Revision == revision && task.Status == personal.TaskRunning {
		delta := elapsedMS - savedElapsed
		if delta > math.MaxInt64-task.UsedMS {
			return 0, errors.New("personal: task used time overflow")
		}
		task.UsedMS += delta
		if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET elapsed_ms=? WHERE id=? AND status='running'`, elapsedMS, runID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET used_ms=?,updated_at=? WHERE id=? AND revision=?`, task.UsedMS, timestamp(time.Now()), taskID, revision); err != nil {
			return 0, err
		}
	} else if elapsedMS > savedElapsed {
		// A stale or finished run cannot charge another revision.
		elapsedMS = savedElapsed
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return time.Duration(remainingMS(task.BudgetMS, task.UsedMS)) * time.Millisecond, nil
}

func (s *Store) AccountEvent(ctx context.Context, eventID int64, elapsed time.Duration) error {
	if eventID <= 0 || elapsed < 0 {
		return errors.New("personal: event ID and nonnegative elapsed time are required")
	}
	elapsedMS := int64(elapsed / time.Millisecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind personal.EventKind
	var status personal.EventStatus
	var taskID, runID, savedElapsed int64
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,task_id,run_id,elapsed_ms FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &status, &taskID, &runID, &savedElapsed); err != nil {
		return err
	}
	if kind != personal.EventTask {
		return errors.New("personal: only task callbacks consume task budget")
	}
	if status != personal.EventRunning {
		return nil
	}
	var revision, runRevision, latest, latestRevision, latestEventID int64
	var taskStatus personal.TaskStatus
	var runStatus personal.RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&revision, &taskStatus); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_runs WHERE id=? AND task_id=?`, runID, taskID).Scan(&runRevision, &runStatus); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id,revision,event_id FROM personal_runs WHERE task_id=? ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&latest, &latestRevision, &latestEventID); err != nil {
		return err
	}
	if runRevision != revision || latestRevision != revision || runStatus == personal.RunCancelled || taskStatus == personal.TaskCancelled || (runID != latest && latestEventID != eventID) {
		return tx.Commit()
	}
	if elapsedMS < savedElapsed {
		elapsedMS = savedElapsed
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return err
	}
	delta := elapsedMS - savedElapsed
	if delta > math.MaxInt64-task.UsedMS {
		return errors.New("personal: task used time overflow")
	}
	task.UsedMS += delta
	if _, err := tx.ExecContext(ctx, `UPDATE personal_events SET elapsed_ms=? WHERE id=? AND status='running'`, elapsedMS, eventID); err != nil {
		return err
	}
	newTaskStatus := taskStatus
	if taskStatus != personal.TaskCompleted && task.UsedMS >= task.BudgetMS {
		newTaskStatus = personal.TaskWaitingBudget
		if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND revision=? AND status='queued'`, taskID, revision); err != nil {
			return err
		}
		if err := releaseTerminalTaskInputsTx(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET used_ms=?,status=?,updated_at=? WHERE id=? AND revision=?`, task.UsedMS, newTaskStatus, timestamp(time.Now()), taskID, revision); err != nil {
		return err
	}
	return tx.Commit()
}
