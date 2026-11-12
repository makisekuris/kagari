package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

// Event queries claim work and decode the event/history records consumed by runtimes.
func (s *Store) ClaimEvent(ctx context.Context) (*personal.Event, error) {
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

func scanEvent(row scanner) (*personal.Event, error) {
	var event personal.Event
	var taskID, runID, scheduleID, replyOutboxID sql.NullInt64
	var attachments, created string
	if err := row.Scan(&event.ID, &event.Kind, &event.Text, &taskID, &runID, &event.Channel, &event.Account, &event.Sender,
		&event.MessageID, &event.ReplyToID, &attachments, &event.Interrupted, &created, &scheduleID, &replyOutboxID); err != nil {
		return nil, err
	}
	event.TaskID, event.RunID = taskID.Int64, runID.Int64
	event.ScheduleID, event.ReplyOutboxID = scheduleID.Int64, replyOutboxID.Int64
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

const eventColumns = `id,kind,text,task_id,run_id,channel,account,sender,message_id,reply_to_id,attachments,interrupted,created_at,schedule_id,reply_outbox_id`

func (s *Store) History(ctx context.Context, eventID int64, limit int) ([]personal.Message, error) {
	if eventID <= 0 || limit <= 0 {
		return []personal.Message{}, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT h.id,h.event_id,h.role,h.text,e.task_id,e.channel,e.account,e.sender,e.message_id,e.reply_to_id,h.attachments,h.schedule_id
		FROM personal_history h JOIN personal_events e ON e.id=h.event_id
		WHERE h.event_id<? ORDER BY h.event_id DESC,h.id DESC LIMIT ?`, eventID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]personal.Message, 0, limit)
	for rows.Next() {
		var m personal.Message
		var taskID, scheduleID sql.NullInt64
		var attachments string
		if err := rows.Scan(&m.HistoryID, &m.EventID, &m.Role, &m.Text, &taskID, &m.Channel, &m.Account, &m.Sender, &m.MessageID, &m.ReplyToID, &attachments, &scheduleID); err != nil {
			return nil, err
		}
		m.TaskID, m.ScheduleID = taskID.Int64, scheduleID.Int64
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
