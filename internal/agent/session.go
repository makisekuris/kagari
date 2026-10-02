package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"kagari/internal/domain"
	"kagari/internal/logging"
	"kagari/internal/reader"
)

type readRequest struct {
	URL      string `json:"url" jsonschema:"description=要读取的已发现链接"`
	ParentID string `json:"parent_source_id" jsonschema:"description=链接所在的来源 ID"`
	Question string `json:"question" jsonschema:"description=本次追读要回答的问题"`
	Role     string `json:"role" jsonschema:"description=primary 或 evidence 或 context"`
}
type readReply struct {
	Source *domain.Source `json:"source,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// readingSession 保存本次来源图和程序侧预算。typed agent 的工具串行执行，
// 因此计数与 URL 索引在单一会话内更新，不需要跨任务共享锁。
type readingSession struct {
	e                   *Engine
	sources             []domain.Source
	readings            []domain.Reading
	allowed             map[string]bool
	byURL               map[string]domain.Source
	depth               map[string]int
	pages, supplemental int
}

func newSession(e *Engine) *readingSession {
	return &readingSession{e: e, allowed: map[string]bool{}, byURL: map[string]domain.Source{}, depth: map[string]int{}}
}
func (s *readingSession) add(src domain.Source) {
	s.sources = append(s.sources, src)
	s.byURL[src.URL] = src
	s.byURL[src.RequestedURL] = src
}

// read 的 err 表示请求违反阅读策略或基础设施失败；普通页面读取失败会保留为 Source
// 返回，让模型可以选择其他已发现链接，并让归档解释哪些材料没有读到。
func (s *readingSession) read(ctx context.Context, r readRequest) (src domain.Source, err error) {
	started := time.Now()
	cacheHit := false
	if s.e.Log != nil {
		s.e.Log.Info("agent read_source started", zap.String("url", logURL(r.URL)), zap.String("role", r.Role), zap.String("parent_source_id", r.ParentID), zap.String("question", s.e.logContent(r.Question)))
	}
	defer func() {
		if s.e.Log == nil {
			return
		}
		fields := []zap.Field{zap.String("url", logURL(r.URL)), zap.String("role", r.Role), zap.String("source_id", src.ID), zap.String("status", src.Status),
			zap.String("source_reason", logText(src.Reason)), zap.Bool("cache_hit", cacheHit), zap.Bool("truncated", src.Truncated), zap.Int("content_chars", len([]rune(src.Content))),
			zap.Duration("elapsed", time.Since(started)), zap.Int("pages_used", s.pages), zap.Int("pages_limit", s.e.Config.Agent.MaxSources), zap.Int("supplemental_used", s.supplemental), zap.Int("supplemental_limit", s.e.Config.Agent.MaxSupplemental)}
		if err != nil {
			s.e.Log.Warn("agent read_source rejected", append(fields, logging.ErrorFields(err)...)...)
		} else if !usable(src) {
			s.e.Log.Warn("agent read_source failed", fields...)
		} else {
			s.e.Log.Info("agent read_source finished", fields...)
		}
	}()
	u, err := reader.NormalizeURL(r.URL)
	if err != nil {
		return domain.Source{}, err
	}
	depth := 0
	if r.Role != "entry" {
		if r.Role != "primary" && r.Role != "evidence" && r.Role != "context" {
			return domain.Source{}, errors.New("role must be primary, evidence or context")
		}
		if r.Question == "" {
			return domain.Source{}, errors.New("a concrete reading question is required")
		}
		found := false
		// URL 必须真实出现在指定父来源中；模型输出的地址本身不构成抓取授权。
		for _, parent := range s.sources {
			if parent.ID == r.ParentID && usable(parent) {
				for _, link := range parent.Links {
					normalized, _ := reader.NormalizeURL(link.URL)
					if normalized == u {
						found = true
					}
				}
				depth = s.depth[parent.ID] + 1
			}
		}
		if !found {
			return domain.Source{}, errors.New("URL must occur in the specified successfully read parent source")
		}
	} else if !s.allowed[u] {
		return domain.Source{}, errors.New("URL was not submitted")
	}
	if depth > s.e.Config.Agent.MaxDepth {
		return domain.Source{}, errors.New("reading depth budget exhausted")
	}
	if src, ok := s.byURL[u]; ok {
		cacheHit = true
		// 同一页面不重复抓取、不再次计入来源预算，但保留这次引用关系和阅读目的。
		s.readings = append(s.readings, domain.Reading{SourceID: src.ID, ParentID: r.ParentID, URL: u, Question: r.Question, Role: r.Role, Depth: depth})
		return src, nil
	}
	if s.pages >= s.e.Config.Agent.MaxSources {
		return domain.Source{}, errors.New("page budget exhausted")
	}
	if r.Role == "evidence" || r.Role == "context" {
		if s.supplemental >= s.e.Config.Agent.MaxSupplemental {
			return domain.Source{}, errors.New("supplemental reading budget exhausted")
		}
		s.supplemental++
	}
	// 首次阅读包括缓存命中与失败尝试，均占来源预算，避免失败页面引发无限追读。
	s.pages++
	if s.e.CachedSource != nil && s.e.Config.Reader.CacheTTL > 0 {
		cached, cacheErr := s.e.CachedSource(ctx, u)
		if cacheErr != nil {
			return src, fmt.Errorf("source cache: %w", cacheErr)
		}
		if cached != nil && cached.Status == "ok" && time.Since(cached.FetchedAt) < s.e.Config.Reader.CacheTTL {
			src = *cached
			cacheHit = true
		}
	}
	if src.ID == "" {
		src, err = s.e.Read(ctx, u)
	}
	if src.ID == "" {
		src.ID = "s_" + hash(u)[:16]
	}
	if src.RequestedURL == "" {
		src.RequestedURL = u
	}
	if src.URL == "" {
		src.URL = u
	}
	if err != nil {
		// 保留 Reader 给出的 incomplete/restricted 语义，不能把可用片段统一降为 failed。
		if src.Status == "" {
			src.Status = "failed"
		}
		if src.Reason == "" {
			src.Reason = "fetch failed"
		}
	}
	if _, ok := s.byURL[src.URL]; !ok {
		s.add(src)
	} else {
		s.byURL[u] = s.byURL[src.URL]
	}
	s.depth[src.ID] = depth
	s.readings = append(s.readings, domain.Reading{SourceID: src.ID, ParentID: r.ParentID, URL: u, Question: r.Question, Role: r.Role, Depth: depth})
	return src, nil
}
