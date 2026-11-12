package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

// Event ingress validates channel identity, deduplicates incoming messages, and persists them.
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
		kind := personal.EventUser
		role := "user"
		if input.Material {
			kind, role = personal.EventMaterial, "material"
		}
		taskID, scheduleID, replyOutboxID, err := replyAssociation(ctx, tx, input)
		if err != nil {
			return nil, err
		}
		attachmentJSON, err := json.Marshal(attachments)
		if err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO personal_events(kind,text,task_id,schedule_id,reply_outbox_id,channel,account,sender,message_id,reply_to_id,attachments,status,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,'queued',?)`, kind, input.Text, nullableID(taskID), nullableID(scheduleID), nullableID(replyOutboxID), channelName, account, input.Sender,
			input.MessageID, input.ReplyToID, string(attachmentJSON), timestamp(time.Now()))
		if err != nil {
			return nil, err
		}
		ids[i], err = res.LastInsertId()
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_history(event_id,role,text,attachments,schedule_id) VALUES(?,?,?,?,?)`, ids[i], role, input.Text, string(attachmentJSON), nullableID(scheduleID)); err != nil {
			return nil, err
		}
		if err := startReplyTx(ctx, tx, ids[i]); err != nil {
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

func replyAssociation(ctx context.Context, tx *sql.Tx, input channel.Input) (int64, int64, int64, error) {
	if input.ReplyToID == "" {
		return 0, 0, 0, nil
	}
	var taskID, scheduleID sql.NullInt64
	var outboxID int64
	err := tx.QueryRowContext(ctx, `SELECT e.task_id,e.schedule_id,d.id FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id
		WHERE d.receipt_channel=? AND d.receipt_account=? AND d.receipt_conversation=? AND d.receipt_message_id=?
		AND d.status='sent' ORDER BY d.id DESC LIMIT 1`, input.Channel, input.Account, input.Sender, input.ReplyToID).Scan(&taskID, &scheduleID, &outboxID)
	if err == nil {
		return taskID.Int64, scheduleID.Int64, outboxID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, err
	}
	err = tx.QueryRowContext(ctx, `SELECT task_id,schedule_id FROM personal_events WHERE channel=? AND account=? AND sender=? AND message_id=?
		AND kind IN ('user','material') ORDER BY id DESC LIMIT 1`, input.Channel, input.Account, input.Sender, input.ReplyToID).Scan(&taskID, &scheduleID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, nil
	}
	return taskID.Int64, scheduleID.Int64, 0, err
}
