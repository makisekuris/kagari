package personal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
		AND NOT EXISTS(SELECT 1 FROM personal_outbox p WHERE p.event_id=d.event_id AND p.part<d.part AND p.status<>'sent')
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
	if err := tx.QueryRowContext(ctx, `SELECT d.id,d.event_id,e.task_id,d.text,d.attempts FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id WHERE d.id=?`, id).
		Scan(&out.ID, &out.EventID, &taskID, &out.Text, &out.Attempts); err != nil {
		return nil, err
	}
	out.TaskID = taskID.Int64
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) UpdateDelivery(ctx context.Context, id int64, status string, receipt int64, detail string, retryAt time.Time) error {
	if id <= 0 {
		return errors.New("personal: delivery ID must be positive")
	}
	switch status {
	case "sent":
		if receipt <= 0 {
			return errors.New("personal: sent delivery requires a positive receipt")
		}
	case "pending", "failed", "uncertain":
		if receipt != 0 {
			return errors.New("personal: only sent deliveries may have a receipt")
		}
	default:
		return fmt.Errorf("personal: invalid delivery status %q", status)
	}
	if status == "pending" && retryAt.IsZero() {
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
	var oldStatus string
	var oldReceipt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT status,receipt FROM personal_outbox WHERE id=?`, id).Scan(&oldStatus, &oldReceipt); err != nil {
		return err
	}
	if oldStatus == status {
		if status != "sent" || oldReceipt.Valid && oldReceipt.Int64 == receipt {
			return tx.Commit()
		}
	}
	if oldStatus != "sending" {
		return fmt.Errorf("personal: cannot update delivery %d in state %q", id, oldStatus)
	}
	res, err := tx.ExecContext(ctx, `UPDATE personal_outbox SET status=?,receipt=?,detail=?,next_attempt_at=? WHERE id=? AND status='sending'`,
		status, nullableID(receipt), detail, timestamp(retryAt), id)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return err
		}
		return errors.New("personal: delivery update lost")
	}
	return tx.Commit()
}
