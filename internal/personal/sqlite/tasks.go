package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

const maxTitleChars = 200

const taskColumns = `id,title,goal,context,attachments,revision,status,summary,budget_ms,used_ms,updated_at,source_run_ids,memory_refs`
const runColumns = `id,task_id,revision,event_id,kind,input,status,result,error,elapsed_ms,context_manifest`

// Task decoding, validation, and idempotent request helpers.
func scanTask(row scanner) (*personal.Task, error) {
	var task personal.Task
	var updated, attachments, sourceRuns, memoryRefs string
	if err := row.Scan(&task.ID, &task.Title, &task.Goal, &task.Context, &attachments, &task.Revision, &task.Status,
		&task.Summary, &task.BudgetMS, &task.UsedMS, &updated, &sourceRuns, &memoryRefs); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(attachments), &task.Attachments); err != nil {
		return nil, fmt.Errorf("personal: decode task attachments: %w", err)
	}
	if err := json.Unmarshal([]byte(sourceRuns), &task.SourceRunIDs); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(memoryRefs), &task.MemoryRefs); err != nil {
		return nil, err
	}
	if task.Attachments == nil {
		task.Attachments = []channel.Attachment{}
	}
	var err error
	task.UpdatedAt, err = parseTimestamp(updated)
	return &task, err
}

func scanRun(row scanner) (*personal.Run, error) {
	var run personal.Run
	var taskID sql.NullInt64
	if err := row.Scan(&run.ID, &taskID, &run.Revision, &run.EventID, &run.Kind, &run.Input, &run.Status,
		&run.Result, &run.Error, &run.ElapsedMS, &run.ContextManifest); err != nil {
		return nil, err
	}
	run.TaskID = taskID.Int64
	return &run, nil
}

func validateTaskInput(title string, input personal.TaskInput) error {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(input.Goal) == "" {
		return errors.New("personal: task title and goal are required")
	}
	if utf8.RuneCountInString(title) > maxTitleChars || utf8.RuneCountInString(input.Goal) > personal.MaxTaskChars || utf8.RuneCountInString(input.Context) > personal.MaxTaskChars ||
		utf8.RuneCountInString(input.Step) > personal.MaxTaskChars || utf8.RuneCountInString(input.Previous) > personal.MaxTaskChars {
		return errors.New("personal: task input exceeds its size limit")
	}
	for _, attachment := range input.Attachments {
		if attachment.ID == "" || attachment.Size < 0 || attachment.Size > channel.MaxFileBytes {
			return errors.New("personal: task attachments must have a valid local ID and size")
		}
	}
	return nil
}

func encodeTaskInput(input personal.TaskInput) (string, error) {
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
	if errors.Is(err, sql.ErrNoRows) || (err == nil && raw == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result personal.StageResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, err
	}
	return result.Attachments, nil
}

func (s *Store) refreshRunAttachments(ctx context.Context, query attachmentQuery, run *personal.Run) error {
	if run == nil || run.Kind != personal.RunTask {
		return nil
	}
	var input personal.TaskInput
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

func requestKeyValid(key string) bool {
	return strings.TrimSpace(key) != "" && utf8.RuneCountInString(key) <= 256
}

func eventState(ctx context.Context, tx *sql.Tx, eventID int64) (kind personal.EventKind, status personal.EventStatus, err error) {
	err = tx.QueryRowContext(ctx, `SELECT kind,status FROM personal_events WHERE id=?`, eventID).Scan(&kind, &status)
	return
}

func requireUserEvent(ctx context.Context, tx *sql.Tx, eventID int64, allowCompleted bool) error {
	kind, status, err := eventState(ctx, tx, eventID)
	if err != nil {
		return err
	}
	if kind != personal.EventUser || (status != personal.EventRunning && !(allowCompleted && status == personal.EventCompleted)) {
		return errors.New("personal: mutation requires an active persisted user event")
	}
	return nil
}

func requireCallback(ctx context.Context, tx *sql.Tx, eventID, taskID, revision int64) error {
	var kind personal.EventKind
	var status personal.EventStatus
	var eventTaskID, runTaskID int64
	var runRevision int64
	var runStatus personal.RunStatus
	var runID int64
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,task_id,run_id FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &status, &eventTaskID, &runID); err != nil {
		return err
	}
	if kind != personal.EventTask || status != personal.EventRunning || eventTaskID != taskID || runID <= 0 {
		return errors.New("personal: task callback cannot mutate this task")
	}
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,status FROM personal_runs WHERE id=?`, runID).
		Scan(&runTaskID, &runRevision, &runStatus); err != nil {
		return err
	}
	var currentRevision, latestRun int64
	var taskStatus personal.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&currentRevision, &taskStatus); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM personal_runs WHERE task_id=?`, taskID).Scan(&latestRun); err != nil {
		return err
	}
	if runTaskID != taskID || runRevision != revision || currentRevision != revision || runID != latestRun ||
		(runStatus != personal.RunCompleted && runStatus != personal.RunFailed) || taskStatus == personal.TaskCancelled || taskStatus == personal.TaskWaitingBudget {
		return errors.New("personal: stale or unauthorized task callback")
	}
	return nil
}

