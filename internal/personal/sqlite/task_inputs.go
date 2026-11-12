package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

const taskInputSchema = `
CREATE TABLE IF NOT EXISTS personal_task_inputs (
	task_id INTEGER NOT NULL REFERENCES personal_tasks(id),
	event_id INTEGER NOT NULL REFERENCES personal_events(id),
	revision INTEGER NOT NULL CHECK(revision > 0),
	kind TEXT NOT NULL CHECK(kind IN ('material','feedback')),
	run_id INTEGER REFERENCES personal_runs(id),
	status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','assigned','applied')),
	PRIMARY KEY(task_id,event_id)
);
CREATE INDEX IF NOT EXISTS personal_task_inputs_pending ON personal_task_inputs(task_id,revision,status,event_id);
CREATE INDEX IF NOT EXISTS personal_task_inputs_run ON personal_task_inputs(run_id,status,event_id);
`

type taskInputRequest struct {
	TaskID           int64   `json:"task_id"`
	ExpectedRevision int64   `json:"expected_revision"`
	InputEventIDs    []int64 `json:"input_event_ids"`
}

type taskInputAccessError struct{ reason string }

func (e taskInputAccessError) Error() string { return e.reason }
func (e taskInputAccessError) Unwrap() error { return personal.ErrTaskInputAccess }

func taskInputAccessErrorf(format string, args ...any) error {
	return taskInputAccessError{reason: fmt.Sprintf(format, args...)}
}

// Input discovery and explicit association with a task.
func taskInputKind(event personal.Event) string {
	if len(event.Attachments) > 0 || personal.IsPureURLMessage(event.Text) {
		return "material"
	}
	return "feedback"
}

