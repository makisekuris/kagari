package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"kagari/internal/browser"
	"kagari/internal/domain"
	"kagari/internal/logging"
	"kagari/internal/reader"
	"kagari/internal/textpage"
)

const (
	httpBackend    = "http"
	browserBackend = "browser"
)

type readRequest struct {
	URL     string `json:"url" jsonschema:"description=要读取的 HTTP(S) URL"`
	Refresh bool   `json:"refresh,omitempty" jsonschema:"description=true 时忽略 HTTP 缓存并重新获取"`
}

// RecordSource persists one collected observation and returns its stable reference.
type RecordSource func(context.Context, string, domain.Source) (domain.SourceRef, error)

type readKey struct {
	URL     string
	Backend string
}

type sourceObservation struct {
	source domain.Source
	ref    domain.SourceRef
}

type sourceCard struct {
	RequestedURL  string        `json:"requested_url,omitempty"`
	Kind          string        `json:"kind,omitempty"`
	Author        string        `json:"author,omitempty"`
	PublishedAt   *time.Time    `json:"published_at,omitempty"`
	Reason        string        `json:"reason,omitempty"`
	ReadMethod    string        `json:"read_method,omitempty"`
	Links         []domain.Link `json:"links,omitempty"`
	LinksOmitted  bool          `json:"links_omitted,omitempty"`
	OmittedFields []string      `json:"omitted_fields,omitempty"`
}

type readReply struct {
	Source domain.SourceRef `json:"source,omitempty"`
	Card   sourceCard       `json:"card,omitempty"`
	Page   textpage.Page    `json:"page,omitempty"`
	Error  string           `json:"error,omitempty"`
}

type sourceRecordError struct{ err error }

func (e *sourceRecordError) Error() string { return "record source: " + e.err.Error() }
func (e *sourceRecordError) Unwrap() error { return e.err }

type captureKeyError struct{ err error }

func (e *captureKeyError) Error() string { return "generate source capture key: " + e.err.Error() }
func (e *captureKeyError) Unwrap() error { return e.err }

func sourceStageFatal(err error) bool {
	var recordErr *sourceRecordError
	var keyErr *captureKeyError
	return errors.As(err, &recordErr) || errors.As(err, &keyErr)
}

func sourceReadReply(ctx context.Context, obs sourceObservation) readReply {
	page, err := textpage.Read(obs.source.Content, textpage.Request{}, false)
	if err != nil {
		return readReply{Error: err.Error()}
	}
	page.SourceTruncated = obs.ref.SourceTruncated
	reply := readReply{Source: obs.ref, Card: sourceCard{RequestedURL: obs.source.RequestedURL, Kind: obs.source.Kind,
		Author: obs.source.Author, PublishedAt: obs.source.PublishedAt, Reason: obs.source.Reason,
		ReadMethod: obs.source.ReadMethod, Links: append([]domain.Link(nil), obs.source.Links...)}, Page: page}
	for _, field := range []struct {
		name  string
		value *string
		limit int
	}{{"title", &reply.Source.Title, 200}, {"author", &reply.Card.Author, 200}, {"reason", &reply.Card.Reason, 800}} {
		runes := []rune(*field.value)
		if len(runes) > field.limit {
			*field.value = string(runes[:field.limit]) + "…"
			reply.Card.OmittedFields = append(reply.Card.OmittedFields, field.name)
		}
	}
	_, err = textpage.EncodeBounded(&reply, textpage.MaxBytes(ctx), func() bool {
		if reply.Page.Shrink() {
			return true
		}
		if len(reply.Card.Links) > 0 {
			reply.Card.Links = nil
			reply.Card.LinksOmitted = true
			return true
		}
		return false
	})
	if err != nil {
		return readReply{Error: "response_item_too_large: source reference exceeds the tool response limit"}
	}
	return reply
}

