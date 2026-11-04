package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"kagari/internal/channel"
	"kagari/internal/logging"
)

func (s *Store) BindChannel(ctx context.Context, channelName, account, sender string, rebind bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	oldAccount, oldSender, exists, err := checkChannelBinding(ctx, tx, channelName, account, sender, rebind)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_bindings(channel,account,sender) VALUES(?,?,?)`, channelName, account, sender); err != nil {
			return err
		}
	} else if oldAccount != account || oldSender != sender {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_bindings SET account=?,sender=? WHERE channel=?`, account, sender, channelName); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type channelBindingQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkChannelBinding(ctx context.Context, query channelBindingQuery, channelName, account, sender string, rebind bool) (string, string, bool, error) {
	if strings.TrimSpace(channelName) == "" || strings.TrimSpace(account) == "" || strings.TrimSpace(sender) == "" {
		return "", "", false, errors.New("personal: channel, account and sender are required for binding")
	}
	var oldAccount, oldSender string
	err := query.QueryRowContext(ctx, `SELECT account,sender FROM personal_bindings WHERE channel=?`, channelName).Scan(&oldAccount, &oldSender)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if (oldAccount != account || oldSender != sender) && !rebind {
		return oldAccount, oldSender, true, fmt.Errorf("personal: %s is already bound to account %q and sender %q; explicit rebind required", channelName, oldAccount, oldSender)
	}
	return oldAccount, oldSender, true, nil
}

// CheckChannelBinding validates a prospective binding without changing it.
func (s *Store) CheckChannelBinding(ctx context.Context, channelName, account, sender string, rebind bool) error {
	_, _, _, err := checkChannelBinding(ctx, s.db, channelName, account, sender, rebind)
	return err
}

