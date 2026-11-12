package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func (s *Store) ClaimDelivery(ctx context.Context) (*personal.Outgoing, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := suppressStaleWaitRepliesTx(ctx, tx); err != nil {
		return nil, err
	}
	if err := suppressTerminalDeliveryTailsTx(ctx, tx); err != nil {
		return nil, err
	}
	if err := refreshPendingDeliveryValidityTx(ctx, tx); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT d.id FROM personal_outbox d
		JOIN personal_delivery_intents current_delivery ON current_delivery.id=d.delivery_id
		WHERE d.status='pending' AND d.next_attempt_at<=? AND current_delivery.state<>'suppressed'
		AND NOT EXISTS(SELECT 1 FROM personal_outbox p WHERE p.delivery_id=d.delivery_id AND p.delivery_part<d.delivery_part AND p.status IN ('pending','sending'))
		AND NOT EXISTS(
			SELECT 1 FROM personal_delivery_scopes current_scope
			JOIN personal_delivery_scopes prior_scope ON prior_scope.task_id=current_scope.task_id
			JOIN personal_delivery_intents prior_delivery ON prior_delivery.id=prior_scope.delivery_id
			WHERE current_scope.delivery_id=d.delivery_id AND current_delivery.purpose IN ('result','status','historical')
			AND prior_delivery.id<current_delivery.id AND prior_delivery.purpose IN ('result','status','historical')
			AND (
				prior_delivery.state='pending' OR EXISTS(SELECT 1 FROM personal_outbox p
					WHERE p.delivery_id=prior_delivery.id AND p.status IN ('pending','sending'))
			)
		)
		ORDER BY d.id LIMIT 1`, timestamp(now)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
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
	var out personal.Outgoing
	var taskID sql.NullInt64
	var attachmentID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT d.id,d.delivery_id,d.delivery_part,d.event_id,e.task_id,d.text,d.attachment_id,d.attempts
		FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id WHERE d.id=?`, id).
		Scan(&out.ID, &out.DeliveryID, &out.DeliveryPart, &out.EventID, &taskID, &out.Text, &attachmentID, &out.Attempts); err != nil {
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
	var deliveryID int64
	if err := tx.QueryRowContext(ctx, `SELECT delivery_id FROM personal_outbox WHERE id=?`, id).Scan(&deliveryID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE personal_outbox SET status=?,attempts=%s,receipt_channel=?,receipt_account=?,receipt_conversation=?,receipt_message_id=?,detail=?,next_attempt_at=? WHERE id=? AND status='sending'`, attemptsSQL),
		targetStatus, receipt.Channel, receipt.Account, receipt.Conversation, receipt.MessageID, detail, nextAttemptAt, id)
	if err != nil {
		return err
	}
	if targetStatus == "failed" || targetStatus == "uncertain" {
		reason := deliverySuppressionPartFailed
		if targetStatus == "uncertain" {
			reason = deliverySuppressionPartUncertain
		}
		if err := suppressDeliveryTx(ctx, tx, deliveryID, reason); err != nil {
			return err
		}
	}
	if targetStatus == "pending" {
		if err := suppressStaleWaitRepliesTx(ctx, tx); err != nil {
			return err
		}
		if err := refreshDeliveryIntentValidityTx(ctx, tx, deliveryID); err != nil {
			return err
		}
	}
	actualStatus := targetStatus
	if targetStatus == "pending" {
		if err := tx.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, id).Scan(&actualStatus); err != nil {
			return err
		}
	}
	return commitStoreOutcome(ctx, tx, "delivery.update", "saved", deliveryOutcomePayload{
		Status: actualStatus, Receipt: receipt, Detail: detail, NextAttemptAt: nextAttemptAt,
	}, zap.Int64("delivery_id", id), zap.Int64("event_id", eventID), zap.Int64("task_id", taskID.Int64), zap.String("status", actualStatus))
}

func refreshPendingDeliveryValidityTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT delivery_id FROM personal_outbox WHERE status='pending' ORDER BY delivery_id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var deliveryID int64
		if err := rows.Scan(&deliveryID); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, deliveryID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshDeliveryIntentValidityTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func suppressTerminalDeliveryTailsTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT d.delivery_id,d.status FROM personal_outbox d
		WHERE d.status IN ('failed','uncertain') AND EXISTS(SELECT 1 FROM personal_outbox p
			WHERE p.delivery_id=d.delivery_id AND p.status='pending') ORDER BY d.delivery_id`)
	if err != nil {
		return err
	}
	type terminal struct {
		id     int64
		status string
	}
	var deliveries []terminal
	for rows.Next() {
		var item terminal
		if err := rows.Scan(&item.id, &item.status); err != nil {
			rows.Close()
			return err
		}
		deliveries = append(deliveries, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range deliveries {
		reason := deliverySuppressionPartFailed
		if item.status == "uncertain" {
			reason = deliverySuppressionPartUncertain
		}
		if err := suppressDeliveryTx(ctx, tx, item.id, reason); err != nil {
			return err
		}
	}
	return nil
}