func newSourceCaptureKey() (string, error) {
	var key [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(key[:]), nil
}

// readingSession keeps HTTP caching and read budgets local to one task stage.
type readingSession struct {
	e          *Engine
	record     RecordSource
	captureKey func() (string, error)
	sources    []domain.Source
	sourceRefs []domain.SourceRef
	readings   []domain.Reading
	byRead     map[readKey]sourceObservation
	attempts   int
	fatalErr   error
}

func newSession(e *Engine, record RecordSource) *readingSession {
	return &readingSession{e: e, record: record, captureKey: newSourceCaptureKey, byRead: make(map[readKey]sourceObservation)}
}

func (s *readingSession) newCaptureKey() (string, error) {
	key, err := s.captureKey()
	if err != nil {
		fatal := &captureKeyError{err: err}
		s.fatalErr = fatal
		return "", fatal
	}
	return key, nil
}

func (s *readingSession) save(ctx context.Context, key string, src domain.Source) (sourceObservation, error) {
	if src.FetchedAt.IsZero() {
		src.FetchedAt = time.Now().UTC()
	}
	var err error
	if s.record == nil {
		err = errors.New("source recorder is unavailable")
	} else {
		var ref domain.SourceRef
		ref, err = s.record(ctx, key, src)
		if err == nil && ref.ID == "" {
			err = errors.New("source recorder returned an empty ID")
		}
		if err == nil {
			src.ID = ref.ID
			observation := sourceObservation{source: src, ref: ref}
			s.sources = append(s.sources, src)
			s.sourceRefs = append(s.sourceRefs, ref)
			return observation, nil
		}
	}
	fatal := &sourceRecordError{err: err}
	s.fatalErr = fatal
	return sourceObservation{}, fatal
}

func (s *readingSession) observe(ctx context.Context, src domain.Source) (sourceObservation, error) {
	key, err := s.newCaptureKey()
	if err != nil {
		return sourceObservation{}, err
	}
	return s.save(ctx, key, src)
}

func (s *readingSession) read(ctx context.Context, rawURL, backend string, refresh bool, read func(context.Context, string) (domain.Source, error)) (obs sourceObservation, err error) {
	started := time.Now()
	cacheHit := false
	if s.e.Log != nil {
		s.e.Log.Info("agent read_url started", zap.String("backend", backend), zap.String("url", logURL(rawURL)))
	}
	defer func() {
		if s.e.Log == nil {
			return
		}
		src := obs.source
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

	if backend != httpBackend && backend != browserBackend {
		return sourceObservation{}, fmt.Errorf("unsupported read backend %q", backend)
	}
	normalizeURL := reader.NormalizeURL
	if backend == browserBackend {
		normalizeURL = browser.NormalizeURL
	}
	u := rawURL
	if backend == httpBackend || rawURL != "" {
		u, err = normalizeURL(rawURL)
		if err != nil {
			return sourceObservation{}, err
		}
	}
	key := readKey{URL: u, Backend: backend}
	if cached, ok := s.byRead[key]; ok && backend == httpBackend && !refresh {
		cacheHit = true
		return cached, nil
	}
	if s.attempts >= s.e.Config.Agent.MaxSources {
		return sourceObservation{}, errors.New("page budget exhausted")
	}
	captureKey, keyErr := s.newCaptureKey()
	if keyErr != nil {
		return sourceObservation{}, keyErr
	}
	s.attempts++
	if read == nil {
		err = errors.New("reader is unavailable")
	} else {
		obs.source, err = read(ctx, u)
	}
	src := obs.source
	if src.RequestedURL == "" {
		src.RequestedURL = u
	}
	if normalized, normalizeErr := normalizeURL(src.URL); normalizeErr == nil {
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
	obs, saveErr := s.save(ctx, captureKey, src)
	if saveErr != nil {
		return sourceObservation{}, saveErr
	}
	if backend == httpBackend {
		s.byRead[key] = obs
	}
	if u == "" {
		u = obs.source.URL
	}
	s.readings = append(s.readings, domain.Reading{SourceID: obs.ref.ID, URL: u, Backend: backend})
	return obs, nil
}
