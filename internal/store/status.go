package store

import (
	"context"
	"fmt"
	"kagari/internal/domain"
)

// StartJob claims a specific CLI task without consuming queued bot submissions.
func (s *Store) StartJob(ctx context.Context, id int64) (*domain.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE jobs SET status='processing',attempts=attempts+1 WHERE id=? AND status='pending'`, id)
	if err != nil {
		return nil, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("job %d is not pending", id)
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return job, tx.Commit()
}

func (s *Store) DeliveryCounts(ctx context.Context, jobID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM deliveries WHERE job_id=? GROUP BY status`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		counts[state] = n
	}
	return counts, rows.Err()
}

type DeliveryStatus struct {
	Target domain.DeliveryTarget
	Status string
	Count  int
}

func (s *Store) DeliveryTargetCounts(ctx context.Context, jobID int64) ([]DeliveryStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel,
		CASE WHEN address='' THEN CAST(chat_id AS TEXT) ELSE address END,status,count(*)
		FROM deliveries WHERE job_id=?
		GROUP BY channel,CASE WHEN address='' THEN CAST(chat_id AS TEXT) ELSE address END,status
		ORDER BY channel,CASE WHEN address='' THEN CAST(chat_id AS TEXT) ELSE address END,status`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var statuses []DeliveryStatus
	for rows.Next() {
		var status DeliveryStatus
		if err := rows.Scan(&status.Target.Channel, &status.Target.Address, &status.Status, &status.Count); err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}
