package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"kagari/internal/domain"
)

var ErrArchiveDeliverySending = errors.New("store: archive delivery is sending")

func (s *Store) ListArchive(ctx context.Context, userID int64, limit, offset int) ([]domain.ArchiveEntry, error) {
	if userID < 0 {
		return nil, errors.New("store: archive user id must be non-negative")
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, errors.New("store: archive limit must be 1..100 and offset non-negative")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,payload,result FROM jobs
		WHERE kind='analyze' AND status='completed'
		AND (json_type(result,'$.kind') IS NULL OR json_extract(result,'$.kind')='analysis')
		AND json_extract(payload,'$.user_id')=?
		ORDER BY id DESC LIMIT ? OFFSET ?`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]domain.ArchiveEntry, 0)
	for rows.Next() {
		var id int64
		var payload, result []byte
		if err := rows.Scan(&id, &payload, &result); err != nil {
			return nil, err
		}
		entry, err := decodeArchiveEntry(id, payload, result)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *Store) ArchiveEntry(ctx context.Context, userID, id int64) (*domain.ArchiveEntry, error) {
	if userID < 0 {
		return nil, errors.New("store: archive user id must be non-negative")
	}
	if id < 1 {
		return nil, errors.New("store: archive job id must be positive")
	}
	var payload, result []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload,result FROM jobs
		WHERE id=? AND kind='analyze' AND status='completed'
		AND (json_type(result,'$.kind') IS NULL OR json_extract(result,'$.kind')='analysis')
		AND json_extract(payload,'$.user_id')=?`, id, userID).Scan(&payload, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeArchiveEntry(id, payload, result)
}

// DeleteArchiveEntry removes one completed analyze archive and its deliveries.
// CLI callers must hold the processor lock; a claimed delivery blocks deletion.
func (s *Store) DeleteArchiveEntry(ctx context.Context, userID, id int64) (map[string]int64, error) {
	if userID < 0 {
		return nil, errors.New("store: archive user id must be non-negative")
	}
	if id < 1 {
		return nil, errors.New("store: archive job id must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM jobs
		WHERE id=? AND kind='analyze' AND status='completed'
		AND (json_type(result,'$.kind') IS NULL OR json_extract(result,'$.kind')='analysis')
		AND json_extract(payload,'$.user_id')=?`, id, userID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: archive entry %d is missing or unavailable", id)
	}
	if err != nil {
		return nil, err
	}
	var sending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deliveries WHERE job_id=? AND status='sending')`, id).Scan(&sending); err != nil {
		return nil, err
	}
	if sending {
		return nil, ErrArchiveDeliverySending
	}
	deliveryResult, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE job_id=?`, id)
	if err != nil {
		return nil, err
	}
	deliveries, err := deliveryResult.RowsAffected()
	if err != nil {
		return nil, err
	}
	jobResult, err := tx.ExecContext(ctx, `DELETE FROM jobs
		WHERE id=? AND kind='analyze' AND status='completed'
		AND (json_type(result,'$.kind') IS NULL OR json_extract(result,'$.kind')='analysis')
		AND json_extract(payload,'$.user_id')=?`, id, userID)
	if err != nil {
		return nil, err
	}
	jobs, err := jobResult.RowsAffected()
	if err != nil {
		return nil, err
	}
	if jobs != 1 {
		return nil, fmt.Errorf("store: archive entry %d changed during deletion", id)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]int64{"jobs": jobs, "deliveries": deliveries}, nil
}

func decodeArchiveEntry(id int64, payload, rawResult []byte) (*domain.ArchiveEntry, error) {
	var entry domain.ArchiveEntry
	entry.JobID = id
	if err := json.Unmarshal(payload, &entry.Submission); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rawResult, &entry.Result); err != nil {
		return nil, err
	}
	return &entry, nil
}
