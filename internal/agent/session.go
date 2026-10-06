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

const (
	httpBackend    = "http"
	browserBackend = "browser"
)

type readRequest struct {
	URL string `json:"url" jsonschema:"description=要读取的 HTTP(S) URL"`
}

type readReply struct {
	Source *domain.Source `json:"source,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type readKey struct {
	URL     string
	Backend string
}

// readingSession keeps per-task caching and read budgets local to one analysis.
type readingSession struct {
	e        *Engine
	sources  []domain.Source
	readings []domain.Reading

	byRead   map[readKey]domain.Source
	seenIDs  map[string]bool
	attempts int
}

func newSession(e *Engine) *readingSession {
	return &readingSession{e: e, byRead: make(map[readKey]domain.Source), seenIDs: make(map[string]bool)}
}

func (s *readingSession) add(src domain.Source) {
	s.sources = append(s.sources, src)
	s.seenIDs[src.ID] = true
}

func (s *readingSession) read(ctx context.Context, rawURL, backend string, read func(context.Context, string) (domain.Source, error)) (src domain.Source, err error) {
	started := time.Now()
	cacheHit := false
	if s.e.Log != nil {
		s.e.Log.Info("agent read_url started", zap.String("backend", backend), zap.String("url", logURL(rawURL)))
	}
	defer func() {
		if s.e.Log == nil {
			return
		}
		fields := []zap.Field{zap.String("backend", backend), zap.String("url", logURL(rawURL)), zap.String("source_id", src.ID), zap.String("status", src.Status),
			zap.String("source_reason", logText(src.Reason)), zap.Bool("cache_hit", cacheHit), zap.Bool("truncated", src.Truncated), zap.Int("content_chars", len([]rune(src.Content))),
			zap.Int("read_attempts", s.attempts), zap.Int("read_limit", s.e.Config.Agent.MaxSources), zap.Duration("elapsed", time.Since(started))}
		if err != nil {
			s.e.Log.Warn("agent read_url rejected", append(fields, logging.ErrorFields(err)...)...)
		} else if !src.Usable() {
			s.e.Log.Warn("agent read_url failed", fields...)
		} else {
			s.e.Log.Info("agent read_url finished", fields...)
		}
	}()

	u, err := reader.NormalizeURL(rawURL)
	if err != nil {
		return domain.Source{}, err
	}
	if backend != httpBackend && backend != browserBackend {
		return domain.Source{}, fmt.Errorf("unsupported read backend %q", backend)
	}
	key := readKey{URL: u, Backend: backend}
	if cached, ok := s.byRead[key]; ok {
		cacheHit = true
		return cached, nil
	}
	if s.attempts >= s.e.Config.Agent.MaxSources {
		return domain.Source{}, errors.New("page budget exhausted")
	}
	s.attempts++

	if backend == httpBackend && s.e.CachedSource != nil && s.e.Config.Reader.CacheTTL > 0 {
		cached, cacheErr := s.e.CachedSource(ctx, u)
		if cacheErr == nil && cached != nil && cached.Status == "ok" && time.Since(cached.FetchedAt) < s.e.Config.Reader.CacheTTL {
			src = *cached
			cacheHit = true
		}
	}
	if src.ID == "" {
		if read == nil {
			err = errors.New("reader is unavailable")
		} else {
			src, err = read(ctx, u)
		}
	}
	if src.ID == "" {
		src.ID = "s_" + hash(u + backend)[:16]
	}
	if src.RequestedURL == "" {
		src.RequestedURL = u
	}
	if normalized, normalizeErr := reader.NormalizeURL(src.URL); normalizeErr == nil {
		src.URL = normalized
	} else {
		src.URL = u
		if src.Status == "ok" {
			src.Status = "restricted"
			src.Reason = "reader returned an invalid final URL"
		}
	}
	if err != nil {
		if src.Status == "" {
			src.Status = "failed"
		}
		if src.Reason == "" {
			src.Reason = "read failed"
		}
	}
	for suffix := 1; s.seenIDs[src.ID]; suffix++ {
		src.ID = fmt.Sprintf("%s_%d", src.ID, suffix)
	}
	s.byRead[key] = src
	s.add(src)
	s.readings = append(s.readings, domain.Reading{SourceID: src.ID, URL: u, Backend: backend})
	return src, nil
}
