package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kagari/internal/config"
	"kagari/internal/domain"
)

func TestReadingSessionCachesPerBackendAndEnforcesTaskBudget(t *testing.T) {
	cfg := config.Config{Agent: config.Agent{MaxSources: 3}}
	e := &Engine{Config: cfg}
	s := newSession(e)
	var httpCalls int
	failedHTTP := func(_ context.Context, url string) (domain.Source, error) {
		httpCalls++
		return domain.Source{ID: "page", URL: url, Status: "restricted", Reason: "HTTP 403"}, errors.New("forbidden")
	}
	first, err := s.read(context.Background(), "https://example.org/a", httpBackend, failedHTTP)
	if err != nil || first.Status != "restricted" {
		t.Fatalf("failed read should be returned as an observation: %+v %v", first, err)
	}
	cached, err := s.read(context.Background(), "https://example.org/a", httpBackend, func(context.Context, string) (domain.Source, error) {
		t.Fatal("same-backend cache called reader twice")
		return domain.Source{}, nil
	})
	if err != nil || cached.ID != first.ID || httpCalls != 1 || len(s.readings) != 1 || s.attempts != 1 {
		t.Fatalf("same-backend retry was not cached: %+v readings=%+v attempts=%d err=%v", cached, s.readings, s.attempts, err)
	}

	browser, err := s.read(context.Background(), "https://example.org/a", browserBackend, func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{ID: "page", URL: url, Status: "ok", Content: "browser read"}, nil
	})
	if err != nil || !browser.Usable() || browser.ID == first.ID {
		t.Fatalf("alternate backend did not produce a distinct source: %+v %v", browser, err)
	}
	if _, err := s.read(context.Background(), "https://different.example/b", httpBackend, func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{ID: "b", URL: url, Status: "ok", Content: "another page"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(context.Background(), "https://example.org/c", httpBackend, failedHTTP); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("task budget did not stop the fourth backend attempt: %v", err)
	}
	if _, err := s.read(context.Background(), "file:///etc/passwd", httpBackend, failedHTTP); err == nil {
		t.Fatal("non-HTTP URL was accepted")
	}
	if len(s.readings) != 3 || s.attempts != 3 {
		t.Fatalf("attempt accounting: readings=%+v attempts=%d", s.readings, s.attempts)
	}
	if s.readings[0].Backend != httpBackend || s.readings[1].Backend != browserBackend {
		t.Fatalf("backend attempts missing: %+v", s.readings)
	}
}
