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
	if err := refreshDeliveryValidityTx(ctx, tx, taskID); err != nil {
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
	key := fmt.Sprintf("internal:cancel_task:%d:%d", taskID, expectedRevision)
	request := personal.DeliveryControlRequest{Action: personal.DeliveryControlAction("cancel_task"), TaskID: taskID}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return personal.Task{}, err
	}
	var savedRequest, savedResult string
	err = tx.QueryRowContext(ctx, `SELECT request_json,result_json FROM personal_delivery_controls WHERE event_id=? AND request_key=?`, eventID, key).
		Scan(&savedRequest, &savedResult)
	if err == nil {
		if savedRequest != string(requestJSON) {
			return personal.Task{}, errors.New("personal: saved task cancellation identity has a different request")
		}
		var saved cancelTaskControlSnapshot
		if err := json.Unmarshal([]byte(savedResult), &saved); err != nil {
			return personal.Task{}, fmt.Errorf("personal: decode saved task cancellation result: %w", err)
		}
		if saved.Task.ID != taskID || saved.Task.Revision != expectedRevision || saved.Task.Status != personal.TaskCancelled {
			return personal.Task{}, errors.New("personal: saved task cancellation result does not match its identity")
		}
		if err := commitStoreOutcome(ctx, tx, "task.cancel", "replayed", taskOutcomePayload{Task: saved.Task},
			zap.Int64("event_id", eventID), zap.Int64("task_id", taskID), zap.Int64("revision", expectedRevision),
			zap.String("task_status", string(saved.Task.Status))); err != nil {
			return personal.Task{}, err
		}
		return saved.Task, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return personal.Task{}, err
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.Task{}, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return personal.Task{}, err
	}
	if task.Revision != expectedRevision {
		return personal.Task{}, fmt.Errorf("personal: task %d revision is %d, expected %d", taskID, task.Revision, expectedRevision)
	}
	now := time.Now().UTC()
	if task.Status != personal.TaskCancelled {
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
	}
	if err := refreshDeliveryValidityTx(ctx, tx, taskID); err != nil {
		return personal.Task{}, err
	}
	if err := insertCancelTaskControlTx(ctx, tx, eventID, key, requestJSON, *task, now); err != nil {
		return personal.Task{}, err
	}
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
	if err := refreshDeliveryValidityTx(ctx, tx, taskID); err != nil {
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

func (s *Store) SettleTask(ctx context.Context, eventID, taskID, expectedRevision int64, settlement personal.TaskSettlement) (personal.Task, error) {
	if taskID <= 0 || expectedRevision <= 0 || utf8.RuneCountInString(settlement.Summary) > personal.MaxTaskChars {
		return personal.Task{}, errors.New("personal: task ID, revision and bounded summary are required")
	}
	if settlement.Status != personal.TaskCompleted && settlement.Status != personal.TaskFailed && settlement.Status != personal.TaskWaitingInput {
		return personal.Task{}, errors.New("personal: invalid settled task state")
	}
	settlement.Delivery = normalizeDeliveryDraft(settlement.Delivery)
	if err := validateDeliveryDraft(settlement.Delivery); err != nil {
		return personal.Task{}, err
	}
	settlementPayload, err := json.Marshal(settlement)
	if err != nil {
		return personal.Task{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Task{}, err
	}
	defer tx.Rollback()
	var storedPayload, storedResult string
	err = tx.QueryRowContext(ctx, `SELECT settlement_payload_json,settlement_result_json FROM personal_delivery_intents
		WHERE event_id=? AND settlement_task_id=? AND settlement_revision=?`, eventID, taskID, expectedRevision).
		Scan(&storedPayload, &storedResult)
	if err == nil {
		if storedPayload != string(settlementPayload) {
			return personal.Task{}, errors.New("personal: task settlement identity already has a different payload")
		}
		var original personal.Task
		if err := json.Unmarshal([]byte(storedResult), &original); err != nil {
			return personal.Task{}, fmt.Errorf("personal: decode saved task settlement result: %w", err)
		}
		if original.ID != taskID || original.Revision != expectedRevision || original.Status != settlement.Status {
			return personal.Task{}, errors.New("personal: saved task settlement result does not match its identity")
		}
		if err := commitStoreOutcome(ctx, tx, "task.settle", "replayed", taskOutcomePayload{Task: original},
			zap.Int64("event_id", eventID), zap.Int64("task_id", taskID), zap.Int64("revision", expectedRevision),
			zap.String("task_status", string(original.Status))); err != nil {
			return personal.Task{}, err
		}
		return original, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return personal.Task{}, err
	}

	kind, sourceStatus, err := eventState(ctx, tx, eventID)
	if err != nil {
		return personal.Task{}, err
	}
	if sourceStatus != personal.EventRunning {
		return personal.Task{}, errors.New("personal: settle requires an active source event")
	}
	var sourceRunID int64
	if kind == personal.EventUser {
		if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
			return personal.Task{}, err
		}
	} else if kind == personal.EventTask {
		if err := requireCallback(ctx, tx, eventID, taskID, expectedRevision); err != nil {
			return personal.Task{}, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT run_id FROM personal_events WHERE id=?`, eventID).Scan(&sourceRunID); err != nil {
			return personal.Task{}, err
		}
		if sourceRunID <= 0 {
			return personal.Task{}, errors.New("personal: task callback has no source run")
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
	for _, id := range settlement.Delivery.Attachments {
		if _, err := s.validateStoredAttachment(ctx, tx, id); err != nil {
			return personal.Task{}, err
		}
	}
	if settlement.Status == personal.TaskCompleted {
		if err := validateTaskInputsSettledTx(ctx, tx, taskID); err != nil {
			return personal.Task{}, err
		}
	}
	if err := settleScheduledTask(ctx, tx, *task, settlement.Status, settlement.Assessment, now); err != nil {
		return personal.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND status IN ('queued','running')`, taskID); err != nil {
		return personal.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status=?,summary=?,updated_at=? WHERE id=? AND revision=?`, settlement.Status, settlement.Summary, timestamp(now), taskID, expectedRevision); err != nil {
		return personal.Task{}, err
	}
	if err := releaseTerminalTaskInputsTx(ctx, tx); err != nil {
		return personal.Task{}, err
	}
	task.Status, task.Summary, task.UpdatedAt = settlement.Status, settlement.Summary, now
	resultSnapshot, err := json.Marshal(task)
	if err != nil {
		return personal.Task{}, err
	}
	var scheduleID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT schedule_id FROM personal_schedule_occurrences WHERE task_id=?`, taskID).Scan(&scheduleID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return personal.Task{}, err
	}
	intent, err := insertDeliveryIntentTx(ctx, tx, deliveryIntentInsert{
		eventID: eventID, scheduleID: scheduleID.Int64,
		purpose: personal.DeliveryResult, draft: settlement.Delivery,
		scopes:           []personal.DeliveryScope{{TaskID: taskID, Revision: expectedRevision, RunID: sourceRunID}},
		settlementTaskID: taskID, settlementRevision: expectedRevision,
		settlementPayload: string(settlementPayload), settlementResult: string(resultSnapshot),
	}, now)
	if err != nil {
		return personal.Task{}, err
	}
	if err := refreshDeliveryIntentValidityTx(ctx, tx, intent.ID); err != nil {
		return personal.Task{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "task.settle", "settled", taskOutcomePayload{Task: *task},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return personal.Task{}, err
	}
	return *task, nil
}

// Task and run queries.
