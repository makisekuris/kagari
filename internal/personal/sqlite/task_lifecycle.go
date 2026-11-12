package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

// Task creation and revision transitions.
func (s *Store) CreateTask(ctx context.Context, eventID int64, key, title string, input personal.TaskInput) (personal.Task, personal.Run, error) {
	if len(input.InputEvents) > 0 {
		return personal.Task{}, personal.Run{}, errors.New("personal: task input event references are runtime-owned")
	}
	if !requestKeyValid(key) {
		return personal.Task{}, personal.Run{}, errors.New("personal: request key is required")
	}
	if input.Trigger != nil {
		return personal.Task{}, personal.Run{}, errors.New("personal: task trigger is set by the scheduler")
	}
	if err := validateTaskInput(title, input); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	defer tx.Rollback()
	input.Attachments, err = s.canonicalTaskAttachments(ctx, tx, input.Attachments)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	payload, err := encodeRequest(requestPayload{Title: title, Input: input})
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	taskID, runID, _, found, err := previousRequest(ctx, tx, eventID, key, "create_task", payload)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if found {
		task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
		if err != nil {
			return personal.Task{}, personal.Run{}, err
		}
		run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, runID))
		if err != nil {
			return personal.Task{}, personal.Run{}, err
		}
		if err := commitStoreOutcome(ctx, tx, "task.create", "replayed", taskOutcomePayload{Task: *task, Run: run},
			zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
			zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
			return personal.Task{}, personal.Run{}, err
		}
		return *task, *run, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	now := time.Now().UTC()
	task, run, err := s.createTaskRunTx(ctx, tx, eventID, title, input, s.taskBudgetMS, now)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	taskID = task.ID
	if err := insertRequest(ctx, tx, eventID, key, "create_task", payload, taskID, run.ID, 0); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "task.create", "created", taskOutcomePayload{Task: task, Run: &run},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	return task, run, nil
}

func (s *Store) ReviseTask(ctx context.Context, eventID int64, key string, taskID, expectedRevision int64, input personal.TaskInput) (personal.Task, personal.Run, error) {
	if len(input.InputEvents) > 0 {
		return personal.Task{}, personal.Run{}, errors.New("personal: task input event references are runtime-owned")
	}
	if input.Trigger != nil {
		return personal.Task{}, personal.Run{}, errors.New("personal: task trigger is set by the scheduler")
	}
	if !requestKeyValid(key) || taskID <= 0 || expectedRevision <= 0 {
		return personal.Task{}, personal.Run{}, errors.New("personal: request key, task ID and positive revision are required")
	}
	if err := validateTaskInput("task", input); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	defer tx.Rollback()
	input.Attachments, err = s.canonicalTaskAttachments(ctx, tx, input.Attachments)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	payload, err := encodeRequest(requestPayload{TaskID: taskID, Revision: expectedRevision, Input: input})
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	gotTaskID, runID, _, found, err := previousRequest(ctx, tx, eventID, key, "revise_task", payload)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if found {
		task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, gotTaskID))
		if err != nil {
			return personal.Task{}, personal.Run{}, err
		}
		run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, runID))
		if err != nil {
			return personal.Task{}, personal.Run{}, err
		}
		if err := commitStoreOutcome(ctx, tx, "task.revise", "replayed", taskOutcomePayload{Task: *task, Run: run},
			zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
			zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
			return personal.Task{}, personal.Run{}, err
		}
		return *task, *run, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if task.Revision != expectedRevision {
		return personal.Task{}, personal.Run{}, fmt.Errorf("personal: task %d revision is %d, expected %d", taskID, task.Revision, expectedRevision)
	}
	if task.Revision == math.MaxInt64 {
		return personal.Task{}, personal.Run{}, errors.New("personal: task revision exhausted")
	}
	if input.Previous == "" {
		input.Previous = taskCheckpoint(ctx, tx, taskID, task.Summary)
	}
	if utf8.RuneCountInString(input.Previous) > personal.MaxTaskChars {
		input.Previous = string([]rune(input.Previous)[:personal.MaxTaskChars])
	}
	newRevision := task.Revision + 1
	now := time.Now().UTC()
	newStatus := personal.TaskQueued
	if remainingMS(task.BudgetMS, task.UsedMS) == 0 {
		newStatus = personal.TaskWaitingBudget
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND status IN ('queued','running')`, taskID); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	attachments, err := json.Marshal(input.Attachments)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET goal=?,context=?,attachments=?,revision=?,status=?,updated_at=? WHERE id=? AND revision=?`,
		input.Goal, input.Context, string(attachments), newRevision, newStatus, timestamp(now), taskID, expectedRevision); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if task.Goal != input.Goal || task.Context != input.Context || !slices.EqualFunc(task.Attachments, input.Attachments, func(a, b channel.Attachment) bool { return a.ID == b.ID }) {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_schedule_occurrences SET coverage_eligible=0 WHERE task_id=?`, taskID); err != nil {
			return personal.Task{}, personal.Run{}, err
		}
	}
	task.Goal, task.Context, task.Attachments, task.Revision, task.Status, task.UpdatedAt = input.Goal, input.Context, input.Attachments, newRevision, newStatus, now
	if err := rebaseTaskInputsRevisionTx(ctx, tx, taskID, newRevision); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	run, err := s.taskRun(ctx, tx, task, eventID, input, now)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if err := insertRequest(ctx, tx, eventID, key, "revise_task", payload, taskID, run.ID, 0); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "task.revise", "revised", taskOutcomePayload{Task: *task, Run: &run},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	return *task, run, nil
}

func taskCheckpoint(ctx context.Context, tx *sql.Tx, taskID int64, fallback string) string {
	var checkpoint string
	if err := tx.QueryRowContext(ctx, `SELECT checkpoint FROM personal_tasks WHERE id=?`, taskID).Scan(&checkpoint); err == nil && checkpoint != "" {
		return checkpoint
	}
	return fallback
}

func (s *Store) CancelTask(ctx context.Context, eventID, taskID, expectedRevision int64) (personal.Task, error) {
	if taskID <= 0 || expectedRevision <= 0 {
		return personal.Task{}, errors.New("personal: task ID and positive revision are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Task{}, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Task{}, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return personal.Task{}, err
	}
	if task.Revision != expectedRevision {
		return personal.Task{}, fmt.Errorf("personal: task %d revision is %d, expected %d", taskID, task.Revision, expectedRevision)
	}
	if task.Status == personal.TaskCancelled {
		if err := commitStoreOutcome(ctx, tx, "task.cancel", "unchanged", taskOutcomePayload{Task: *task},
			zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID),
			zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
			return personal.Task{}, err
		}
		return *task, nil
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND status IN ('queued','running')`, taskID); err != nil {
		return personal.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status='cancelled',updated_at=? WHERE id=? AND revision=?`, timestamp(now), taskID, expectedRevision); err != nil {
		return personal.Task{}, err
	}
	if err := releaseTerminalTaskInputsTx(ctx, tx); err != nil {
		return personal.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_reply_groups SET closed=1
		WHERE id IN (SELECT group_id FROM personal_reply_tasks WHERE task_id=?) AND NOT EXISTS(
			SELECT 1 FROM personal_reply_tasks rt JOIN personal_tasks t ON t.id=rt.task_id
			WHERE rt.group_id=personal_reply_groups.id AND t.status IN ('queued','running','awaiting_review'))`, taskID); err != nil {
		return personal.Task{}, err
	}
	task.Status, task.UpdatedAt = personal.TaskCancelled, now
	if err := commitStoreOutcome(ctx, tx, "task.cancel", "cancelled", taskOutcomePayload{Task: *task},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return personal.Task{}, err
	}
	return *task, nil
}