// PendingInputs returns up to ten consecutive later queued direct-user messages containing only HTTP(S) URLs.
func (s *Store) PendingInputs(ctx context.Context, currentEventID int64) ([]personal.Event, error) {
	if currentEventID <= 0 {
		return nil, errors.New("personal: current event ID must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, currentEventID, false); err != nil {
		return nil, err
	}
	return pendingInputsTx(ctx, tx, currentEventID)
}

func pendingInputsTx(ctx context.Context, tx *sql.Tx, currentEventID int64) ([]personal.Event, error) {
	var channelName, account, sender string
	if err := tx.QueryRowContext(ctx, `SELECT channel,account,sender FROM personal_events WHERE id=?`, currentEventID).
		Scan(&channelName, &account, &sender); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+eventColumns+` FROM personal_events
		WHERE id>? AND kind='user' AND status='queued' AND channel=? AND account=? AND sender=? ORDER BY id`,
		currentEventID, channelName, account, sender)
	if err != nil {
		return nil, err
	}
	events := make([]personal.Event, 0, personal.MaxPendingInputs)
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !personal.IsPureURLMessage(event.Text) || len(event.Attachments) > 0 {
			break
		}
		events = append(events, *event)
		if len(events) == personal.MaxPendingInputs {
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return events, nil
}

// AddTaskInputs records explicitly selected user messages without changing the task goal or revision.
func (s *Store) AddTaskInputs(ctx context.Context, eventID int64, key string, taskID, expectedRevision int64, inputIDs []int64) (personal.Task, error) {
	if !requestKeyValid(key) || taskID <= 0 || expectedRevision <= 0 {
		return personal.Task{}, errors.New("personal: request key, task ID and positive revision are required")
	}
	if len(inputIDs) == 0 || len(inputIDs) > personal.MaxPendingInputs+1 {
		return personal.Task{}, fmt.Errorf("personal: one current event and up to %d pending inputs are required", personal.MaxPendingInputs)
	}
	seen := make(map[int64]bool, len(inputIDs))
	includesCurrent := false
	additionalCount := 0
	for _, id := range inputIDs {
		if id <= 0 || seen[id] {
			return personal.Task{}, errors.New("personal: input event IDs must be positive and unique")
		}
		seen[id] = true
		if id == eventID {
			includesCurrent = true
		} else {
			additionalCount++
		}
	}
	if !includesCurrent || additionalCount > personal.MaxPendingInputs {
		return personal.Task{}, errors.New("personal: input event IDs must include the current event and at most ten pending inputs")
	}
	payload, err := json.Marshal(taskInputRequest{TaskID: taskID, ExpectedRevision: expectedRevision, InputEventIDs: inputIDs})
	if err != nil {
		return personal.Task{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Task{}, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Task{}, err
	}
	storedTaskID, storedRunID, _, found, err := previousRequest(ctx, tx, eventID, key, "add_task_input", string(payload))
	if err != nil {
		return personal.Task{}, err
	}
	if found {
		if storedTaskID != taskID || storedRunID <= 0 {
			return personal.Task{}, errors.New("personal: invalid add_task_input request receipt")
		}
		var belongs bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM personal_runs WHERE id=? AND task_id=? AND kind='task')`, storedRunID, taskID).Scan(&belongs); err != nil {
			return personal.Task{}, err
		}
		if !belongs {
			return personal.Task{}, errors.New("personal: add_task_input receipt references a different task run")
		}
		task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
		if err != nil {
			return personal.Task{}, err
		}
		if err := commitStoreOutcome(ctx, tx, "task.input.add", "replayed", *task,
			zap.Int64("event_id", eventID), zap.Int64("task_id", taskID), zap.Int64("run_id", storedRunID)); err != nil {
			return personal.Task{}, err
		}
		return *task, nil
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
	if task.Status == personal.TaskCompleted || task.Status == personal.TaskCancelled {
		return personal.Task{}, fmt.Errorf("personal: cannot add input to task in state %q", task.Status)
	}
	var latestRunID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM personal_runs WHERE task_id=? AND kind='task' ORDER BY id DESC LIMIT 1`, taskID).Scan(&latestRunID); err != nil {
		return personal.Task{}, err
	}
	current, err := readEventTx(ctx, s, tx, eventID)
	if err != nil {
		return personal.Task{}, err
	}
	if current.Kind != personal.EventUser || current.Channel == "" || current.Account == "" || current.Sender == "" {
		return personal.Task{}, errors.New("personal: current input must be a persisted direct user event")
	}
	for _, id := range inputIDs {
		if id == eventID {
			continue
		}
		if id <= eventID {
			return personal.Task{}, errors.New("personal: additional inputs must follow the current event")
		}
	}
	pending, err := pendingInputsTx(ctx, tx, eventID)
	if err != nil {
		return personal.Task{}, err
	}
	eligible := make(map[int64]personal.Event, len(pending)+1)
	eligible[current.ID] = current
	for _, event := range pending {
		eligible[event.ID] = event
	}
	for _, id := range inputIDs {
		if id != eventID {
			if _, ok := eligible[id]; ok {
				continue
			}
			return personal.Task{}, fmt.Errorf("personal: event %d is not an eligible pending URL input from this user", id)
		}
	}

	if err := mergeReplyInputsTx(ctx, tx, eventID, inputIDs); err != nil {
		return personal.Task{}, err
	}
	if err := bindReplyTaskTx(ctx, tx, eventID, taskID); err != nil {
		return personal.Task{}, err
	}
	for _, id := range inputIDs {
		inputEvent := eligible[id]
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_task_inputs(task_id,event_id,revision,kind,status)
			VALUES(?,?,?,?,'pending') ON CONFLICT(task_id,event_id) DO NOTHING`, taskID, id, expectedRevision, taskInputKind(inputEvent)); err != nil {
			return personal.Task{}, err
		}
	}
	now := timestamp(time.Now())
	for _, id := range inputIDs {
		if id == eventID {
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE personal_events SET status='completed',task_id=? WHERE id=? AND kind='user' AND status='queued'`, taskID, id)
		if err != nil {
			return personal.Task{}, err
		}
		if affected, err := res.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return personal.Task{}, err
			}
			return personal.Task{}, fmt.Errorf("personal: input event %d changed while being selected", id)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_runs(event_id,kind,input,status,created_at)
			VALUES(?,'memory_review',?,'queued',?) ON CONFLICT(event_id) WHERE kind='memory_review' DO NOTHING`, id, fmt.Sprintf(`{"event_id":%d}`, id), now); err != nil {
			return personal.Task{}, err
		}
	}
	if err := s.updateQueuedInitialTaskRunTx(ctx, tx, *task, eventID); err != nil {
		return personal.Task{}, err
	}
	if err := insertRequest(ctx, tx, eventID, key, "add_task_input", string(payload), taskID, latestRunID, 0); err != nil {
		return personal.Task{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "task.input.add", "added", *task,
		zap.Int64("event_id", eventID), zap.Int64("task_id", taskID), zap.Int64("run_id", latestRunID), zap.Int("input_count", len(inputIDs))); err != nil {
		return personal.Task{}, err
	}
	return *task, nil
}

func (s *Store) updateQueuedInitialTaskRunTx(ctx context.Context, tx *sql.Tx, task personal.Task, eventID int64) error {
	if task.Revision != 1 || task.Status != personal.TaskQueued {
		return nil
	}
	var runID int64
	var encoded string
	err := tx.QueryRowContext(ctx, `SELECT id,input FROM personal_runs WHERE task_id=? AND revision=1 AND event_id=? AND kind='task' AND status='queued'
		AND (SELECT COUNT(*) FROM personal_runs WHERE task_id=? AND kind='task')=1`, task.ID, eventID, task.ID).Scan(&runID, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var input personal.TaskInput
	if err := json.Unmarshal([]byte(encoded), &input); err != nil {
		return fmt.Errorf("personal: decode queued initial task input: %w", err)
	}
	input.Context = task.Context
	if err := s.withTaskInputsForRunTx(ctx, tx, task.ID, runID, &input); err != nil {
		return err
	}
	encoded, err = encodeTaskInput(input)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE personal_runs SET input=? WHERE id=? AND task_id=? AND revision=1 AND event_id=? AND kind='task' AND status='queued'`,
		encoded, runID, task.ID, eventID)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return err
		}
		return errors.New("personal: queued initial task run changed while adding inputs")
	}
	return markTaskInputsRunTx(ctx, tx, task.ID, runID)
}