type requestPayload struct {
	TaskID       int64                  `json:"task_id,omitempty"`
	Revision     int64                  `json:"revision,omitempty"`
	Title        string                 `json:"title,omitempty"`
	Input        personal.TaskInput     `json:"input,omitempty"`
	Step         string                 `json:"step,omitempty"`
	ExtendBudget bool                   `json:"extend_budget,omitempty"`
	Change       *personal.MemoryChange `json:"change,omitempty"`
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

// Task creation and revision transitions.
func (s *Store) taskRun(ctx context.Context, tx *sql.Tx, task *personal.Task, eventID int64, input personal.TaskInput, now time.Time) (personal.Run, error) {
	if err := bindTaskReferences(ctx, tx, task, eventID, &input); err != nil {
		return personal.Run{}, err
	}
	trigger, err := scheduledTaskTrigger(ctx, tx, task.ID)
	if err != nil {
		return personal.Run{}, err
	}
	input.Trigger = trigger
	if err := s.withTaskInputsTx(ctx, tx, task.ID, &input); err != nil {
		return personal.Run{}, err
	}
	encoded, err := encodeTaskInput(input)
	if err != nil {
		return personal.Run{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_runs(task_id,revision,event_id,kind,input,status,created_at)
		VALUES(?,?,?,'task',?,'queued',?)`, task.ID, task.Revision, eventID, encoded, timestamp(now))
	if err != nil {
		return personal.Run{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return personal.Run{}, err
	}
	if err := markTaskInputsRunTx(ctx, tx, task.ID, id); err != nil {
		return personal.Run{}, err
	}
	return personal.Run{ID: id, TaskID: task.ID, Revision: task.Revision, EventID: eventID, Kind: personal.RunTask,
		Input: encoded, Status: personal.RunQueued}, nil
}

// createTaskRunTx leaves authorization and transaction ownership with its caller.
func (s *Store) createTaskRunTx(ctx context.Context, tx *sql.Tx, eventID int64, title string, input personal.TaskInput, budgetMS int64, now time.Time) (personal.Task, personal.Run, error) {
	if err := validateTaskInput(title, input); err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	attachments, err := json.Marshal(input.Attachments)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_tasks(title,goal,context,attachments,revision,status,budget_ms,updated_at)
		VALUES(?,?,?,?,1,'queued',?,?)`, title, input.Goal, input.Context, string(attachments), budgetMS, timestamp(now))
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	taskID, err := res.LastInsertId()
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	task := personal.Task{ID: taskID, Title: title, Goal: input.Goal, Context: input.Context, Attachments: input.Attachments, Revision: 1,
		Status: personal.TaskQueued, BudgetMS: budgetMS, UpdatedAt: now}
	run, err := s.taskRun(ctx, tx, &task, eventID, input, now)
	if err != nil {
		return personal.Task{}, personal.Run{}, err
	}
	return task, run, nil
}

// Stage results and execution-time accounting.
