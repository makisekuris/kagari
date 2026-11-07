package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/textpage"
)

func TestReadingSessionCachesHTTPAndEnforcesTaskBudget(t *testing.T) {
	cfg := config.Config{Agent: config.Agent{MaxSources: 3}}
	e := &Engine{Config: cfg}
	s := newSession(e, memorySourceRecorder)
	var httpCalls int
	failedHTTP := func(_ context.Context, url string) (domain.Source, error) {
		httpCalls++
		return domain.Source{URL: url, Status: "restricted", Reason: "HTTP 403"}, errors.New("forbidden")
	}
	first, err := s.read(context.Background(), "https://example.org/a", httpBackend, false, failedHTTP)
	if err != nil || first.source.Status != "restricted" || first.ref.ID == "" {
		t.Fatalf("failed read should be a persisted observation: %+v %v", first, err)
	}
	cached, err := s.read(context.Background(), "https://example.org/a", httpBackend, false, func(context.Context, string) (domain.Source, error) {
		t.Fatal("same-backend cache called reader twice")
		return domain.Source{}, nil
	})
	if err != nil || cached.ref.ID != first.ref.ID || httpCalls != 1 || len(s.readings) != 1 || s.attempts != 1 {
		t.Fatalf("same-backend retry was not cached: %+v readings=%+v attempts=%d err=%v", cached, s.readings, s.attempts, err)
	}

	browserObservation, err := s.read(context.Background(), "https://example.org/a", browserBackend, false, func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{URL: url, Status: "ok", Content: "browser read"}, nil
	})
	if err != nil || !browserObservation.source.Usable() || browserObservation.ref.ID == first.ref.ID {
		t.Fatalf("alternate backend did not produce a distinct source: %+v %v", browserObservation, err)
	}
	if _, err := s.read(context.Background(), "https://different.example/b", httpBackend, false, func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{URL: url, Status: "ok", Content: "another page"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(context.Background(), "https://example.org/c", httpBackend, false, failedHTTP); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("task budget did not stop the fourth backend attempt: %v", err)
	}
	if _, err := s.read(context.Background(), "file:///etc/passwd", httpBackend, false, failedHTTP); err == nil {
		t.Fatal("non-HTTP URL was accepted")
	}
	if len(s.readings) != 3 || s.attempts != 3 {
		t.Fatalf("attempt accounting: readings=%+v attempts=%d", s.readings, s.attempts)
	}
	if s.readings[0].Backend != httpBackend || s.readings[1].Backend != browserBackend {
		t.Fatalf("backend attempts missing: %+v", s.readings)
	}
}

func TestBrowserObservationsAreFreshAndUseSharedBudget(t *testing.T) {
	s := newSession(&Engine{Config: config.Config{Agent: config.Agent{MaxSources: 3}}}, memorySourceRecorder)
	calls := 0
	read := func(_ context.Context, _ string) (domain.Source, error) {
		calls++
		return domain.Source{URL: "https://example.org/page", Status: "ok", Content: strings.Repeat("changed ", calls)}, nil
	}
	for _, url := range []string{"https://example.org/page", "https://example.org/page", ""} {
		obs, err := s.read(context.Background(), url, browserBackend, false, read)
		if err != nil || len(obs.source.Content) != len("changed ")*calls || len(s.readings) != calls {
			t.Fatalf("browser observation reused: source=%+v calls=%d readings=%+v err=%v", obs, calls, s.readings, err)
		}
	}
	if s.sourceRefs[0].ID == s.sourceRefs[1].ID || s.sourceRefs[0].ID == s.sourceRefs[2].ID || s.sourceRefs[1].ID == s.sourceRefs[2].ID {
		t.Fatalf("browser observations reused IDs: %+v", s.sourceRefs)
	}
	if calls != 3 || len(s.byRead) != 0 || s.readings[2].URL != "https://example.org/page" {
		t.Fatalf("browser cache/URL accounting: calls=%d cache=%v readings=%+v", calls, s.byRead, s.readings)
	}
	if _, err := s.read(context.Background(), "", browserBackend, false, read); err == nil || calls != 3 {
		t.Fatalf("browser exceeded budget: calls=%d err=%v", calls, err)
	}
}

