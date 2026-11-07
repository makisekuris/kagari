package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"kagari/internal/channel"
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

const maxPendingInputs = 10

type taskInputRequest struct {
	TaskID           int64   `json:"task_id"`
	ExpectedRevision int64   `json:"expected_revision"`
	InputEventIDs    []int64 `json:"input_event_ids"`
}

type taskInputAccessError struct{ reason string }

func (e taskInputAccessError) Error() string { return e.reason }

func taskInputAccessErrorf(format string, args ...any) error {
	return taskInputAccessError{reason: fmt.Sprintf(format, args...)}
}

// isPureURLMessage accepts one or more whitespace-separated HTTP(S) URLs and nothing else.
func isPureURLMessage(text string) bool {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		u, err := url.Parse(part)
		if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" ||
			(!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
			return false
		}
	}
	return true
}

func taskInputKind(event Event) string {
	if len(event.Attachments) > 0 || isPureURLMessage(event.Text) {
		return "material"
	}
	return "feedback"
}

// PendingInputs returns up to ten consecutive later queued direct-user messages containing only HTTP(S) URLs.
func (s *Store) PendingInputs(ctx context.Context, currentEventID int64) ([]Event, error) {
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

func pendingInputsTx(ctx context.Context, tx *sql.Tx, currentEventID int64) ([]Event, error) {
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
	events := make([]Event, 0, maxPendingInputs)
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !isPureURLMessage(event.Text) || len(event.Attachments) > 0 {
			break
		}
		events = append(events, *event)
		if len(events) == maxPendingInputs {
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
func (s *Store) AddTaskInputs(ctx context.Context, eventID int64, key string, taskID, expectedRevision int64, inputIDs []int64) (Task, error) {
	if !requestKeyValid(key) || taskID <= 0 || expectedRevision <= 0 {
		return Task{}, errors.New("personal: request key, task ID and positive revision are required")
	}
	if len(inputIDs) == 0 || len(inputIDs) > maxPendingInputs+1 {
		return Task{}, fmt.Errorf("personal: one current event and up to %d pending inputs are required", maxPendingInputs)
	}
	seen := make(map[int64]bool, len(inputIDs))
	includesCurrent := false
	additionalCount := 0
	for _, id := range inputIDs {
		if id <= 0 || seen[id] {
			return Task{}, errors.New("personal: input event IDs must be positive and unique")
		}
		seen[id] = true
		if id == eventID {
			includesCurrent = true
		} else {
			additionalCount++
		}
	}
	if !includesCurrent || additionalCount > maxPendingInputs {
		return Task{}, errors.New("personal: input event IDs must include the current event and at most ten pending inputs")
	}
	payload, err := json.Marshal(taskInputRequest{TaskID: taskID, ExpectedRevision: expectedRevision, InputEventIDs: inputIDs})
	if err != nil {
		return Task{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return Task{}, err
	}
	storedTaskID, storedRunID, _, found, err := previousRequest(ctx, tx, eventID, key, "add_task_input", string(payload))
	if err != nil {
		return Task{}, err
	}
	if found {
		if storedTaskID != taskID || storedRunID <= 0 {
			return Task{}, errors.New("personal: invalid add_task_input request receipt")
		}
		var belongs bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM personal_runs WHERE id=? AND task_id=? AND kind='task')`, storedRunID, taskID).Scan(&belongs); err != nil {
			return Task{}, err
		}
		if !belongs {
			return Task{}, errors.New("personal: add_task_input receipt references a different task run")
		}
		task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
		if err != nil {
			return Task{}, err
		}
		if err := commitStoreOutcome(ctx, tx, "task.input.add", "replayed", *task,
			zap.Int64("event_id", eventID), zap.Int64("task_id", taskID), zap.Int64("run_id", storedRunID)); err != nil {
			return Task{}, err
		}
		return *task, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return Task{}, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return Task{}, err
	}
	if task.Revision != expectedRevision {
		return Task{}, fmt.Errorf("personal: task %d revision is %d, expected %d", taskID, task.Revision, expectedRevision)
	}
	if task.Status == TaskCompleted || task.Status == TaskCancelled {
		return Task{}, fmt.Errorf("personal: cannot add input to task in state %q", task.Status)
	}
	var latestRunID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM personal_runs WHERE task_id=? AND kind='task' ORDER BY id DESC LIMIT 1`, taskID).Scan(&latestRunID); err != nil {
		return Task{}, err
	}
	current, err := readEventTx(ctx, s, tx, eventID)
	if err != nil {
		return Task{}, err
	}
	if current.Kind != EventUser || current.Channel == "" || current.Account == "" || current.Sender == "" {
		return Task{}, errors.New("personal: current input must be a persisted direct user event")
	}
	for _, id := range inputIDs {
		if id == eventID {
			continue
		}
		if id <= eventID {
			return Task{}, errors.New("personal: additional inputs must follow the current event")
		}
	}
	pending, err := pendingInputsTx(ctx, tx, eventID)
	if err != nil {
		return Task{}, err
	}
	eligible := make(map[int64]Event, len(pending)+1)
	eligible[current.ID] = current
	for _, event := range pending {
		eligible[event.ID] = event
	}
	for _, id := range inputIDs {
		if id != eventID {
			if _, ok := eligible[id]; ok {
				continue
			}
			return Task{}, fmt.Errorf("personal: event %d is not an eligible pending URL input from this user", id)
		}
	}

	if err := mergeReplyInputsTx(ctx, tx, eventID, inputIDs); err != nil {
		return Task{}, err
	}
	if err := bindReplyTaskTx(ctx, tx, eventID, taskID); err != nil {
		return Task{}, err
	}
	for _, id := range inputIDs {
		inputEvent := eligible[id]
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_task_inputs(task_id,event_id,revision,kind,status)
			VALUES(?,?,?,?,'pending') ON CONFLICT(task_id,event_id) DO NOTHING`, taskID, id, expectedRevision, taskInputKind(inputEvent)); err != nil {
			return Task{}, err
		}
	}
	now := timestamp(time.Now())
	for _, id := range inputIDs {
		if id == eventID {
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE personal_events SET status='completed',task_id=? WHERE id=? AND kind='user' AND status='queued'`, taskID, id)
		if err != nil {
			return Task{}, err
		}
		if affected, err := res.RowsAffected(); err != nil || affected != 1 {
			if err != nil {
				return Task{}, err
			}
			return Task{}, fmt.Errorf("personal: input event %d changed while being selected", id)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_runs(event_id,kind,input,status,created_at)
			VALUES(?,'memory_review',?,'queued',?) ON CONFLICT(event_id) WHERE kind='memory_review' DO NOTHING`, id, fmt.Sprintf(`{"event_id":%d}`, id), now); err != nil {
			return Task{}, err
		}
	}
	if err := s.updateQueuedInitialTaskRunTx(ctx, tx, *task, eventID); err != nil {
		return Task{}, err
	}
	if err := insertRequest(ctx, tx, eventID, key, "add_task_input", string(payload), taskID, latestRunID, 0); err != nil {
		return Task{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "task.input.add", "added", *task,
		zap.Int64("event_id", eventID), zap.Int64("task_id", taskID), zap.Int64("run_id", latestRunID), zap.Int("input_count", len(inputIDs))); err != nil {
		return Task{}, err
	}
	return *task, nil
}

func (s *Store) updateQueuedInitialTaskRunTx(ctx context.Context, tx *sql.Tx, task Task, eventID int64) error {
	if task.Revision != 1 || task.Status != TaskQueued {
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
	var input TaskInput
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
func (s *Store) withTaskInputsTx(ctx context.Context, tx *sql.Tx, taskID int64, input *TaskInput) error {
	return s.withTaskInputsForRunTx(ctx, tx, taskID, 0, input)
}

func (s *Store) withTaskInputsForRunTx(ctx context.Context, tx *sql.Tx, taskID, runID int64, input *TaskInput) error {
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
		var runKind RunKind
		if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,kind FROM personal_runs WHERE id=?`, runID).Scan(&runTaskID, &runRevision, &runKind); err != nil {
			return err
		}
		if runTaskID != taskID || runRevision != revision || runKind != RunTask {
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
	refs := make([]TaskInputReference, 0, len(rowsFound))
	eventAttachments := make([][]channel.Attachment, 0, len(rowsFound))
	for _, item := range rowsFound {
		event, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM personal_events WHERE id=?`, item.eventID))
		if err != nil {
			return err
		}
		if event.Kind != EventUser {
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
		refs = append(refs, TaskInputReference{EventID: item.eventID, Kind: item.kind, Order: len(refs),
			TextRunes: runeLen(event.Text), TextPreview: taskInputTextPreview(event.Text), Attachments: attachmentRefs})
		eventAttachments = append(eventAttachments, validAttachments)
	}
	input.InputEvents = refs
	groups := [][]channel.Attachment{attachments}
	groups = append(groups, eventAttachments...)
	input.Attachments = mergeAttachments(groups...)
	return nil
}

func taskInputRefsFromRun(encoded string) (map[int64]TaskInputReference, error) {
	var input TaskInput
	if err := json.Unmarshal([]byte(encoded), &input); err != nil {
		return nil, fmt.Errorf("personal: decode task input references: %w", err)
	}
	refs := make(map[int64]TaskInputReference, len(input.InputEvents))
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
	var kind RunKind
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,kind,input FROM personal_runs WHERE id=?`, runID).Scan(&runTaskID, &revision, &kind, &encoded); err != nil {
		return err
	}
	if !runTaskID.Valid || runTaskID.Int64 != taskID || kind != RunTask {
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

// PendingTaskInputs returns user events still awaiting an explicit processed report.
func (s *Store) PendingTaskInputs(ctx context.Context, taskID int64) ([]Event, error) {
	if taskID <= 0 {
		return nil, errors.New("personal: task ID must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.`+strings.ReplaceAll(eventColumns, ",", ",e.")+`
		FROM personal_task_inputs i JOIN personal_events e ON e.id=i.event_id WHERE i.task_id=? AND i.status IN ('pending','assigned') ORDER BY e.id`, taskID)
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0)
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
			WHERE task_id=? AND status IN ('pending','assigned') ORDER BY event_id LIMIT ?`, taskID, maxPendingInputs)
		if err != nil {
			return err
		}
		ids := make([]string, 0, maxPendingInputs)
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
func (s *Store) ReadTaskInput(ctx context.Context, runID, eventID int64) (Event, error) {
	if runID <= 0 || eventID <= 0 {
		return Event{}, taskInputAccessErrorf("personal: run and input event IDs must be positive")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, runID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Event{}, taskInputAccessErrorf("personal: task run %d does not exist", runID)
		}
		return Event{}, err
	}
	if run.Kind != RunTask || run.TaskID <= 0 || run.Status != RunRunning {
		return Event{}, taskInputAccessErrorf("personal: task input reads require the active task run")
	}
	var taskRevision int64
	var taskStatus TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, run.TaskID).Scan(&taskRevision, &taskStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Event{}, taskInputAccessErrorf("personal: task %d does not exist", run.TaskID)
		}
		return Event{}, err
	}
	if taskRevision != run.Revision || taskStatus != TaskRunning {
		return Event{}, taskInputAccessErrorf("personal: task input belongs to a stale run")
	}
	refs, err := taskInputRefsFromRun(run.Input)
	if err != nil {
		return Event{}, err
	}
	if _, ok := refs[eventID]; !ok {
		return Event{}, taskInputAccessErrorf("personal: event %d is not in run %d input", eventID, runID)
	}
	var status string
	var revision int64
	var assignedRun sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT status,revision,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, run.TaskID, eventID).
		Scan(&status, &revision, &assignedRun); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Event{}, taskInputAccessErrorf("personal: event %d is not registered for task %d", eventID, run.TaskID)
		}
		return Event{}, err
	}
	if status != "assigned" || revision != run.Revision || !assignedRun.Valid || assignedRun.Int64 != runID {
		return Event{}, taskInputAccessErrorf("personal: event %d is not assigned to run %d", eventID, runID)
	}
	event, err := readEventTx(ctx, s, tx, eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrAttachmentUnavailable) {
			return Event{}, taskInputAccessErrorf("personal: event %d is unavailable", eventID)
		}
		return Event{}, err
	}
	if event.Kind != EventUser {
		return Event{}, taskInputAccessErrorf("personal: task input event %d is not a user message", eventID)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, err
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
	var kind RunKind
	var status RunStatus
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,kind,status,input FROM personal_runs WHERE id=?`, runID).
		Scan(&taskID, &revision, &kind, &status, &encoded); err != nil {
		return err
	}
	if kind != RunTask || status != RunRunning {
		return errors.New("personal: task input claim requires the active task run")
	}
	var taskRevision int64
	var taskStatus TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&taskRevision, &taskStatus); err != nil {
		return err
	}
	if taskRevision != revision || taskStatus != TaskRunning {
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
	var kind RunKind
	var status RunStatus
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,kind,status,input FROM personal_runs WHERE id=?`, runID).
		Scan(&taskID, &runRevision, &kind, &status, &encoded); err != nil {
		return err
	}
	if kind != RunTask || runRevision != revision || status != RunRunning {
		return errors.New("personal: task input result does not match the active run revision")
	}
	var taskRevision int64
	var taskStatus TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&taskRevision, &taskStatus); err != nil {
		return err
	}
	if taskRevision != revision || taskStatus != TaskRunning {
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

func readEventTx(ctx context.Context, s *Store, tx *sql.Tx, eventID int64) (Event, error) {
	event, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM personal_events WHERE id=?`, eventID))
	if err != nil {
		return Event{}, err
	}
	if err := s.refreshAttachments(ctx, tx, event.Attachments); err != nil {
		return Event{}, err
	}
	return *event, nil
}
