package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"kagari/internal/domain"
)

func (s *Store) FindResult(ctx context.Context, cacheKey string) (*domain.Result, error) {
	var raw []byte
	// 分析缓存按内容 cache_key 跨任务复用；部分尝试不是可复用结果。
	err := s.db.QueryRowContext(ctx, `SELECT result FROM jobs WHERE kind='analyze' AND status='completed' AND json_extract(payload,'$.cache_key')=? ORDER BY id DESC LIMIT 1`, cacheKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result domain.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *Store) CachedSource(ctx context.Context, url string) (*domain.Source, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT source FROM source_cache WHERE url=?`, url).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var source domain.Source
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, err
	}
	return &source, nil
}

func (s *Store) Entries(ctx context.Context, userID int64, start, end time.Time) ([]domain.ArchiveEntry, error) {
	if !start.Before(end) {
		return []domain.ArchiveEntry{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,payload,result FROM jobs WHERE kind='analyze' AND status='completed' AND json_extract(payload,'$.user_id')=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]domain.ArchiveEntry, 0)
	for rows.Next() {
		var id int64
		var payload, rawResult []byte
		if err := rows.Scan(&id, &payload, &rawResult); err != nil {
			return nil, err
		}
		var submission domain.Submission
		if err := json.Unmarshal(payload, &submission); err != nil {
			return nil, err
		}
		received := submission.ReceivedAt.UTC()
		// 周报用提交收录时间的半开区间；与文章发布日期无关。
		if received.Before(start.UTC()) || !received.Before(end.UTC()) {
			continue
		}
		var result domain.Result
		if err := json.Unmarshal(rawResult, &result); err != nil {
			return nil, err
		}
		entries = append(entries, domain.ArchiveEntry{JobID: id, Submission: submission, Result: result})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Submission.ReceivedAt.Equal(entries[j].Submission.ReceivedAt) {
			return entries[i].JobID < entries[j].JobID
		}
		return entries[i].Submission.ReceivedAt.Before(entries[j].Submission.ReceivedAt)
	})
	seen := make(map[string]bool)
	unique := entries[:0]
	for _, entry := range entries {
		// 周报只在本用户、本周期内去重；它不改变上面的全局分析缓存语义。
		key := entry.Submission.CacheKey
		if key != "" && seen[key] {
			continue
		}
		if key != "" {
			seen[key] = true
		}
		unique = append(unique, entry)
	}
	return unique, nil
}

func (s *Store) Stats(ctx context.Context, userID int64, start, end time.Time) (total, pending, failed int, err error) {
	if !start.Before(end) {
		return 0, 0, 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status,payload FROM jobs WHERE kind='analyze' AND json_extract(payload,'$.user_id')=?`, userID)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var payload []byte
		if err := rows.Scan(&status, &payload); err != nil {
			return 0, 0, 0, err
		}
		var submission domain.Submission
		if err := json.Unmarshal(payload, &submission); err != nil {
			return 0, 0, 0, err
		}
		received := submission.ReceivedAt.UTC()
		if received.Before(start.UTC()) || !received.Before(end.UTC()) {
			continue
		}
		total++
		switch status {
		case "pending", "processing":
			pending++
		case "failed":
			failed++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, err
	}
	return total, pending, failed, nil
}

func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
