package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"go.uber.org/zap"
	"kagari/internal/channel"
	"kagari/internal/domain"
)

const maxTaskChars = 32_000
const maxTitleChars = 200

const taskColumns = `id,title,goal,context,attachments,revision,status,summary,budget_ms,used_ms,updated_at`
const runColumns = `id,task_id,revision,event_id,kind,input,status,result,error,elapsed_ms`

func scanTask(row scanner) (*Task, error) {
	var task Task
	var updated, attachments string
	if err := row.Scan(&task.ID, &task.Title, &task.Goal, &task.Context, &attachments, &task.Revision, &task.Status,
		&task.Summary, &task.BudgetMS, &task.UsedMS, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(attachments), &task.Attachments); err != nil {
		return nil, fmt.Errorf("personal: decode task attachments: %w", err)
	}
	if task.Attachments == nil {
		task.Attachments = []channel.Attachment{}
	}
	var err error
	task.UpdatedAt, err = parseTimestamp(updated)
	return &task, err
}

func scanRun(row scanner) (*Run, error) {
	var run Run
	var taskID sql.NullInt64
	if err := row.Scan(&run.ID, &taskID, &run.Revision, &run.EventID, &run.Kind, &run.Input, &run.Status,
		&run.Result, &run.Error, &run.ElapsedMS); err != nil {
		return nil, err
	}
	run.TaskID = taskID.Int64
	return &run, nil
}

func validateTaskInput(title string, input TaskInput) error {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(input.Goal) == "" {
		return errors.New("personal: task title and goal are required")
	}
	if runeLen(title) > maxTitleChars || runeLen(input.Goal) > maxTaskChars || runeLen(input.Context) > maxTaskChars ||
		runeLen(input.Step) > maxTaskChars || runeLen(input.Previous) > maxTaskChars {
		return errors.New("personal: task input exceeds its size limit")
	}
	for _, attachment := range input.Attachments {
		if attachment.ID == "" || attachment.Size < 0 || attachment.Size > channel.MaxFileBytes {
			return errors.New("personal: task attachments must have a valid local ID and size")
		}
	}
	return nil
}

func runeLen(s string) int { return len([]rune(s)) }

func encodeTaskInput(input TaskInput) (string, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *Store) canonicalTaskAttachments(ctx context.Context, tx *sql.Tx, attachments []channel.Attachment) ([]channel.Attachment, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	result := make([]channel.Attachment, 0, len(attachments))
	for _, item := range attachments {
		canonical, err := s.validateStoredAttachment(ctx, tx, item.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, canonical)
	}
	return result, nil
}

func mergeAttachments(groups ...[]channel.Attachment) []channel.Attachment {
	seen := map[string]bool{}
	merged := []channel.Attachment{}
	for _, group := range groups {
		for _, attachment := range group {
			if attachment.ID == "" || seen[attachment.ID] {
				continue
			}
			seen[attachment.ID] = true
			merged = append(merged, attachment)
		}
	}
	return merged
}

