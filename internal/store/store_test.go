package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func telegramTargets(ids ...int64) []domain.DeliveryTarget {
	targets := make([]domain.DeliveryTarget, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, domain.DeliveryTarget{Channel: "telegram", Address: strconv.FormatInt(id, 10)})
	}
	return targets
}

func completeSubmission(t *testing.T, s *Store, sub domain.Submission) int64 {
	t.Helper()
	id, created, err := s.Enqueue(context.Background(), "analyze", sub.Text, mustJSON(t, sub), telegramTargets(sub.ChatID))
	if err != nil || !created {
		t.Fatalf("Enqueue() = (%d, %v, %v)", id, created, err)
	}
	job, err := s.ClaimJob(context.Background())
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("ClaimJob() = (%+v, %v), want job %d", job, err, id)
	}
	if err := s.CompletePublication(context.Background(), id, mustJSON(t, domain.Result{Body: sub.Text}), nil, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRestartAndPrivateDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "jobs.sqlite")
	s := testStore(t, path)
	id, created, err := s.Enqueue(context.Background(), "analyze", "one", []byte(`{"cache_key":"one"}`), nil)
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
	if _, _, err := s.AcceptUpdate(ctx, 20, "analyze", "failed", []byte(`{"cache_key":"failed"}`), nil); err == nil {
		t.Fatal("AcceptUpdate() succeeded despite rejecting insert trigger")
	}
	if offset, err := s.Offset(ctx); err != nil || offset != 0 {
		t.Fatalf("Offset() = (%d, %v), want 0", offset, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER stop_enqueue`); err != nil {
		t.Fatal(err)
	}
	id, created, err := s.AcceptUpdate(ctx, 20, "analyze", "same", []byte(`{"cache_key":"same"}`), nil)
	if err != nil || !created || id == 0 {
		t.Fatalf("AcceptUpdate() = (%d, %v, %v)", id, created, err)
	}
	if gotID, gotCreated, err := s.AcceptUpdate(ctx, 20, "analyze", "same", []byte(`{"cache_key":"same"}`), nil); err != nil || gotCreated || gotID != 0 {
		t.Fatalf("duplicate update = (%d, %v, %v), want skipped", gotID, gotCreated, err)
	}
	gotID, gotCreated, err := s.AcceptUpdate(ctx, 21, "analyze", "same", []byte(`{"cache_key":"same"}`), nil)
	if err != nil || gotCreated || gotID != id {
		t.Fatalf("idempotent job = (%d, %v, %v), want (%d,false,nil)", gotID, gotCreated, err, id)
	}
	if _, created, err := s.AcceptUpdate(ctx, 22, "", "", nil, nil); err != nil || created {
		t.Fatalf("ignored update = (created %v, %v)", created, err)
	}
	if offset, err := s.Offset(ctx); err != nil || offset != 23 {
		t.Fatalf("Offset() = (%d, %v), want 23", offset, err)
	}
}

func TestRetryRecoveryAndOrderedUncertainDeliveries(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	id, _, err := s.Enqueue(ctx, "analyze", "retry", []byte(`{"cache_key":"retry"}`), telegramTargets(42))
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
	target := telegramTargets(42)[0]
	if err := s.CompletePublication(ctx, id, mustJSON(t, domain.Result{Body: "done"}), []domain.Delivery{
		{Target: target, Part: 1, Text: "first"}, {Target: target, Part: 2, Text: "second"},
	}, sources); err != nil {
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
	if err := s.SentDeliveryReceipt(ctx, first.ID, "1001"); err != nil {
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
	id, _, err := s.Enqueue(ctx, "analyze", "recover", []byte(`{"cache_key":"recover"}`), telegramTargets(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CompletePublication(ctx, id, []byte(`{}`), []domain.Delivery{{Target: telegramTargets(1)[0], Part: 1, Text: "message"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	activeID, _, err := s.Enqueue(ctx, "analyze", "active", []byte(`{"cache_key":"active"}`), nil)
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

func TestMultiTargetPublicationSnapshotOrderingAndRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "multi-target.sqlite")
	targets := []domain.DeliveryTarget{{Channel: "telegram", Address: "101"}, {Channel: "telegram", Address: "-100202"}}
	s := testStore(t, path)
	id, created, err := s.Enqueue(ctx, "analyze", "multi", []byte(`{"cache_key":"multi"}`), targets)
	if err != nil || !created {
		t.Fatalf("Enqueue() = (%d, %v, %v)", id, created, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.Job(ctx, id)
	if err != nil || job == nil || !reflect.DeepEqual(job.Targets, targets) {
		t.Fatalf("restarted Job().Targets = (%+v, %v), want %+v", job, err, targets)
	}
	if _, err := s.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	deliveries := []domain.Delivery{
		{Target: targets[0], Part: 1, Text: "A1"},
		{Target: targets[0], Part: 2, Text: "A2"},
		{Target: targets[1], Part: 1, Text: "B1"},
		{Target: targets[1], Part: 2, Text: "B2"},
	}
	if err := s.CompletePublication(ctx, id, []byte(`{"published":true}`), deliveries, nil); err != nil {
		t.Fatal(err)
	}
	a1, err := s.ClaimDelivery(ctx)
	if err != nil || a1 == nil || a1.Target != targets[0] || a1.Part != 1 {
		t.Fatalf("first claim = (%+v, %v), want target A part 1", a1, err)
	}
	if err := s.FailDelivery(ctx, a1.ID, "blocked", 1, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	b1, err := s.ClaimDelivery(ctx)
	if err != nil || b1 == nil || b1.Target != targets[1] || b1.Part != 1 {
		t.Fatalf("claim after target A failure = (%+v, %v), want target B part 1", b1, err)
	}
	const largeReceipt = "18446744073709551615"
	if err := s.SentDeliveryReceipt(ctx, b1.ID, largeReceipt); err != nil {
		t.Fatal(err)
	}
	var savedReceipt string
	if err := s.db.QueryRowContext(ctx, `SELECT message_id FROM deliveries WHERE id=?`, b1.ID).Scan(&savedReceipt); err != nil || savedReceipt != largeReceipt {
		t.Fatalf("large receipt = (%q, %v), want %q", savedReceipt, err, largeReceipt)
	}
	if err := s.SentDeliveryReceipt(ctx, b1.ID, largeReceipt); err != nil {
		t.Fatalf("same receipt was not idempotent: %v", err)
	}
	if err := s.SentDeliveryReceipt(ctx, b1.ID, "different-receipt"); err == nil {
		t.Fatal("different receipt replaced a sent delivery")
	}
	b2, err := s.ClaimDelivery(ctx)
	if err != nil || b2 == nil || b2.Target != targets[1] || b2.Part != 2 {
		t.Fatalf("target B second claim = (%+v, %v)", b2, err)
	}
	if err := s.SentDeliveryReceipt(ctx, b2.ID, "2002"); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
		t.Fatalf("failed target A was unexpectedly bypassed within its target: (%+v, %v)", next, err)
	}
	statuses, err := s.DeliveryTargetCounts(ctx, id)
	if err != nil || !reflect.DeepEqual(statuses, []DeliveryStatus{
		{Target: targets[1], Status: "sent", Count: 2},
		{Target: targets[0], Status: "failed", Count: 1},
		{Target: targets[0], Status: "pending", Count: 1},
	}) {
		t.Fatalf("DeliveryTargetCounts() = (%+v, %v)", statuses, err)
	}
	if err := s.RetryDeliveries(ctx, id); err != nil {
		t.Fatal(err)
	}
	a1, err = s.ClaimDelivery(ctx)
	if err != nil || a1 == nil || a1.Target != targets[0] || a1.Part != 1 {
		t.Fatalf("retried target A = (%+v, %v)", a1, err)
	}
	if err := s.SentDeliveryReceipt(ctx, a1.ID, "A1"); err != nil {
		t.Fatal(err)
	}
	a2, err := s.ClaimDelivery(ctx)
	if err != nil || a2 == nil || a2.Target != targets[0] || a2.Part != 2 {
		t.Fatalf("target A second claim = (%+v, %v)", a2, err)
	}
	if err := s.SentDeliveryReceipt(ctx, a2.ID, "A2"); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptUpdatePersistsTargetSnapshot(t *testing.T) {
	s := testStore(t, ":memory:")
	targets := []domain.DeliveryTarget{{Channel: "telegram", Address: "11"}, {Channel: "telegram", Address: "-10022"}}
	id, created, err := s.AcceptUpdate(context.Background(), 0, "analyze", "update", []byte(`{}`), targets)
	if err != nil || !created {
		t.Fatalf("AcceptUpdate() = (%d, %v, %v)", id, created, err)
	}
	job, err := s.Job(context.Background(), id)
	if err != nil || job == nil || !reflect.DeepEqual(job.Targets, targets) {
		t.Fatalf("AcceptUpdate Job().Targets = (%+v, %v)", job, err)
	}
}

func TestCompletePublicationRollsBackOnDuplicateOutboxPart(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	id, _, err := s.Enqueue(ctx, "analyze", "rollback", []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	target := domain.DeliveryTarget{Channel: "telegram", Address: "7"}
	err = s.CompletePublication(ctx, id, []byte(`{"result":true}`), []domain.Delivery{
		{Target: target, Part: 1, Text: "first"},
		{Target: target, Part: 1, Text: "duplicate"},
	}, []domain.Source{{URL: "https://cached.example", Status: "ok"}})
	if err == nil {
		t.Fatal("duplicate outbox part completed publication")
	}
	job, err := s.Job(ctx, id)
	if err != nil || job == nil || job.Status != "processing" || len(job.Result) != 0 {
		t.Fatalf("failed completion persisted the job: (%+v, %v)", job, err)
	}
	var deliveries int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE job_id=?`, id).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("failed completion left %d deliveries: %v", deliveries, err)
	}
	if source, err := s.CachedSource(ctx, "https://cached.example"); err != nil || source != nil {
		t.Fatalf("failed completion cached source: (%+v, %v)", source, err)
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
	_, _, err := s.Enqueue(ctx, "analyze", "pending", mustJSON(t, submission(5, "p", start)), nil)
	if err != nil {
		t.Fatal(err)
	}
	failedID, _, err := s.Enqueue(ctx, "analyze", "failed", mustJSON(t, submission(5, "f", start.Add(time.Minute))), nil)
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