func (s *Store) ContinueTask(ctx context.Context, eventID int64, key string, taskID, expectedRevision int64, step string, extendBudget bool) (personal.Task, personal.Run, error) {
	if !requestKeyValid(key) || taskID <= 0 || expectedRevision <= 0 || strings.TrimSpace(step) == "" || utf8.RuneCountInString(step) > personal.MaxTaskChars {
		return personal.Task{}, personal.Run{}, errors.New("personal: request key, task ID, revision and bounded step are required")
	}
	payload, err := encodeRequest(requestPayload{TaskID: taskID, Revision: expectedRevision, Step: step, ExtendBudget: extendBudget})
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	defer tx.Rollback()
	kind, status, err := eventState(ctx, tx, eventID)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if kind != personal.EventUser && kind != personal.EventTask {
		return personal.Task{}, personal.Run{}, errors.New("personal: continue requires a user event or authorized task callback")
	}
	gotTaskID, runID, _, found, err := previousRequest(ctx, tx, eventID, key, "continue_task", payload)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if found {
		task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, gotTaskID))
		if err != nil {
			return personal.Task{}, personal.Run{}, err
		}
		run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, runID))
		if err != nil {
			return personal.Task{}, personal.Run{}, err
		}
		if err := commitStoreOutcome(ctx, tx, "task.continue", "replayed", taskOutcomePayload{Task: *task, Run: run},
			zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
			zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
			return personal.Task{}, personal.Run{}, err
		}
		return *task, *run, nil
	}
	if status != personal.EventRunning {
		return personal.Task{}, personal.Run{}, errors.New("personal: continue requires an active source event")
	}
	if kind != personal.EventUser {
		if extendBudget {
			return personal.Task{}, personal.Run{}, errors.New("personal: task callbacks cannot extend budget")
		}
		if err := requireCallback(ctx, tx, eventID, taskID, expectedRevision); err != nil {
			return personal.Task{}, personal.Run{}, err
		}
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if task.Revision != expectedRevision {
		return personal.Task{}, personal.Run{}, fmt.Errorf("personal: task %d revision is %d, expected %d", taskID, task.Revision, expectedRevision)
	}
	if kind == personal.EventUser && status == personal.EventRunning {
		if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
			return personal.Task{}, personal.Run{}, err
		}
	}
	if task.Status == personal.TaskQueued || task.Status == personal.TaskRunning || task.Status == personal.TaskCompleted {
		return personal.Task{}, personal.Run{}, fmt.Errorf("personal: task %d cannot continue from state %q", taskID, task.Status)
	}
	if kind == personal.EventTask && (task.Status == personal.TaskCancelled || task.Status == personal.TaskWaitingBudget) {
		return personal.Task{}, personal.Run{}, errors.New("personal: task callback cannot revive a cancelled or budget-waiting task")
	}
	if extendBudget {
		if task.BudgetMS > math.MaxInt64/int64(time.Millisecond)-s.taskBudgetMS {
			return personal.Task{}, personal.Run{}, errors.New("personal: task budget overflow")
		}
		task.BudgetMS += s.taskBudgetMS
	}
	if task.UsedMS >= task.BudgetMS {
		return personal.Task{}, personal.Run{}, errors.New("personal: task budget exhausted; explicit extension required")
	}
	previous := taskCheckpoint(ctx, tx, taskID, task.Summary)
	stageAttachments, err := previousStageAttachments(ctx, tx, taskID)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	input := personal.TaskInput{Goal: task.Goal, Context: task.Context, Step: step, Previous: previous,
		Attachments: mergeAttachments(task.Attachments, stageAttachments)}
	if err := s.refreshAttachments(ctx, tx, input.Attachments); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if err := validateTaskInput(task.Title, input); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND status='queued'`, taskID); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	attachments, err := json.Marshal(input.Attachments)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET attachments=?,budget_ms=?,status='queued',updated_at=? WHERE id=? AND revision=?`, string(attachments), task.BudgetMS, timestamp(now), taskID, expectedRevision); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	task.Attachments, task.Status, task.UpdatedAt = input.Attachments, personal.TaskQueued, now
	run, err := s.taskRun(ctx, tx, task, eventID, input, now)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if err := insertRequest(ctx, tx, eventID, key, "continue_task", payload, taskID, run.ID, 0); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "task.continue", "continued", taskOutcomePayload{Task: *task, Run: &run},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	return *task, run, nil
}