func previousStageAttachments(ctx context.Context, tx *sql.Tx, taskID int64) ([]channel.Attachment, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT result FROM personal_runs WHERE task_id=? AND kind='task' ORDER BY id DESC LIMIT 1`, taskID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && raw == "") { return nil, nil }
	if err != nil { return nil, err }
	var result StageResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil { return nil, err }
	return result.Attachments, nil
}

func (s *Store) refreshRunAttachments(ctx context.Context, query attachmentQuery, run *Run) error {
	if run == nil || run.Kind != RunTask {
		return nil
	}
	var input TaskInput
	if err := json.Unmarshal([]byte(run.Input), &input); err != nil {
		return err
	}
	if err := s.refreshAttachments(ctx, query, input.Attachments); err != nil {
		return err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	run.Input = string(raw)
	return nil
}

func requestKeyValid(key string) bool { return strings.TrimSpace(key) != "" && runeLen(key) <= 256 }

func eventState(ctx context.Context, tx *sql.Tx, eventID int64) (kind EventKind, status EventStatus, err error) {
	err = tx.QueryRowContext(ctx, `SELECT kind,status FROM personal_events WHERE id=?`, eventID).Scan(&kind, &status)
	return
}

func requireUserEvent(ctx context.Context, tx *sql.Tx, eventID int64, allowCompleted bool) error {
	kind, status, err := eventState(ctx, tx, eventID)
	if err != nil {
		return err
	}
	if kind != EventUser || (status != EventRunning && !(allowCompleted && status == EventCompleted)) {
		return errors.New("personal: mutation requires an active persisted user event")
	}
	return nil
}

func requireCallback(ctx context.Context, tx *sql.Tx, eventID, taskID, revision int64) error {
	var kind EventKind
	var status EventStatus
	var eventTaskID, runTaskID int64
	var runRevision int64
	var runStatus RunStatus
	var runID int64
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,task_id,run_id FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &status, &eventTaskID, &runID); err != nil {
		return err
	}
	if kind != EventTask || status != EventRunning || eventTaskID != taskID || runID <= 0 {
		return errors.New("personal: task callback cannot mutate this task")
	}
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,status FROM personal_runs WHERE id=?`, runID).
		Scan(&runTaskID, &runRevision, &runStatus); err != nil {
		return err
	}
	var currentRevision, latestRun int64
	var taskStatus TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&currentRevision, &taskStatus); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM personal_runs WHERE task_id=?`, taskID).Scan(&latestRun); err != nil {
		return err
	}
	if runTaskID != taskID || runRevision != revision || currentRevision != revision || runID != latestRun ||
		(runStatus != RunCompleted && runStatus != RunFailed) || taskStatus == TaskCancelled || taskStatus == TaskWaitingBudget {
		return errors.New("personal: stale or unauthorized task callback")
	}
	return nil
}

type requestPayload struct {
	TaskID       int64         `json:"task_id,omitempty"`
	Revision     int64         `json:"revision,omitempty"`
	Title        string        `json:"title,omitempty"`
	Input        TaskInput     `json:"input,omitempty"`
	Step         string        `json:"step,omitempty"`
	ExtendBudget bool          `json:"extend_budget,omitempty"`
	Change       *MemoryChange `json:"change,omitempty"`
}

func encodeRequest(payload requestPayload) (string, error) {
	raw, err := json.Marshal(payload)
	return string(raw), err
}

func previousRequest(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string) (taskID, runID, memoryHistoryID int64, found bool, err error) {
	var storedOperation, storedPayload string
	var storedTaskID, storedRunID, storedMemoryHistoryID sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT operation,payload,task_id,run_id,memory_history_id FROM personal_requests WHERE event_id=? AND request_key=?`, eventID, key).
		Scan(&storedOperation, &storedPayload, &storedTaskID, &storedRunID, &storedMemoryHistoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, 0, false, err
	}
	if storedOperation != operation || storedPayload != payload {
		return 0, 0, 0, false, errors.New("personal: request key reused with different input")
	}
	return storedTaskID.Int64, storedRunID.Int64, storedMemoryHistoryID.Int64, true, nil
}

func insertRequest(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string, taskID, runID, memoryHistoryID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO personal_requests(event_id,request_key,operation,payload,task_id,run_id,memory_history_id) VALUES(?,?,?,?,?,?,?)`,
		eventID, key, operation, payload, nullableID(taskID), nullableID(runID), nullableID(memoryHistoryID))
	return err
}

func taskRun(ctx context.Context, tx *sql.Tx, task Task, eventID int64, input TaskInput, now time.Time) (Run, error) {
	encoded, err := encodeTaskInput(input)
	if err != nil {
		return Run{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_runs(task_id,revision,event_id,kind,input,status,created_at)
		VALUES(?,?,?,'task',?,'queued',?)`, task.ID, task.Revision, eventID, encoded, timestamp(now))
	if err != nil {
		return Run{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Run{}, err
	}
	return Run{ID: id, TaskID: task.ID, Revision: task.Revision, EventID: eventID, Kind: RunTask,
		Input: encoded, Status: RunQueued}, nil
}