func TestHTTPRefreshCreatesNewObservationAndReplacesCache(t *testing.T) {
	var keys []string
	record := func(ctx context.Context, key string, src domain.Source) (domain.SourceRef, error) {
		keys = append(keys, key)
		return memorySourceRecorder(ctx, key, src)
	}
	s := newSession(&Engine{Config: config.Config{Agent: config.Agent{MaxSources: 4}}}, record)
	calls := 0
	read := func(_ context.Context, url string) (domain.Source, error) {
		calls++
		return domain.Source{URL: url, Status: "ok", Content: "revision " + string(rune('0'+calls))}, nil
	}
	first, err := s.read(context.Background(), "https://example.org/page", httpBackend, false, read)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := s.read(context.Background(), "https://example.org/page", httpBackend, false, read)
	if err != nil || cached.ref.ID != first.ref.ID || calls != 1 {
		t.Fatalf("cache did not return saved observation: first=%+v cached=%+v calls=%d err=%v", first, cached, calls, err)
	}
	refreshed, err := s.read(context.Background(), "https://example.org/page", httpBackend, true, read)
	if err != nil || refreshed.ref.ID == first.ref.ID || refreshed.source.Content != "revision 2" || calls != 2 {
		t.Fatalf("refresh did not create a new observation: refreshed=%+v calls=%d err=%v", refreshed, calls, err)
	}
	latest, err := s.read(context.Background(), "https://example.org/page", httpBackend, false, read)
	if err != nil || latest.ref.ID != refreshed.ref.ID || calls != 2 || len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("refresh did not replace saved cache: latest=%+v calls=%d keys=%v err=%v", latest, calls, keys, err)
	}
}

func TestCaptureKeyAndPersistenceFailuresAreStageFatal(t *testing.T) {
	s := newSession(&Engine{Config: config.Config{Agent: config.Agent{MaxSources: 1}}}, memorySourceRecorder)
	s.captureKey = func() (string, error) { return "", errors.New("random source unavailable") }
	readerCalled := false
	_, err := s.read(context.Background(), "https://example.org/page", httpBackend, false, func(context.Context, string) (domain.Source, error) {
		readerCalled = true
		return domain.Source{}, nil
	})
	if !sourceStageFatal(err) || readerCalled || s.attempts != 0 {
		t.Fatalf("capture-key failure did not stop before collection: err=%v called=%t attempts=%d", err, readerCalled, s.attempts)
	}

	s = newSession(&Engine{Config: config.Config{Agent: config.Agent{MaxSources: 1}}}, func(context.Context, string, domain.Source) (domain.SourceRef, error) {
		return domain.SourceRef{}, errors.New("disk full")
	})
	_, err = s.read(context.Background(), "https://example.org/page", httpBackend, false, func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{URL: url, Status: "ok", Content: "collected"}, nil
	})
	if !sourceStageFatal(err) || !strings.Contains(err.Error(), "disk full") || len(s.sources) != 0 || len(s.byRead) != 0 {
		t.Fatalf("persistence failure was hidden or cached: err=%v sources=%+v cache=%v", err, s.sources, s.byRead)
	}

	s = newSession(&Engine{Config: config.Config{Agent: config.Agent{MaxSources: 1}}}, func(context.Context, string, domain.Source) (domain.SourceRef, error) {
		return domain.SourceRef{}, nil
	})
	_, err = s.read(context.Background(), "https://example.org/page", httpBackend, false, func(_ context.Context, url string) (domain.Source, error) {
		return domain.Source{URL: url, Status: "ok", Content: "collected"}, nil
	})
	if !sourceStageFatal(err) || !strings.Contains(err.Error(), "empty ID") || len(s.sources) != 0 || len(s.byRead) != 0 {
		t.Fatalf("empty source ID was not rejected: err=%v sources=%+v cache=%v", err, s.sources, s.byRead)
	}
}

func TestSourceReplyFitsBoundAndIncludesPageRange(t *testing.T) {
	content := strings.Repeat("<", 8000)
	src := domain.Source{URL: "https://example.org/article", Title: "article", Status: "incomplete", Content: content, Truncated: true,
		Links: []domain.Link{{URL: "https://example.org/related", Text: strings.Repeat("related", 12000)}}}
	ref := memorySourceRef("capture", src)
	reply := sourceReadReply(context.Background(), sourceObservation{source: src, ref: ref})
	encoded, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 32<<10 || reply.Source.ID != ref.ID || reply.Page.Offset != 0 || reply.Page.End <= 0 || reply.Page.TotalRunes != len([]rune(content)) ||
		!reply.Page.HasMore || !reply.Source.SourceTruncated || !reply.Page.SourceTruncated || !reply.Card.LinksOmitted || reply.Page.End >= textpage.DefaultTextLimit {
		t.Fatalf("source reply is not a bounded first page: bytes=%d reply=%+v", len(encoded), reply)
	}
}

func memorySourceRef(captureKey string, src domain.Source) domain.SourceRef {
	return domain.SourceRef{ID: "m_" + captureKey, RunID: 1, URL: src.URL, Title: src.Title, Status: src.Status,
		FetchedAt: src.FetchedAt, SourceTruncated: src.Truncated, TotalRunes: len([]rune(src.Content))}
}
