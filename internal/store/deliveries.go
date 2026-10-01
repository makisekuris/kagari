package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"kagari/internal/domain"
)

func (s *Store) ClaimDelivery(ctx context.Context) (*domain.Delivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	// 后续分段必须等前面全部 sent；failed/uncertain 也会阻塞，防止用户看到乱序正文。
	err = tx.QueryRowContext(ctx, `SELECT d.id FROM deliveries d
		WHERE d.status='pending' AND d.next_attempt_at<=?
		AND NOT EXISTS (SELECT 1 FROM deliveries p WHERE p.job_id=d.job_id AND p.part<d.part AND p.status<>'sent')
		ORDER BY d.id LIMIT 1`, timestamp(time.Now())).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET status='sending',attempts=attempts+1 WHERE id=? AND status='pending'`, id); err != nil {
		return nil, err
	}
	delivery, err := scanDelivery(tx.QueryRowContext(ctx, `SELECT id,job_id,chat_id,part,text,status,attempts FROM deliveries WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return delivery, nil
}

func scanDelivery(row interface{ Scan(...any) error }) (*domain.Delivery, error) {
	var delivery domain.Delivery
	err := row.Scan(&delivery.ID, &delivery.JobID, &delivery.ChatID, &delivery.Part, &delivery.Text, &delivery.Status, &delivery.Attempts)
	return &delivery, err
}

func (s *Store) SentDelivery(ctx context.Context, id, messageID int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE deliveries SET status='sent',message_id=?,last_error='' WHERE id=? AND status='sending'`, messageID, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	var status string
	var previous sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT status,message_id FROM deliveries WHERE id=?`, id).Scan(&status, &previous); err != nil {
		return err
	}
	if status == "sent" && previous.Valid && previous.Int64 == messageID {
		return nil
	}
	return fmt.Errorf("store: cannot mark delivery %d sent in state %q", id, status)
}

func (s *Store) FailDelivery(ctx context.Context, id int64, reason string, maxAttempts int, retryAt time.Time, uncertain bool) error {
	if !uncertain && maxAttempts < 1 {
		return errors.New("store: max attempts must be positive")
	}
	state := "pending"
	if uncertain {
		// 网络错误可能发生在 Telegram 已受理之后，不能自动重试造成重复消息。
		state = "uncertain"
	} else {
		var attempts int
		if err := s.db.QueryRowContext(ctx, `SELECT attempts FROM deliveries WHERE id=? AND status='sending'`, id).Scan(&attempts); err != nil {
			return err
		}
		if attempts >= maxAttempts {
			state = "failed"
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE deliveries SET status=?,last_error=?,next_attempt_at=? WHERE id=? AND status='sending'`, state, reason, timestamp(retryAt), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("store: delivery %d is missing or is not sending", id)
	}
	return nil
}

func (s *Store) RetryDeliveries(ctx context.Context, jobID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id=?`, jobID).Scan(&exists); err != nil {
		return err
	}
	// 仅此显式操作重置 uncertain；调用者已接受远端可能实际发送成功而产生重复。
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET status='pending',attempts=0,last_error='',next_attempt_at=? WHERE job_id=? AND status IN ('failed','uncertain')`, timestamp(time.Now()), jobID); err != nil {
		return err
	}
	return tx.Commit()
}
