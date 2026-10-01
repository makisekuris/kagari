package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"kagari/internal/domain"
)

func (s *Store) Enqueue(ctx context.Context, kind, key string, payload []byte, targetChatID int64) (int64, bool, error) {
	if kind == "" || key == "" {
		return 0, false, errors.New("store: job kind and key are required")
	}
	if err := validJSON(payload); err != nil {
		return 0, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	id, created, err := enqueue(ctx, tx, kind, key, payload, targetChatID, time.Now().UTC())
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, created, nil
}

func (s *Store) AcceptUpdate(ctx context.Context, updateID int64, kind, key string, payload []byte, targetChatID int64) (int64, bool, error) {
	if updateID < 0 {
		return 0, false, errors.New("store: update id must be non-negative")
	}
	if kind != "" {
		if key == "" {
			return 0, false, errors.New("store: job key is required")
		}
		if err := validJSON(payload); err != nil {
			return 0, false, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var offset int64
	if err := tx.QueryRowContext(ctx, `SELECT next_offset FROM update_state WHERE id=1`).Scan(&offset); err != nil {
		return 0, false, err
	}
	if updateID < offset {
		if err := tx.Commit(); err != nil {
			return 0, false, err
		}
		return 0, false, nil
	}
	// 入队和 offset 必须同事务提交，避免崩溃后丢更新；kind 为空的忽略项也推进 offset，避免轮询卡住。
	var id int64
	var created bool
	if kind != "" {
		id, created, err = enqueue(ctx, tx, kind, key, payload, targetChatID, time.Now().UTC())
		if err != nil {
			return 0, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE update_state SET next_offset=? WHERE id=1`, updateID+1); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, created, nil
}

func (s *Store) Offset(ctx context.Context) (int64, error) {
	var offset int64
	err := s.db.QueryRowContext(ctx, `SELECT next_offset FROM update_state WHERE id=1`).Scan(&offset)
	return offset, err
}

func (s *Store) Recover(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 未完成的处理可安全重做；sending 的远端结果未知，只能标 uncertain 交给显式人工重发。
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='pending' WHERE status='processing'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET status='uncertain',last_error='outcome unknown after restart' WHERE status='sending'`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimJob(ctx context.Context) (*domain.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE status='pending' AND next_attempt_at<=? ORDER BY id LIMIT 1`, timestamp(time.Now())).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='processing',attempts=attempts+1 WHERE id=? AND status='pending'`, id); err != nil {
		return nil, err
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Store) SaveAttempt(ctx context.Context, id int64, result []byte) error {
	if err := validJSON(result); err != nil {
		return err
	}
	updated, err := s.db.ExecContext(ctx, `UPDATE jobs SET result=? WHERE id=? AND status='processing'`, result, id)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("store: job %d is missing or is not processing", id)
	}
	return nil
}

func (s *Store) CompleteJob(ctx context.Context, id int64, result []byte, messages []string, sources []domain.Source) error {
	if err := validJSON(result); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var chatID int64
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT target_chat_id,status FROM jobs WHERE id=?`, id).Scan(&chatID, &status); err != nil {
		return err
	}
	if status != "processing" {
		return fmt.Errorf("store: cannot complete job %d in state %q", id, status)
	}
	// 结果、分段 outbox 和成功来源缓存一起提交，避免出现“任务完成但消息/证据丢失”。
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='completed',result=?,last_error='' WHERE id=? AND status='processing'`, result, id); err != nil {
		return err
	}
	now := timestamp(time.Now())
	for i, message := range messages {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(job_id,chat_id,part,text,status,next_attempt_at) VALUES(?,?,?,?, 'pending', ?)`, id, chatID, i+1, message, now); err != nil {
			return err
		}
	}
	for _, source := range sources {
		if source.Status != "ok" {
			continue
		}
		data, err := json.Marshal(source)
		if err != nil {
			return err
		}
		for _, url := range sourceCacheURLs(source) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO source_cache(url,source,cached_at) VALUES(?,?,?) ON CONFLICT(url) DO UPDATE SET source=excluded.source,cached_at=excluded.cached_at`, url, data, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func sourceCacheURLs(source domain.Source) []string {
	urls := make([]string, 0, 2)
	if source.RequestedURL != "" {
		urls = append(urls, source.RequestedURL)
	}
	if source.URL != "" && source.URL != source.RequestedURL {
		urls = append(urls, source.URL)
	}
	return urls
}

func (s *Store) FailJob(ctx context.Context, id int64, reason string, maxAttempts int, retryAt time.Time) error {
	if maxAttempts < 1 {
		return errors.New("store: max attempts must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts int
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT attempts,status FROM jobs WHERE id=?`, id).Scan(&attempts, &status); err != nil {
		return err
	}
	if status != "processing" {
		return fmt.Errorf("store: cannot fail job %d in state %q", id, status)
	}
	state := "pending"
	if attempts >= maxAttempts {
		state = "failed"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status=?,last_error=?,next_attempt_at=? WHERE id=?`, state, reason, timestamp(retryAt), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Job(ctx context.Context, id int64) (*domain.Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return job, err
}

func (s *Store) RetryJob(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='pending',attempts=0,last_error='',next_attempt_at=? WHERE id=? AND status='failed'`, timestamp(time.Now()), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("store: job %d is missing or is not failed", id)
	}
	return nil
}