func (s *Store) SettleTask(ctx context.Context, eventID, taskID, expectedRevision int64, status personal.TaskStatus, summary string, assessment ...*personal.CheckAssessment) (personal.Task, error) {
	if taskID <= 0 || expectedRevision <= 0 || utf8.RuneCountInString(summary) > personal.MaxTaskChars {
		return personal.Task{}, errors.New("personal: task ID, revision and bounded summary are required")
	}
	if status != personal.TaskCompleted && status != personal.TaskFailed && status != personal.TaskWaitingInput {
		return personal.Task{}, errors.New("personal: invalid settled task state")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Task{}, err
	}
	defer tx.Rollback()
	kind, sourceStatus, err := eventState(ctx, tx, eventID)
	if err != nil {
		return personal.Task{}, err
	}
	if sourceStatus != personal.EventRunning {
		return personal.Task{}, errors.New("personal: settle requires an active source event")
	}
	if kind == personal.EventUser {
		if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
			return personal.Task{}, err
		}
	} else if kind == personal.EventTask {
		if err := requireCallback(ctx, tx, eventID, taskID, expectedRevision); err != nil {
			return personal.Task{}, err
		}
	} else {
		return personal.Task{}, errors.New("personal: settle requires a user event or authorized task callback")
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return personal.Task{}, err
	}
	if task.Revision != expectedRevision {
		return personal.Task{}, errors.New("personal: task revision changed")
	}
	if task.Status == personal.TaskCancelled || task.Status == personal.TaskWaitingBudget {
		return personal.Task{}, fmt.Errorf("personal: task cannot be settled from state %q", task.Status)
	}
	if kind == personal.EventTask && (task.Status == personal.TaskQueued || task.Status == personal.TaskRunning) {
		return personal.Task{}, errors.New("personal: callback cannot settle an active task run")
	}
	now := time.Now().UTC()
	if len(assessment) > 1 {
		return personal.Task{}, errors.New("personal: at most one check assessment is allowed")
	}
	var check *personal.CheckAssessment
	if len(assessment) == 1 {
		check = assessment[0]
	}
	if status == personal.TaskCompleted {
		if err := validateTaskInputsSettledTx(ctx, tx, taskID); err != nil {
			return personal.Task{}, err
		}
	}
	if err := settleScheduledTask(ctx, tx, *task, status, check, now); err != nil {
		return personal.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND status IN ('queued','running')`, taskID); err != nil {
		return personal.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status=?,summary=?,updated_at=? WHERE id=? AND revision=?`, status, summary, timestamp(now), taskID, expectedRevision); err != nil {
		return personal.Task{}, err
	}
	if err := releaseTerminalTaskInputsTx(ctx, tx); err != nil {
		return personal.Task{}, err
	}
	task.Status, task.Summary, task.UpdatedAt = status, summary, now
	if err := commitStoreOutcome(ctx, tx, "task.settle", "settled", taskOutcomePayload{Task: *task},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return personal.Task{}, err
	}
	return *task, nil
}

// Task and run queries.