func (s *Store) CreateTask(ctx context.Context, eventID int64, key, title string, input TaskInput) (Task, Run, error) {
	if !requestKeyValid(key) {
		return Task{}, Run{}, errors.New("personal: request key is required")
	}
	if err := validateTaskInput(title, input); err != nil {
		return Task{}, Run{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, Run{}, err
	}
	defer tx.Rollback()
	input.Attachments, err = s.canonicalTaskAttachments(ctx, tx, input.Attachments)
	if err != nil {
		return Task{}, Run{}, err
	}
	payload, err := encodeRequest(requestPayload{Title: title, Input: input})
	if err != nil {
		return Task{}, Run{}, err
	}
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return Task{}, Run{}, err
	}
	taskID, runID, _, found, err := previousRequest(ctx, tx, eventID, key, "create_task", payload)
	if err != nil {
		return Task{}, Run{}, err
	}
	if found {
		task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
		if err != nil {
			return Task{}, Run{}, err
		}
		run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, runID))
		if err != nil {
			return Task{}, Run{}, err
		}
		if err := commitStoreOutcome(ctx, tx, "task.create", "replayed", taskOutcomePayload{Task: *task, Run: run},
			zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
			zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
			return Task{}, Run{}, err
		}
		return *task, *run, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return Task{}, Run{}, err
	}
	now := time.Now().UTC()
	attachments, err := json.Marshal(input.Attachments)
	if err != nil {
		return Task{}, Run{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_tasks(title,goal,context,attachments,revision,status,budget_ms,updated_at)
		VALUES(?,?,?,?,1,'queued',?,?)`, title, input.Goal, input.Context, string(attachments), s.taskBudgetMS, timestamp(now))
	if err != nil {
		return Task{}, Run{}, err
	}
	taskID, err = res.LastInsertId()
	if err != nil {
		return Task{}, Run{}, err
	}
	task := Task{ID: taskID, Title: title, Goal: input.Goal, Context: input.Context, Attachments: input.Attachments, Revision: 1,
		Status: TaskQueued, BudgetMS: s.taskBudgetMS, UpdatedAt: now}
	run, err := taskRun(ctx, tx, task, eventID, input, now)
	if err != nil {
		return Task{}, Run{}, err
	}
	if err := insertRequest(ctx, tx, eventID, key, "create_task", payload, taskID, run.ID, 0); err != nil {
		return Task{}, Run{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "task.create", "created", taskOutcomePayload{Task: task, Run: &run},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return Task{}, Run{}, err
	}
	return task, run, nil
}

func (s *Store) ReviseTask(ctx context.Context, eventID int64, key string, taskID, expectedRevision int64, input TaskInput) (Task, Run, error) {
	if !requestKeyValid(key) || taskID <= 0 || expectedRevision <= 0 {
		return Task{}, Run{}, errors.New("personal: request key, task ID and positive revision are required")
	}
	if err := validateTaskInput("task", input); err != nil {
		return Task{}, Run{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, Run{}, err
	}
	defer tx.Rollback()
	input.Attachments, err = s.canonicalTaskAttachments(ctx, tx, input.Attachments)
	if err != nil {
		return Task{}, Run{}, err
	}
	payload, err := encodeRequest(requestPayload{TaskID: taskID, Revision: expectedRevision, Input: input})
	if err != nil {
		return Task{}, Run{}, err
	}
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return Task{}, Run{}, err
	}
	gotTaskID, runID, _, found, err := previousRequest(ctx, tx, eventID, key, "revise_task", payload)
	if err != nil {
		return Task{}, Run{}, err
	}
	if found {
		task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, gotTaskID))
		if err != nil {
			return Task{}, Run{}, err
		}
		run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, runID))
		if err != nil {
			return Task{}, Run{}, err
		}
		if err := commitStoreOutcome(ctx, tx, "task.revise", "replayed", taskOutcomePayload{Task: *task, Run: run},
			zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
			zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
			return Task{}, Run{}, err
		}
		return *task, *run, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return Task{}, Run{}, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return Task{}, Run{}, err
	}
	if task.Revision != expectedRevision {
		return Task{}, Run{}, fmt.Errorf("personal: task %d revision is %d, expected %d", taskID, task.Revision, expectedRevision)
	}
	if task.Revision == math.MaxInt64 {
		return Task{}, Run{}, errors.New("personal: task revision exhausted")
	}
	if input.Previous == "" {
		input.Previous = taskCheckpoint(ctx, tx, taskID, task.Summary)
	}
	if runeLen(input.Previous) > maxTaskChars {
		input.Previous = string([]rune(input.Previous)[:maxTaskChars])
	}
	newRevision := task.Revision + 1
	now := time.Now().UTC()
	newStatus := TaskQueued
	if remainingMS(task.BudgetMS, task.UsedMS) == 0 {
		newStatus = TaskWaitingBudget
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND status IN ('queued','running')`, taskID); err != nil {
		return Task{}, Run{}, err
	}
	attachments, err := json.Marshal(input.Attachments)
	if err != nil {
		return Task{}, Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET goal=?,context=?,attachments=?,revision=?,status=?,updated_at=? WHERE id=? AND revision=?`,
		input.Goal, input.Context, string(attachments), newRevision, newStatus, timestamp(now), taskID, expectedRevision); err != nil {
		return Task{}, Run{}, err
	}
	task.Goal, task.Context, task.Attachments, task.Revision, task.Status, task.UpdatedAt = input.Goal, input.Context, input.Attachments, newRevision, newStatus, now
	run, err := taskRun(ctx, tx, *task, eventID, input, now)
	if err != nil {
		return Task{}, Run{}, err
	}
	if err := insertRequest(ctx, tx, eventID, key, "revise_task", payload, taskID, run.ID, 0); err != nil {
		return Task{}, Run{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "task.revise", "revised", taskOutcomePayload{Task: *task, Run: &run},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return Task{}, Run{}, err
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

func (s *Store) CancelTask(ctx context.Context, eventID, taskID, expectedRevision int64) (Task, error) {
	if taskID <= 0 || expectedRevision <= 0 {
		return Task{}, errors.New("personal: task ID and positive revision are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return Task{}, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return Task{}, err
	}
	if task.Revision != expectedRevision {
		return Task{}, fmt.Errorf("personal: task %d revision is %d, expected %d", taskID, task.Revision, expectedRevision)
	}
	if task.Status == TaskCancelled {
		if err := commitStoreOutcome(ctx, tx, "task.cancel", "unchanged", taskOutcomePayload{Task: *task},
			zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID),
			zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
			return Task{}, err
		}
		return *task, nil
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND status IN ('queued','running')`, taskID); err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status='cancelled',updated_at=? WHERE id=? AND revision=?`, timestamp(now), taskID, expectedRevision); err != nil {
		return Task{}, err
	}
	task.Status, task.UpdatedAt = TaskCancelled, now
	if err := commitStoreOutcome(ctx, tx, "task.cancel", "cancelled", taskOutcomePayload{Task: *task},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return Task{}, err
	}
	return *task, nil
}

func (s *Store) ContinueTask(ctx context.Context, eventID int64, key string, taskID, expectedRevision int64, step string, extendBudget bool) (Task, Run, error) {
	if !requestKeyValid(key) || taskID <= 0 || expectedRevision <= 0 || strings.TrimSpace(step) == "" || runeLen(step) > maxTaskChars {
		return Task{}, Run{}, errors.New("personal: request key, task ID, revision and bounded step are required")
	}
	payload, err := encodeRequest(requestPayload{TaskID: taskID, Revision: expectedRevision, Step: step, ExtendBudget: extendBudget})
	if err != nil {
		return Task{}, Run{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, Run{}, err
	}
	defer tx.Rollback()
	kind, status, err := eventState(ctx, tx, eventID)
	if err != nil {
		return Task{}, Run{}, err
	}
	if kind != EventUser && kind != EventTask {
		return Task{}, Run{}, errors.New("personal: continue requires a user event or authorized task callback")
	}
	gotTaskID, runID, _, found, err := previousRequest(ctx, tx, eventID, key, "continue_task", payload)
	if err != nil {
		return Task{}, Run{}, err
	}
	if found {
		task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, gotTaskID))
		if err != nil {
			return Task{}, Run{}, err
		}
		run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, runID))
		if err != nil {
			return Task{}, Run{}, err
		}
		if err := commitStoreOutcome(ctx, tx, "task.continue", "replayed", taskOutcomePayload{Task: *task, Run: run},
			zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
			zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
			return Task{}, Run{}, err
		}
		return *task, *run, nil
	}
	if status != EventRunning {
		return Task{}, Run{}, errors.New("personal: continue requires an active source event")
	}
	if kind != EventUser {
		if extendBudget {
			return Task{}, Run{}, errors.New("personal: task callbacks cannot extend budget")
		}
		if err := requireCallback(ctx, tx, eventID, taskID, expectedRevision); err != nil {
			return Task{}, Run{}, err
		}
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return Task{}, Run{}, err
	}
	if task.Revision != expectedRevision {
		return Task{}, Run{}, fmt.Errorf("personal: task %d revision is %d, expected %d", taskID, task.Revision, expectedRevision)
	}
	if kind == EventUser && status == EventRunning {
		if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
			return Task{}, Run{}, err
		}
	}
	if task.Status == TaskQueued || task.Status == TaskRunning || task.Status == TaskCompleted {
		return Task{}, Run{}, fmt.Errorf("personal: task %d cannot continue from state %q", taskID, task.Status)
	}
	if kind == EventTask && (task.Status == TaskCancelled || task.Status == TaskWaitingBudget) {
		return Task{}, Run{}, errors.New("personal: task callback cannot revive a cancelled or budget-waiting task")
	}
	if extendBudget {
		if task.BudgetMS > math.MaxInt64-s.taskBudgetMS {
			return Task{}, Run{}, errors.New("personal: task budget overflow")
		}
		task.BudgetMS += s.taskBudgetMS
	}
	if task.UsedMS >= task.BudgetMS {
		return Task{}, Run{}, errors.New("personal: task budget exhausted; explicit extension required")
	}
	previous := taskCheckpoint(ctx, tx, taskID, task.Summary)
	stageAttachments, err := previousStageAttachments(ctx, tx, taskID)
	if err != nil { return Task{}, Run{}, err }
	input := TaskInput{Goal: task.Goal, Context: task.Context, Step: step, Previous: previous,
		Attachments: mergeAttachments(task.Attachments, stageAttachments)}
	if err := s.refreshAttachments(ctx, tx, input.Attachments); err != nil {
		return Task{}, Run{}, err
	}
	if err := validateTaskInput(task.Title, input); err != nil {
		return Task{}, Run{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND status='queued'`, taskID); err != nil {
		return Task{}, Run{}, err
	}
	attachments, err := json.Marshal(input.Attachments)
	if err != nil {
		return Task{}, Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET attachments=?,budget_ms=?,status='queued',updated_at=? WHERE id=? AND revision=?`, string(attachments), task.BudgetMS, timestamp(now), taskID, expectedRevision); err != nil {
		return Task{}, Run{}, err
	}
	task.Attachments, task.Status, task.UpdatedAt = input.Attachments, TaskQueued, now
	run, err := taskRun(ctx, tx, *task, eventID, input, now)
	if err != nil {
		return Task{}, Run{}, err
	}
	if err := insertRequest(ctx, tx, eventID, key, "continue_task", payload, taskID, run.ID, 0); err != nil {
		return Task{}, Run{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "task.continue", "continued", taskOutcomePayload{Task: *task, Run: &run},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID), zap.Int64("run_id", run.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return Task{}, Run{}, err
	}
	return *task, run, nil
}

func (s *Store) SettleTask(ctx context.Context, eventID, taskID, expectedRevision int64, status TaskStatus, summary string) (Task, error) {
	if taskID <= 0 || expectedRevision <= 0 || runeLen(summary) > maxTaskChars {
		return Task{}, errors.New("personal: task ID, revision and bounded summary are required")
	}
	if status != TaskCompleted && status != TaskFailed && status != TaskWaitingInput {
		return Task{}, errors.New("personal: invalid settled task state")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	kind, sourceStatus, err := eventState(ctx, tx, eventID)
	if err != nil {
		return Task{}, err
	}
	if sourceStatus != EventRunning {
		return Task{}, errors.New("personal: settle requires an active source event")
	}
	if kind == EventUser {
		if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
			return Task{}, err
		}
	} else if kind == EventTask {
		if err := requireCallback(ctx, tx, eventID, taskID, expectedRevision); err != nil {
			return Task{}, err
		}
	} else {
		return Task{}, errors.New("personal: settle requires a user event or authorized task callback")
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return Task{}, err
	}
	if task.Revision != expectedRevision {
		return Task{}, errors.New("personal: task revision changed")
	}
	if task.Status == TaskCancelled || task.Status == TaskWaitingBudget {
		return Task{}, fmt.Errorf("personal: task cannot be settled from state %q", task.Status)
	}
	if kind == EventTask && (task.Status == TaskQueued || task.Status == TaskRunning) {
		return Task{}, errors.New("personal: callback cannot settle an active task run")
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND status IN ('queued','running')`, taskID); err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status=?,summary=?,updated_at=? WHERE id=? AND revision=?`, status, summary, timestamp(now), taskID, expectedRevision); err != nil {
		return Task{}, err
	}
	task.Status, task.Summary, task.UpdatedAt = status, summary, now
	if err := commitStoreOutcome(ctx, tx, "task.settle", "settled", taskOutcomePayload{Task: *task},
		zap.Int64("event_id", eventID), zap.Int64("task_id", task.ID),
		zap.Int64("revision", task.Revision), zap.String("task_status", string(task.Status))); err != nil {
		return Task{}, err
	}
	return *task, nil
}

func (s *Store) Task(ctx context.Context, id int64) (*Task, error) {
	if id <= 0 {
		return nil, nil
	}
	task, err := scanTaskOrNil(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, id))
	if err != nil || task == nil {
		return task, err
	}
	if err := s.refreshAttachments(ctx, s.db, task.Attachments); err != nil {
		return nil, err
	}
	return task, nil
}

func scanTaskOrNil(row scanner) (*Task, error) {
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return task, err
}

func (s *Store) Tasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 {
		return []Task{}, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks ORDER BY updated_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]Task, 0, limit)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range tasks {
		if err := s.refreshAttachments(ctx, s.db, tasks[i].Attachments); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

func (s *Store) Run(ctx context.Context, id int64) (*Run, error) {
	if id <= 0 {
		return nil, nil
	}
	run, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || run == nil {
		return run, err
	}
	return run, s.refreshRunAttachments(ctx, s.db, run)
}

func (s *Store) Runs(ctx context.Context, taskID int64, limit int) ([]Run, error) {
	if taskID <= 0 || limit <= 0 {
		return []Run{}, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE task_id=? ORDER BY id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]Run, 0, limit)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range runs {
		if err := s.refreshRunAttachments(ctx, s.db, &runs[i]); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

func (s *Store) ClaimRun(ctx context.Context, kind RunKind) (*Run, error) {
	if kind != RunTask && kind != RunMemoryReview {
		return nil, fmt.Errorf("personal: unknown run kind %q", kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var query string
	if kind == RunTask {
		query = `SELECT r.id FROM personal_runs r JOIN personal_tasks t ON t.id=r.task_id JOIN personal_events e ON e.id=r.event_id
			WHERE r.kind='task' AND r.status='queued' AND e.status='completed' AND t.status='queued' AND t.revision=r.revision
			AND t.used_ms<t.budget_ms AND NOT EXISTS(SELECT 1 FROM personal_runs active WHERE active.task_id=t.id AND active.revision=t.revision AND active.status='running')
			ORDER BY r.id LIMIT 1`
	} else {
		query = `SELECT r.id FROM personal_runs r JOIN personal_events e ON e.id=r.event_id
			WHERE r.kind='memory_review' AND r.status='queued' AND e.status='completed'
			AND NOT EXISTS(SELECT 1 FROM personal_runs active WHERE active.kind='memory_review' AND active.status='running')
			ORDER BY r.id LIMIT 1`
	}
	var id int64
	if err := tx.QueryRowContext(ctx, query).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var taskID int64
	var revision int64
	var remaining int64
	if kind == RunTask {
		if err := tx.QueryRowContext(ctx, `SELECT r.task_id,r.revision,CASE WHEN t.used_ms>=t.budget_ms THEN 0 ELSE t.budget_ms-t.used_ms END FROM personal_runs r JOIN personal_tasks t ON t.id=r.task_id WHERE r.id=?`, id).Scan(&taskID, &revision, &remaining); err != nil {
			return nil, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='running' WHERE id=? AND status='queued'`, id)
	if err != nil {
		return nil, err
	}
	if affected, err := res.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("personal: run claim lost")
	}
	if kind == RunTask {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status='running',updated_at=? WHERE id=? AND revision=? AND status='queued'`, timestamp(time.Now()), taskID, revision); err != nil {
			return nil, err
		}
	}
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.refreshRunAttachments(ctx, tx, run); err != nil { return nil, err }
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	run.RemainingMS = remaining
	return run, nil
}

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
	var status RunStatus
	var kind RunKind
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,status,kind FROM personal_runs WHERE id=?`, runID).Scan(&taskID, &revision, &status, &kind); err != nil {
		return err
	}
	if kind != RunTask || status != RunRunning {
		return errors.New("personal: progress requires the current active task run")
	}
	var currentRevision int64
	var taskStatus TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&currentRevision, &taskStatus); err != nil {
		return err
	}
	if currentRevision != revision || taskStatus != TaskRunning {
		return errors.New("personal: progress belongs to a stale task run")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET summary=?,updated_at=? WHERE id=? AND revision=? AND status='running'`, text, timestamp(time.Now()), taskID, revision); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishStage(ctx context.Context, runID int64, result StageResult, failure string) error {
	if runID <= 0 {
		return errors.New("personal: run ID must be positive")
	}
	if runeLen(result.Summary) > maxTaskChars || runeLen(failure) > maxTaskChars {
		return errors.New("personal: stage result exceeds its size limit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result.Attachments, err = s.canonicalTaskAttachments(ctx, tx, result.Attachments)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var taskID, revision int64
	var runStatus RunStatus
	var kind RunKind
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,status,kind FROM personal_runs WHERE id=?`, runID).Scan(&taskID, &revision, &runStatus, &kind); err != nil {
		return err
	}
	if runStatus != RunRunning || kind != RunTask {
		if runStatus == RunCancelled || runStatus == RunCompleted || runStatus == RunFailed {
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
	if task.Revision != revision || task.Status != TaskRunning {
		_, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE id=? AND status='running'`, runID)
		if err != nil {
			return err
		}
		return commitStoreOutcome(ctx, tx, "task.stage.finish", "discarded_stale", nil,
			zap.Int64("task_id", taskID), zap.Int64("run_id", runID), zap.Int64("revision", revision),
			zap.String("run_status", string(RunCancelled)), zap.String("task_status", string(task.Status)))
	}
	status := RunCompleted
	if failure != "" {
		status = RunFailed
	}
	taskStatus := TaskAwaitingReview
	if status == RunFailed {
		taskStatus = TaskFailed
	}
	if remainingMS(task.BudgetMS, task.UsedMS) == 0 {
		taskStatus = TaskWaitingBudget
	}
	now := timestamp(time.Now())
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,task_id,run_id,status,created_at)
		VALUES('task',?,?,?,'queued',?)`, callbackText, taskID, runID, now); err != nil {
		return err
	}
	return commitStoreOutcome(ctx, tx, "task.stage.finish", string(status), stageOutcomePayload{Result: result, Failure: failure},
		zap.Int64("task_id", taskID), zap.Int64("run_id", runID), zap.Int64("revision", revision),
		zap.String("run_status", string(status)), zap.String("task_status", string(taskStatus)))
}

// Full evidence stays in Run.Result; subsequent stages receive a bounded, valid summary.
func stageCheckpoint(runID int64, result StageResult) string {
	result.Attachments = nil
	checkpoint := struct {
		RunID int64 `json:"run_id"`
		StageResult
	}{RunID: runID, StageResult: result}
	checkpoint.Summary = clip(result.Summary, 12000)
	checkpoint.Sources = make([]domain.Source, 0, len(result.Sources))
	for _, source := range result.Sources {
		checkpoint.Sources = append(checkpoint.Sources, domain.Source{ID: clip(source.ID, 128), URL: clip(source.URL, 512), Title: clip(source.Title, 200), Status: source.Status})
	}
	for {
		raw, _ := json.Marshal(checkpoint)
		if runeLen(string(raw)) <= maxTaskChars {
			return string(raw)
		}
		if len(checkpoint.Sources) > 0 {
			checkpoint.Sources = checkpoint.Sources[:len(checkpoint.Sources)-1]
		} else {
			checkpoint.Summary = clip(checkpoint.Summary, runeLen(checkpoint.Summary)/2)
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
	var status RunStatus
	var kind RunKind
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,elapsed_ms,status,kind FROM personal_runs WHERE id=?`, runID).
		Scan(&taskID, &revision, &savedElapsed, &status, &kind); err != nil {
		return 0, err
	}
	if kind != RunTask {
		return 0, errors.New("personal: only task runs have a time budget")
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return 0, err
	}
	if elapsedMS < savedElapsed {
		elapsedMS = savedElapsed
	}
	if status == RunRunning && task.Revision == revision && task.Status == TaskRunning {
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
	var kind EventKind
	var status EventStatus
	var taskID, runID, savedElapsed int64
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,task_id,run_id,elapsed_ms FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &status, &taskID, &runID, &savedElapsed); err != nil {
		return err
	}
	if kind != EventTask {
		return errors.New("personal: only task callbacks consume task budget")
	}
	if status != EventRunning {
		return nil
	}
	var revision, runRevision, latest, latestRevision, latestEventID int64
	var taskStatus TaskStatus
	var runStatus RunStatus
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
	if runRevision != revision || latestRevision != revision || runStatus == RunCancelled || taskStatus == TaskCancelled || (runID != latest && latestEventID != eventID) {
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
	if taskStatus != TaskCompleted && task.UsedMS >= task.BudgetMS {
		newTaskStatus = TaskWaitingBudget
		if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='cancelled' WHERE task_id=? AND revision=? AND status='queued'`, taskID, revision); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET used_ms=?,status=?,updated_at=? WHERE id=? AND revision=?`, task.UsedMS, newTaskStatus, timestamp(time.Now()), taskID, revision); err != nil {
		return err
	}
	return tx.Commit()
}
