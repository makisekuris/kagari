package personal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"kagari/internal/channel"
)

func (s *Store) ClaimDelivery(ctx context.Context) (*Outgoing, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT d.id FROM personal_outbox d
		WHERE d.status='pending' AND d.next_attempt_at<=?
		AND NOT EXISTS(SELECT 1 FROM personal_outbox p WHERE p.event_id=d.event_id AND p.part<d.part AND p.status IN ('pending','sending'))
		ORDER BY d.id LIMIT 1`, timestamp(now)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE personal_outbox SET status='sending',attempts=attempts+1,next_attempt_at=? WHERE id=? AND status='pending'`, timestamp(now), id)
	if err != nil {
		return nil, err
	}
	if affected, err := res.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("personal: delivery claim lost")
	}
	var out Outgoing
	var taskID sql.NullInt64
	var attachmentID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT d.id,d.event_id,e.task_id,d.text,d.attachment_id,d.attempts
		FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id WHERE d.id=?`, id).
		Scan(&out.ID, &out.EventID, &taskID, &out.Text, &attachmentID, &out.Attempts); err != nil {
		return nil, err
	}
	out.TaskID, out.AttachmentID = taskID.Int64, attachmentID.String
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) UpdateDelivery(ctx context.Context, id int64, status string, receipt channel.Receipt, detail string, retryAt time.Time) error {
	if id <= 0 {
		return errors.New("personal: delivery ID must be positive")
	}
	targetStatus := status
	refundAttempt := false
	if status == "waiting" {
		targetStatus, refundAttempt = "pending", true
	}
	switch targetStatus {
	case "sent", "pending", "failed", "uncertain":
	default:
		return fmt.Errorf("personal: invalid delivery status %q", status)
	}
	if targetStatus != "sent" && receipt.MessageID != "" {
		return errors.New("personal: only sent deliveries may have a remote message ID")
	}
	if targetStatus == "pending" && retryAt.IsZero() {
		return errors.New("personal: pending delivery requires a retry time")
	}
	if retryAt.IsZero() {
		retryAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldStatus, oldDetail, oldNext string
	var oldReceipt channel.Receipt
	var eventID int64
	var taskID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT d.status,d.receipt_channel,d.receipt_account,d.receipt_conversation,d.receipt_message_id,
		d.detail,d.next_attempt_at,d.event_id,e.task_id FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id WHERE d.id=?`, id).
		Scan(&oldStatus, &oldReceipt.Channel, &oldReceipt.Account, &oldReceipt.Conversation, &oldReceipt.MessageID,
			&oldDetail, &oldNext, &eventID, &taskID); err != nil {
		return err
	}
	if oldStatus == targetStatus {
		if targetStatus != "sent" || oldReceipt == receipt {
			payload := deliveryOutcomePayload{Status: oldStatus, Receipt: oldReceipt, Detail: oldDetail, NextAttemptAt: oldNext}
			return commitStoreOutcome(ctx, tx, "delivery.update", "unchanged", payload,
				zap.Int64("delivery_id", id), zap.Int64("event_id", eventID), zap.Int64("task_id", taskID.Int64), zap.String("status", oldStatus))
		}
	}
	if oldStatus != "sending" {
		return fmt.Errorf("personal: cannot update delivery %d in state %q", id, oldStatus)
	}
	nextAttemptAt := timestamp(retryAt)
	attemptsSQL := "attempts"
	if refundAttempt {
		attemptsSQL = "MAX(0,attempts-1)"
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE personal_outbox SET status=?,attempts=%s,receipt_channel=?,receipt_account=?,receipt_conversation=?,receipt_message_id=?,detail=?,next_attempt_at=? WHERE id=? AND status='sending'`, attemptsSQL),
		targetStatus, receipt.Channel, receipt.Account, receipt.Conversation, receipt.MessageID, detail, nextAttemptAt, id)
	if err != nil {
		return err
	}
	return commitStoreOutcome(ctx, tx, "delivery.update", "saved", deliveryOutcomePayload{
		Status: targetStatus, Receipt: receipt, Detail: detail, NextAttemptAt: nextAttemptAt,
	}, zap.Int64("delivery_id", id), zap.Int64("event_id", eventID), zap.Int64("task_id", taskID.Int64), zap.String("status", targetStatus))
}
