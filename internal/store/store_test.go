package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/domain"
)

func testStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func submission(userID int64, key string, received time.Time) domain.Submission {
	return domain.Submission{UserID: userID, ChatID: userID + 100, Text: key, ReceivedAt: received, CacheKey: key}
}

func completeSubmission(t *testing.T, s *Store, sub domain.Submission) int64 {
	t.Helper()
	id, created, err := s.Enqueue(context.Background(), "analyze", sub.Text, mustJSON(t, sub), sub.ChatID)
	if err != nil || !created {
		t.Fatalf("Enqueue() = (%d, %v, %v)", id, created, err)
	}
	job, err := s.ClaimJob(context.Background())
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("ClaimJob() = (%+v, %v), want job %d", job, err, id)
	}
	if err := s.CompleteJob(context.Background(), id, mustJSON(t, domain.Result{Analysis: domain.Analysis{Title: sub.Text}}), nil, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRestartAndPrivateDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "jobs.sqlite")
	s := testStore(t, path)
	id, created, err := s.Enqueue(context.Background(), "analyze", "one", []byte(`{"cache_key":"one"}`), 7)
	if err != nil || !created {
		t.Fatalf("Enqueue() = (%d, %v, %v)", id, created, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	job, err := s2.Job(context.Background(), id)
	if err != nil || job == nil || job.Key != "one" {
		t.Fatalf("Job() = (%+v, %v)", job, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0077 != 0 {
		t.Fatalf("database permissions = %o, want no group/other access", got)
	}
}

func TestAcceptUpdateEnqueueOffsetAreAtomicAndDeduplicated(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER stop_enqueue BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptUpdate(ctx, 20, "analyze", "failed", []byte(`{"cache_key":"failed"}`), 1); err == nil {
		t.Fatal("AcceptUpdate() succeeded despite rejecting insert trigger")
	}
	if offset, err := s.Offset(ctx); err != nil || offset != 0 {
		t.Fatalf("Offset() = (%d, %v), want 0", offset, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER stop_enqueue`); err != nil {
		t.Fatal(err)
	}
	id, created, err := s.AcceptUpdate(ctx, 20, "analyze", "same", []byte(`{"cache_key":"same"}`), 1)
	if err != nil || !created || id == 0 {
		t.Fatalf("AcceptUpdate() = (%d, %v, %v)", id, created, err)
	}
	if gotID, gotCreated, err := s.AcceptUpdate(ctx, 20, "analyze", "same", []byte(`{"cache_key":"same"}`), 1); err != nil || gotCreated || gotID != 0 {
		t.Fatalf("duplicate update = (%d, %v, %v), want skipped", gotID, gotCreated, err)
	}
	gotID, gotCreated, err := s.AcceptUpdate(ctx, 21, "analyze", "same", []byte(`{"cache_key":"same"}`), 1)
	if err != nil || gotCreated || gotID != id {
		t.Fatalf("idempotent job = (%d, %v, %v), want (%d,false,nil)", gotID, gotCreated, err, id)
	}
	if _, created, err := s.AcceptUpdate(ctx, 22, "", "", nil, 1); err != nil || created {
		t.Fatalf("ignored update = (created %v, %v)", created, err)
	}
	if offset, err := s.Offset(ctx); err != nil || offset != 23 {
		t.Fatalf("Offset() = (%d, %v), want 23", offset, err)
	}
}

func TestRetryRecoveryAndOrderedUncertainDeliveries(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	id, _, err := s.Enqueue(ctx, "analyze", "retry", []byte(`{"cache_key":"retry"}`), 42)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimJob(ctx)
	if err != nil || job == nil || job.Attempts != 1 {
		t.Fatalf("ClaimJob() = (%+v, %v)", job, err)
	}
	partial := mustJSON(t, domain.Result{Sources: []domain.Source{{URL: "https://example.com", Status: "ok"}}})
	if err := s.FailJob(ctx, id, partial, "temporary", 1, time.Now().Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	job, err = s.Job(ctx, id)
	if err != nil || job.Status != "failed" || string(job.Result) != string(partial) {
		t.Fatalf("failed job did not preserve partial result: %+v, %v", job, err)
	}
	if err := s.RetryJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	job, err = s.ClaimJob(ctx)
	if err != nil || job == nil || job.Attempts != 1 {
		t.Fatalf("retried ClaimJob() = (%+v, %v)", job, err)
	}
	sources := []domain.Source{
		{RequestedURL: "https://example.com/start", URL: "https://example.com/final", Status: "ok", Title: "cached"},
		{URL: "https://example.com/failure", Status: "error", Title: "not cached"},
	}
	if err := s.CompleteJob(ctx, id, mustJSON(t, domain.Result{Analysis: domain.Analysis{Title: "done"}}), []string{"first", "second"}, sources); err != nil {
		t.Fatal(err)
	}
	cached, err := s.CachedSource(ctx, "https://example.com/start")
	if err != nil || cached == nil || cached.Title != "cached" {
		t.Fatalf("CachedSource() = (%+v, %v)", cached, err)
	}
	if cached, err := s.CachedSource(ctx, "https://example.com/failure"); err != nil || cached != nil {
		t.Fatalf("failed source cache = (%+v, %v), want nil", cached, err)
	}
	first, err := s.ClaimDelivery(ctx)
	if err != nil || first == nil || first.Part != 1 {
		t.Fatalf("ClaimDelivery() = (%+v, %v), want part 1", first, err)
	}
	if err := s.FailDelivery(ctx, first.ID, "unknown outcome", 3, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
		t.Fatalf("later part overtook uncertain first part: (%+v, %v)", next, err)
	}
	if err := s.RetryDeliveries(ctx, id); err != nil {
		t.Fatal(err)
	}
	first, err = s.ClaimDelivery(ctx)
	if err != nil || first == nil || first.Part != 1 {
		t.Fatalf("retried delivery = (%+v, %v), want part 1", first, err)
	}
	if err := s.SentDelivery(ctx, first.ID, 1001); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimDelivery(ctx)
	if err != nil || second == nil || second.Part != 2 {
		t.Fatalf("second delivery = (%+v, %v), want part 2", second, err)
	}
	if err := s.FailDelivery(ctx, second.ID, "temporary", 1, time.Now().Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
		t.Fatalf("failed delivery was unexpectedly claimable: (%+v, %v)", next, err)
	}
	if err := s.RetryDeliveries(ctx, id); err != nil {
		t.Fatal(err)
	}
	second, err = s.ClaimDelivery(ctx)
	if err != nil || second == nil || second.Part != 2 {
		t.Fatalf("manually retried delivery = (%+v, %v)", second, err)
	}
}

func TestRecoverMarksSendingDeliveryUncertain(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	id, _, err := s.Enqueue(ctx, "analyze", "recover", []byte(`{"cache_key":"recover"}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteJob(ctx, id, []byte(`{}`), []string{"message"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	activeID, _, err := s.Enqueue(ctx, "analyze", "active", []byte(`{"cache_key":"active"}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimJob(ctx); err != nil || claimed == nil || claimed.ID != activeID {
		t.Fatalf("ClaimJob() = (%+v, %v), want active job", claimed, err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if job, err := s.Job(ctx, activeID); err != nil || job == nil || job.Status != "pending" {
		t.Fatalf("recovered job = (%+v, %v), want pending", job, err)
	}
	if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
		t.Fatalf("recovered uncertain delivery was retried: (%+v, %v)", next, err)
	}
}

func TestArchiveCutoffDedupAndUserIsolation(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	start := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	firstID := completeSubmission(t, s, submission(7, "first", start))
	firstShared := submission(7, "shared-one", start.Add(time.Hour))
	firstShared.CacheKey = "shared"
	duplicateID := completeSubmission(t, s, firstShared)
	secondShared := submission(7, "shared-two", start.Add(2*time.Hour))
	secondShared.CacheKey = "shared"
	completeSubmission(t, s, secondShared)
	completeSubmission(t, s, submission(7, "last", end))
	otherUser := submission(8, "other-user", start.Add(time.Hour))
	otherUser.CacheKey = "shared"
	completeSubmission(t, s, otherUser)
	entries, err := s.Entries(ctx, 7, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].JobID != firstID || entries[1].JobID != duplicateID {
		t.Fatalf("Entries() = %+v, want first and one shared entry for user 7", entries)
	}
	if entries[1].Submission.UserID != 7 || entries[1].Submission.ReceivedAt.Before(start) || !entries[1].Submission.ReceivedAt.Before(end) {
		t.Fatalf("entry escaped user or half-open range: %+v", entries[1])
	}
}

func TestStatsCountsAnalyzeSubmissionsByStatus(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(time.Hour)
	_, _, err := s.Enqueue(ctx, "analyze", "pending", mustJSON(t, submission(5, "p", start)), 5)
	if err != nil {
		t.Fatal(err)
	}
	failedID, _, err := s.Enqueue(ctx, "analyze", "failed", mustJSON(t, submission(5, "f", start.Add(time.Minute))), 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimJob(ctx); err != nil || claimed == nil || claimed.ID != failedID {
		t.Fatalf("second ClaimJob() = (%+v, %v)", claimed, err)
	}
	if err := s.FailJob(ctx, failedID, []byte(`{}`), "no", 1, start, nil); err != nil {
		t.Fatal(err)
	}
	total, pending, failed, err := s.Stats(ctx, 5, start, end)
	if err != nil || total != 2 || pending != 1 || failed != 1 {
		t.Fatalf("Stats() = (%d, %d, %d, %v), want (2, 1, 1, nil)", total, pending, failed, err)
	}
}