const taskInputPreviewRunes = 160

func taskInputTextPreview(text string) string {
	runes := []rune(text)
	if len(runes) <= taskInputPreviewRunes {
		return text
	}
	return string(runes[:taskInputPreviewRunes]) + "…"
}

// withTaskInputsTx snapshots pending references without copying event bodies into Context.
func (s *Store) withTaskInputsTx(ctx context.Context, tx *sql.Tx, taskID int64, input *personal.TaskInput) error {
	return s.withTaskInputsForRunTx(ctx, tx, taskID, 0, input)
}

func (s *Store) withTaskInputsForRunTx(ctx context.Context, tx *sql.Tx, taskID, runID int64, input *personal.TaskInput) error {
	if taskID <= 0 || runID < 0 || input == nil {
		return errors.New("personal: task ID and task input are required")
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM personal_tasks WHERE id=?`, taskID).Scan(&revision); err != nil {
		return err
	}
	query := `SELECT event_id,kind FROM personal_task_inputs WHERE task_id=? AND revision=? AND status='pending' ORDER BY event_id`
	args := []any{taskID, revision}
	if runID > 0 {
		var runTaskID, runRevision int64
		var runKind personal.RunKind
		if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,kind FROM personal_runs WHERE id=?`, runID).Scan(&runTaskID, &runRevision, &runKind); err != nil {
			return err
		}
		if runTaskID != taskID || runRevision != revision || runKind != personal.RunTask {
			return errors.New("personal: task input run does not match the current task revision")
		}
		query = `SELECT event_id,kind FROM personal_task_inputs WHERE task_id=? AND revision=? AND
			(status='pending' OR (status='assigned' AND run_id=?)) ORDER BY event_id`
		args = append(args, runID)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	type taskInputRow struct {
		eventID int64
		kind    string
	}
	var rowsFound []taskInputRow
	for rows.Next() {
		var item taskInputRow
		if err := rows.Scan(&item.eventID, &item.kind); err != nil {
			_ = rows.Close()
			return err
		}
		rowsFound = append(rowsFound, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	attachments, err := s.canonicalTaskAttachments(ctx, tx, input.Attachments)
	if err != nil {
		return err
	}
	refs := make([]personal.TaskInputReference, 0, len(rowsFound))
	eventAttachments := make([][]channel.Attachment, 0, len(rowsFound))
	for _, item := range rowsFound {
		event, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM personal_events WHERE id=?`, item.eventID))
		if err != nil {
			return err
		}
		if event.Kind != personal.EventUser {
			return fmt.Errorf("personal: task input event %d is not a direct user message", item.eventID)
		}
		attachmentRefs := make([]channel.Attachment, 0, len(event.Attachments))
		validAttachments := make([]channel.Attachment, 0, len(event.Attachments))
		for _, attachment := range event.Attachments {
			if attachment.ID == "" {
				attachmentRefs = append(attachmentRefs, attachment)
				continue
			}
			canonical, err := s.validateStoredAttachment(ctx, tx, attachment.ID)
			if err != nil {
				return err
			}
			attachmentRefs = append(attachmentRefs, canonical)
			validAttachments = append(validAttachments, canonical)
		}
		refs = append(refs, personal.TaskInputReference{EventID: item.eventID, Kind: item.kind, Order: len(refs),
			TextRunes: utf8.RuneCountInString(event.Text), TextPreview: taskInputTextPreview(event.Text), Attachments: attachmentRefs})
		eventAttachments = append(eventAttachments, validAttachments)
	}
	input.InputEvents = refs
	groups := [][]channel.Attachment{attachments}
	groups = append(groups, eventAttachments...)
	input.Attachments = mergeAttachments(groups...)
	return nil
}

// Run snapshots, completion, and recovery of task inputs.
func taskInputRefsFromRun(encoded string) (map[int64]personal.TaskInputReference, error) {
	var input personal.TaskInput
	if err := json.Unmarshal([]byte(encoded), &input); err != nil {
		return nil, fmt.Errorf("personal: decode task input references: %w", err)
	}
	refs := make(map[int64]personal.TaskInputReference, len(input.InputEvents))
	for _, ref := range input.InputEvents {
		if ref.EventID <= 0 || (ref.Kind != "material" && ref.Kind != "feedback") {
			return nil, errors.New("personal: invalid task input reference")
		}
		if _, exists := refs[ref.EventID]; exists {
			return nil, errors.New("personal: duplicate task input reference")
		}
		refs[ref.EventID] = ref
	}
	return refs, nil
}

// markTaskInputsRunTx assigns only pending inputs already present in the saved Run.Input.
func markTaskInputsRunTx(ctx context.Context, tx *sql.Tx, taskID, runID int64) error {
	if taskID <= 0 || runID <= 0 {
		return errors.New("personal: task and run IDs must be positive")
	}
	var runTaskID sql.NullInt64
	var revision int64
	var kind personal.RunKind
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,kind,input FROM personal_runs WHERE id=?`, runID).Scan(&runTaskID, &revision, &kind, &encoded); err != nil {
		return err
	}
	if !runTaskID.Valid || runTaskID.Int64 != taskID || kind != personal.RunTask {
		return errors.New("personal: task input run does not belong to the task")
	}
	refs, err := taskInputRefsFromRun(encoded)
	if err != nil {
		return err
	}
	for eventID := range refs {
		var status string
		var revisionInTable int64
		var assignedRun sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT status,revision,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, taskID, eventID).
			Scan(&status, &revisionInTable, &assignedRun); err != nil {
			return fmt.Errorf("personal: task input reference %d is not registered: %w", eventID, err)
		}
		if revisionInTable != revision ||
			(status != "pending" && !(status == "assigned" && assignedRun.Valid && assignedRun.Int64 == runID)) {
			return fmt.Errorf("personal: task input %d is not pending for run %d", eventID, runID)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_id FROM personal_task_inputs WHERE task_id=? AND revision=? AND status='pending'`, taskID, revision)
	if err != nil {
		return err
	}
	for rows.Next() {
		var eventID int64
		if err := rows.Scan(&eventID); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := refs[eventID]; !ok {
			_ = rows.Close()
			return fmt.Errorf("personal: pending task input %d is missing from run %d input", eventID, runID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE personal_task_inputs SET run_id=?,status='assigned'
		WHERE task_id=? AND revision=? AND status='pending' AND run_id IS NULL`, runID, taskID, revision)
	return err
}
