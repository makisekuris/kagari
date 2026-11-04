package personal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

func (s *Store) BindOwner(ctx context.Context, ownerID int64) error {
	if ownerID == 0 {
		return errors.New("personal: owner ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO personal_meta(key,value) VALUES('owner_id',?) ON CONFLICT(key) DO NOTHING`, fmt.Sprint(ownerID)); err != nil {
		return err
	}
	var bound string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM personal_meta WHERE key='owner_id'`).Scan(&bound); err != nil {
		return err
	}
	if bound != fmt.Sprint(ownerID) {
		return fmt.Errorf("personal: owner is already bound to ID %s", bound)
	}
	return tx.Commit()
}

func (s *Store) Offset(ctx context.Context) (int64, error) {
	var offset int64
	err := s.db.QueryRowContext(ctx, `SELECT next_offset FROM personal_state WHERE id=1`).Scan(&offset)
	return offset, err
}

func (s *Store) AcceptInput(ctx context.Context, updateID int64, input *Input) (int64, error) {
	if updateID < 0 || updateID == math.MaxInt64 {
		return 0, errors.New("personal: update ID is out of range")
	}
	if input != nil && (input.MessageID <= 0 || strings.TrimSpace(input.Text) == "") {
		return 0, errors.New("personal: inbound message ID and text are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var offset int64
	if err := tx.QueryRowContext(ctx, `SELECT next_offset FROM personal_state WHERE id=1`).Scan(&offset); err != nil {
		return 0, err
	}
	if updateID < offset {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return 0, nil
	}
	var eventID int64
	if input != nil {
		kind := EventUser
		if input.Material {
			kind = EventMaterial
		}
		taskID, err := replyTaskID(ctx, tx, input.ReplyToID)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,task_id,message_id,reply_to_id,status,created_at)
			VALUES(?,?,?,?,?,'queued',?)`, kind, input.Text, nullableID(taskID), input.MessageID, nullablePositive(input.ReplyToID), timestamp(time.Now()))
		if err != nil {
			return 0, err
		}
		eventID, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
		role := "user"
		if input.Material {
			role = "material"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_history(event_id,role,text) VALUES(?,?,?)`, eventID, role, input.Text); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_state SET next_offset=? WHERE id=1 AND next_offset<=?`, updateID+1, updateID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return eventID, nil
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullablePositive(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func replyTaskID(ctx context.Context, tx *sql.Tx, replyToID int64) (int64, error) {
	if replyToID <= 0 {
		return 0, nil
	}
	var id sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT e.task_id FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id
		WHERE d.receipt=? AND d.status='sent' AND e.task_id IS NOT NULL ORDER BY d.id DESC LIMIT 1`, replyToID).Scan(&id)
	if err == nil && id.Valid {
		return id.Int64, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM personal_events WHERE message_id=?
		AND kind IN ('user','material') AND task_id IS NOT NULL ORDER BY id DESC LIMIT 1`, replyToID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id.Int64, nil
}

func (s *Store) ClaimEvent(ctx context.Context) (*Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM personal_events WHERE status='queued' ORDER BY id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE personal_events SET status='running' WHERE id=? AND status='queued'`, id)
	if err != nil {
		return nil, err
	}
	if affected, err := res.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("personal: event claim lost")
	}
	event, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM personal_events WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return event, nil
}

type scanner interface{ Scan(...any) error }

func scanEvent(row scanner) (*Event, error) {
	var event Event
	var taskID, runID, messageID, replyToID sql.NullInt64
	var created string
	if err := row.Scan(&event.ID, &event.Kind, &event.Text, &taskID, &runID, &messageID, &replyToID, &event.Interrupted, &created); err != nil {
		return nil, err
	}
	event.TaskID = taskID.Int64
	event.RunID = runID.Int64
	event.MessageID = messageID.Int64
	event.ReplyToID = replyToID.Int64
	createdAt, err := parseTimestamp(created)
	event.CreatedAt = createdAt
	return &event, err
}

const eventColumns = `id,kind,text,task_id,run_id,message_id,reply_to_id,interrupted,created_at`

func (s *Store) History(ctx context.Context, eventID int64, limit int) ([]Message, error) {
	if eventID <= 0 || limit <= 0 {
		return []Message{}, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT h.event_id,h.role,h.text,e.task_id FROM personal_history h JOIN personal_events e ON e.id=h.event_id
		WHERE h.event_id<? ORDER BY h.event_id DESC,h.id DESC LIMIT ?`, eventID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		var taskID sql.NullInt64
		if err := rows.Scan(&m.EventID, &m.Role, &m.Text, &taskID); err != nil {
			return nil, err
		}
		m.TaskID = taskID.Int64
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}

func (s *Store) CompleteEvent(ctx context.Context, eventID int64, output TurnOutput, chunks []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind EventKind
	var status EventStatus
	var callbackTaskID, callbackRunID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,task_id,run_id FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &status, &callbackTaskID, &callbackRunID); err != nil {
		return err
	}
	if status != EventRunning {
		return fmt.Errorf("personal: cannot complete event %d in state %q", eventID, status)
	}
	if kind == EventTask {
		current, err := currentTaskCallback(ctx, tx, eventID, callbackTaskID.Int64, callbackRunID.Int64)
		if err != nil {
			return err
		}
		if !current {
			// A user revision or cancellation may arrive while the callback is thinking.
			output, chunks = TurnOutput{Silent: true}, nil
		} else if output.TaskID != 0 && output.TaskID != callbackTaskID.Int64 {
			return errors.New("personal: task callback cannot link its reply to another task")
		}
	}
	taskID := output.TaskID
	if taskID != 0 {
		if _, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID)); err != nil {
			return fmt.Errorf("personal: event task %d: %w", taskID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_events SET status='completed',task_id=? WHERE id=? AND status='running'`, nullableID(taskID), eventID); err != nil {
		return err
	}
	if output.Text != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_history(event_id,role,text) VALUES(?,'assistant',?)`, eventID, output.Text); err != nil {
			return err
		}
	}
	if !output.Silent && output.Text != "" {
		now := timestamp(time.Now())
		part := 0
		for _, chunk := range chunks {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			part++
			if _, err := tx.ExecContext(ctx, `INSERT INTO personal_outbox(event_id,part,text,status,next_attempt_at) VALUES(?,?,?,'pending',?)`, eventID, part, chunk, now); err != nil {
				return err
			}
		}
	}
	if kind == EventUser {
		raw := fmt.Sprintf(`{"event_id":%d}`, eventID)
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_runs(event_id,kind,input,status,created_at)
			VALUES(?,? ,?,'queued',?) ON CONFLICT(event_id) WHERE kind='memory_review' DO NOTHING`, eventID, RunMemoryReview, raw, timestamp(time.Now())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func currentTaskCallback(ctx context.Context, tx *sql.Tx, eventID, taskID, runID int64) (bool, error) {
	if taskID <= 0 || runID <= 0 {
		return false, nil
	}
	var runTaskID, runRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision FROM personal_runs WHERE id=? AND kind='task'`, runID).
		Scan(&runTaskID, &runRevision); err != nil {
		return false, err
	}
	var taskRevision int64
	var taskStatus TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).
		Scan(&taskRevision, &taskStatus); err != nil {
		return false, err
	}
	if runTaskID != taskID || runRevision != taskRevision || taskStatus == TaskCancelled {
		return false, nil
	}
	var latestID, latestRevision, latestEventID int64
	if err := tx.QueryRowContext(ctx, `SELECT id,revision,event_id FROM personal_runs WHERE task_id=? ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&latestID, &latestRevision, &latestEventID); err != nil {
		return false, err
	}
	return latestRevision == taskRevision && (latestID == runID || latestEventID == eventID), nil
}

func (s *Store) Recover(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Callback time is absolute within one claim; task spend itself stays cumulative.
	if _, err := tx.ExecContext(ctx, `UPDATE personal_events SET status='queued',elapsed_ms=0,interrupted=1 WHERE status='running'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,task_id,run_id,status,created_at)
		SELECT 'task','任务预算在中断前已用完；已有阶段成果已保留。',r.task_id,r.id,'queued',?
		FROM personal_runs r JOIN personal_tasks t ON t.id=r.task_id
		WHERE r.kind='task' AND r.status='running' AND t.status='running' AND r.revision=t.revision AND t.used_ms>=t.budget_ms
		AND NOT EXISTS(SELECT 1 FROM personal_events e WHERE e.run_id=r.id)`, timestamp(time.Now())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='failed',error=CASE WHEN error='' THEN 'task budget exhausted during interrupted run' ELSE error END
		WHERE kind='task' AND status='running' AND EXISTS(SELECT 1 FROM personal_tasks t WHERE t.id=personal_runs.task_id
		AND t.status='running' AND t.revision=personal_runs.revision AND t.used_ms>=t.budget_ms)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status='waiting_budget',updated_at=?
		WHERE status='running' AND used_ms>=budget_ms`, timestamp(time.Now())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='queued' WHERE status='running'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status='queued',updated_at=? WHERE status='running'`, timestamp(time.Now())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_outbox SET status='uncertain',detail='outcome unknown after restart' WHERE status='sending'`); err != nil {
		return err
	}
	return tx.Commit()
}