func (s *Store) Cursor(ctx context.Context, channelName, account string) (string, error) {
	if strings.TrimSpace(channelName) == "" || strings.TrimSpace(account) == "" {
		return "", errors.New("personal: channel and account are required")
	}
	var cursor string
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM personal_inbox WHERE channel=? AND account=?`, channelName, account).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

func (s *Store) AcceptBatch(ctx context.Context, channelName, account, cursor string, inputs []channel.Input) ([]int64, error) {
	if strings.TrimSpace(channelName) == "" || strings.TrimSpace(account) == "" {
		return nil, errors.New("personal: channel and account are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var boundAccount, boundSender string
	if err := tx.QueryRowContext(ctx, `SELECT account,sender FROM personal_bindings WHERE channel=?`, channelName).Scan(&boundAccount, &boundSender); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("personal: %s is not bound", channelName)
		}
		return nil, err
	}
	if boundAccount != account {
		return nil, fmt.Errorf("personal: account %q is not bound to %s", account, channelName)
	}
	ids := make([]int64, len(inputs))
	for i, input := range inputs {
		if input.Channel != channelName || input.Account != account || strings.TrimSpace(input.Sender) == "" || strings.TrimSpace(input.MessageID) == "" {
			return nil, fmt.Errorf("personal: input %d has incomplete or mismatched source identifiers", i)
		}
		if input.Sender != boundSender {
			return nil, fmt.Errorf("personal: sender %q is not bound to %s account %q", input.Sender, channelName, account)
		}
		var existing int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM personal_events WHERE channel=? AND account=? AND sender=? AND message_id=?`,
			channelName, account, input.Sender, input.MessageID).Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		attachments, err := s.canonicalInputAttachments(ctx, tx, input.Attachments)
		if err != nil {
			return nil, fmt.Errorf("personal: input %d attachments: %w", i, err)
		}
		if strings.TrimSpace(input.Text) == "" && len(attachments) == 0 {
			continue
		}
		kind := EventUser
		role := "user"
		if input.Material {
			kind, role = EventMaterial, "material"
		}
		taskID, err := replyTaskID(ctx, tx, input)
		if err != nil {
			return nil, err
		}
		attachmentJSON, err := json.Marshal(attachments)
		if err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,task_id,channel,account,sender,message_id,reply_to_id,attachments,status,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,'queued',?)`, kind, input.Text, nullableID(taskID), channelName, account, input.Sender,
			input.MessageID, input.ReplyToID, string(attachmentJSON), timestamp(time.Now()))
		if err != nil {
			return nil, err
		}
		ids[i], err = res.LastInsertId()
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_history(event_id,role,text,attachments) VALUES(?,?,?,?)`, ids[i], role, input.Text, string(attachmentJSON)); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO personal_inbox(channel,account,cursor) VALUES(?,?,?)
		ON CONFLICT(channel,account) DO UPDATE SET cursor=excluded.cursor`, channelName, account, cursor); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *Store) canonicalInputAttachments(ctx context.Context, tx *sql.Tx, input []channel.Attachment) ([]channel.Attachment, error) {
	if len(input) == 0 {
		return nil, nil
	}
	attachments := make([]channel.Attachment, 0, len(input))
	for _, attachment := range input {
		if attachment.Size < 0 {
			return nil, fmt.Errorf("attachment %q has an invalid size", attachment.Name)
		}
		if attachment.ID == "" {
			if attachment.Error == "" {
				return nil, errors.New("attachment without a local ID must have a failure reason")
			}
			attachments = append(attachments, attachment)
			continue
		}
		canonical, err := s.validateStoredAttachment(ctx, tx, attachment.ID)
		if err != nil {
			return nil, err
		}
		if canonical.MIME == "" {
			canonical.MIME = attachment.MIME
		}
		attachments = append(attachments, canonical)
	}
	return attachments, nil
}

func replyTaskID(ctx context.Context, tx *sql.Tx, input channel.Input) (int64, error) {
	if input.ReplyToID == "" {
		return 0, nil
	}
	var id sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT e.task_id FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id
		WHERE d.receipt_channel=? AND d.receipt_account=? AND d.receipt_conversation=? AND d.receipt_message_id=?
		AND d.status='sent' AND e.task_id IS NOT NULL ORDER BY d.id DESC LIMIT 1`,
		input.Channel, input.Account, input.Sender, input.ReplyToID).Scan(&id)
	if err == nil && id.Valid {
		return id.Int64, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM personal_events WHERE channel=? AND account=? AND sender=? AND message_id=?
		AND kind IN ('user','material') AND task_id IS NOT NULL ORDER BY id DESC LIMIT 1`,
		input.Channel, input.Account, input.Sender, input.ReplyToID).Scan(&id)
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
	if err := s.refreshAttachments(ctx, tx, event.Attachments); err != nil {
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
	var taskID, runID sql.NullInt64
	var attachments, created string
	if err := row.Scan(&event.ID, &event.Kind, &event.Text, &taskID, &runID, &event.Channel, &event.Account, &event.Sender,
		&event.MessageID, &event.ReplyToID, &attachments, &event.Interrupted, &created); err != nil {
		return nil, err
	}
	event.TaskID, event.RunID = taskID.Int64, runID.Int64
	if err := json.Unmarshal([]byte(attachments), &event.Attachments); err != nil {
		return nil, fmt.Errorf("personal: decode event attachments: %w", err)
	}
	if event.Attachments == nil {
		event.Attachments = []channel.Attachment{}
	}
	var err error
	event.CreatedAt, err = parseTimestamp(created)
	return &event, err
}

const eventColumns = `id,kind,text,task_id,run_id,channel,account,sender,message_id,reply_to_id,attachments,interrupted,created_at`

func (s *Store) History(ctx context.Context, eventID int64, limit int) ([]Message, error) {
	if eventID <= 0 || limit <= 0 {
		return []Message{}, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT h.event_id,h.role,h.text,e.task_id,e.channel,e.account,e.sender,e.message_id,e.reply_to_id,h.attachments
		FROM personal_history h JOIN personal_events e ON e.id=h.event_id
		WHERE h.event_id<? ORDER BY h.event_id DESC,h.id DESC LIMIT ?`, eventID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		var taskID sql.NullInt64
		var attachments string
		if err := rows.Scan(&m.EventID, &m.Role, &m.Text, &taskID, &m.Channel, &m.Account, &m.Sender, &m.MessageID, &m.ReplyToID, &attachments); err != nil {
			return nil, err
		}
		m.TaskID = taskID.Int64
		if err := json.Unmarshal([]byte(attachments), &m.Attachments); err != nil {
			return nil, fmt.Errorf("personal: decode history attachments: %w", err)
		}
		if m.Attachments == nil {
			m.Attachments = []channel.Attachment{}
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range messages {
		if err := s.refreshAttachments(ctx, s.db, messages[i].Attachments); err != nil {
			return nil, err
		}
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
	discardedStaleCallback := false
	if kind == EventTask {
		current, err := currentTaskCallback(ctx, tx, eventID, callbackTaskID.Int64, callbackRunID.Int64)
		if err != nil {
			return err
		}
		if !current {
			output, chunks = TurnOutput{Silent: true}, nil
			discardedStaleCallback = true
		} else if output.TaskID != 0 && output.TaskID != callbackTaskID.Int64 {
			return errors.New("personal: task callback cannot link its reply to another task")
		}
	}
	attachments := make([]channel.Attachment, 0, len(output.Attachments))
	if !output.Silent {
		for _, id := range output.Attachments {
			attachment, err := s.validateStoredAttachment(ctx, tx, id)
			if err != nil {
				return err
			}
			attachments = append(attachments, attachment)
		}
	} else {
		output.Attachments = nil
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
	if output.Text != "" || len(attachments) > 0 {
		attachmentJSON, err := json.Marshal(attachments)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_history(event_id,role,text,attachments) VALUES(?,'assistant',?,?)`, eventID, output.Text, string(attachmentJSON)); err != nil {
			return err
		}
	}
	replyParts := 0
	if !output.Silent {
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
			replyParts = part
		}
		for _, attachment := range attachments {
			part++
			if _, err := tx.ExecContext(ctx, `INSERT INTO personal_outbox(event_id,part,text,attachment_id,status,next_attempt_at) VALUES(?,?,? ,?,'pending',?)`, eventID, part, "", attachment.ID, now); err != nil {
				return err
			}
			replyParts = part
		}
	}
	if kind == EventUser {
		raw := fmt.Sprintf(`{"event_id":%d}`, eventID)
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_runs(event_id,kind,input,status,created_at)
			VALUES(?,? ,?,'queued',?) ON CONFLICT(event_id) WHERE kind='memory_review' DO NOTHING`, eventID, RunMemoryReview, raw, timestamp(time.Now())); err != nil {
			return err
		}
	}
	outcome := "completed"
	if discardedStaleCallback {
		outcome = "discarded_stale_callback"
	}
	if err := commitStoreOutcome(ctx, tx, "event.complete", outcome, nil,
		zap.Int64("event_id", eventID), zap.String("event_kind", string(kind)),
		zap.Int64("task_id", taskID), zap.Int64("run_id", callbackRunID.Int64),
		zap.String("event_status", string(EventCompleted)), zap.Bool("silent", output.Silent), zap.Int("reply_part_count", replyParts)); err != nil {
		return err
	}
	if replyParts > 0 {
		logging.FromContext(ctx).Info("conversation.reply_queued", zap.Int64("event_id", eventID), zap.Int64("task_id", taskID),
			zap.Int64("run_id", callbackRunID.Int64), zap.Int("parts", replyParts), zap.Any("payload", output.Text), zap.Any("attachments", output.Attachments))
	}
	return nil
}

func currentTaskCallback(ctx context.Context, tx *sql.Tx, eventID, taskID, runID int64) (bool, error) {
	if taskID <= 0 || runID <= 0 {
		return false, nil
	}
	var runTaskID, runRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision FROM personal_runs WHERE id=? AND kind='task'`, runID).Scan(&runTaskID, &runRevision); err != nil {
		return false, err
	}
	var taskRevision int64
	var taskStatus TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&taskRevision, &taskStatus); err != nil {
		return false, err
	}
	if runTaskID != taskID || runRevision != taskRevision || taskStatus == TaskCancelled {
		return false, nil
	}
	var latestID, latestRevision, latestEventID int64
	if err := tx.QueryRowContext(ctx, `SELECT id,revision,event_id FROM personal_runs WHERE task_id=? ORDER BY id DESC LIMIT 1`, taskID).Scan(&latestID, &latestRevision, &latestEventID); err != nil {
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
	var summary recoveryOutcomePayload
	res, err := tx.ExecContext(ctx, `UPDATE personal_events SET status='queued',elapsed_ms=0,interrupted=1 WHERE status='running'`)
	if err != nil {
		return err
	}
	if summary.RequeuedEvents, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,task_id,run_id,status,created_at)
		SELECT 'task','任务预算在中断前已用完；已有阶段成果已保留。',r.task_id,r.id,'queued',?
		FROM personal_runs r JOIN personal_tasks t ON t.id=r.task_id
		WHERE r.kind='task' AND r.status='running' AND t.status='running' AND r.revision=t.revision AND t.used_ms>=t.budget_ms
		AND NOT EXISTS(SELECT 1 FROM personal_events e WHERE e.run_id=r.id)`, timestamp(time.Now()))
	if err != nil {
		return err
	}
	if summary.BudgetEvents, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_runs SET status='failed',error=CASE WHEN error='' THEN 'task budget exhausted during interrupted run' ELSE error END
		WHERE kind='task' AND status='running' AND EXISTS(SELECT 1 FROM personal_tasks t WHERE t.id=personal_runs.task_id
		AND t.status='running' AND t.revision=personal_runs.revision AND t.used_ms>=t.budget_ms)`)
	if err != nil {
		return err
	}
	if summary.FailedRuns, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_tasks SET status='waiting_budget',updated_at=?
		WHERE status='running' AND used_ms>=budget_ms`, timestamp(time.Now()))
	if err != nil {
		return err
	}
	if summary.WaitingTasks, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_runs SET status='queued' WHERE status='running'`)
	if err != nil {
		return err
	}
	if summary.RequeuedRuns, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_tasks SET status='queued',updated_at=? WHERE status='running'`, timestamp(time.Now()))
	if err != nil {
		return err
	}
	if summary.QueuedTasks, err = changedRows(res); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE personal_outbox SET status='uncertain',detail='outcome unknown after restart' WHERE status='sending'`)
	if err != nil {
		return err
	}
	if summary.UncertainSends, err = changedRows(res); err != nil {
		return err
	}
	return commitStoreOutcome(ctx, tx, "store.recover", "recovered", summary,
		zap.Int64("requeued_events", summary.RequeuedEvents), zap.Int64("requeued_runs", summary.RequeuedRuns), zap.Int64("uncertain_sends", summary.UncertainSends))
}
