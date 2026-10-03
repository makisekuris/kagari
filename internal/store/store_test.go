package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestMultiTargetPublicationSnapshotOrderingAndRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "multi-target.sqlite")
	targets := []domain.DeliveryTarget{{Channel: "telegram", Address: "101"}, {Channel: "telegram", Address: "-100202"}}
	s := testStore(t, path)
	id, created, err := s.Enqueue(ctx, "analyze", "multi", []byte(`{"cache_key":"multi"}`), 101, targets...)
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
	if err != nil || a1 == nil || a1.Target != targets[0] || a1.ChatID != 101 || a1.Part != 1 {
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
	if err := s.SentDelivery(ctx, b2.ID, 2002); err != nil {
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
	id, created, err := s.AcceptUpdate(context.Background(), 0, "analyze", "update", []byte(`{}`), 11, targets...)
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
	id, _, err := s.Enqueue(ctx, "analyze", "rollback", []byte(`{}`), 7)
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

const legacySchema = `
CREATE TABLE jobs (
	id INTEGER PRIMARY KEY, kind TEXT NOT NULL, key TEXT NOT NULL, payload BLOB NOT NULL, result BLOB,
	target_chat_id INTEGER NOT NULL, status TEXT NOT NULL CHECK (status IN ('pending','processing','completed','failed')),
	attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', next_attempt_at TEXT NOT NULL,
	created_at TEXT NOT NULL, UNIQUE(kind,key)
);
CREATE TABLE deliveries (
	id INTEGER PRIMARY KEY, job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE, chat_id INTEGER NOT NULL,
	part INTEGER NOT NULL, text TEXT NOT NULL, status TEXT NOT NULL CHECK (status IN ('pending','sending','sent','failed','uncertain')),
	attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', next_attempt_at TEXT NOT NULL, message_id INTEGER,
	UNIQUE(job_id,part)
);
CREATE INDEX deliveries_claim ON deliveries(status,next_attempt_at,id);
CREATE INDEX deliveries_order ON deliveries(job_id,part,status);
INSERT INTO jobs(id,kind,key,payload,result,target_chat_id,status,attempts,last_error,next_attempt_at,created_at)
	VALUES(5,'analyze','legacy','{}','{"done":true}',77,'completed',2,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
`

func openLegacyDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacySchema); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db
}

func TestMigrationPreservesLegacyOutboxAndMessageIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db := openLegacyDatabase(t, path)
	if _, err := db.Exec(`INSERT INTO deliveries(id,job_id,chat_id,part,text,status,attempts,last_error,next_attempt_at,message_id) VALUES
		(31,5,77,1,'sent text','sent',1,'','2026-01-01T00:00:00Z',901),
		(32,5,77,2,'uncertain text','uncertain',3,'outcome unknown','2026-01-02T00:00:00Z','opaque:32')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, path)
	job, err := s.Job(context.Background(), 5)
	wantTarget := []domain.DeliveryTarget{{Channel: "telegram", Address: "77"}}
	if err != nil || job == nil || !reflect.DeepEqual(job.Targets, wantTarget) || job.Status != "completed" || string(job.Result) != `{"done":true}` {
		t.Fatalf("migrated job = (%+v, %v)", job, err)
	}
	rows, err := s.db.Query(`SELECT id,job_id,channel,address,chat_id,part,text,status,attempts,last_error,next_attempt_at,message_id FROM deliveries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type savedDelivery struct {
		id, jobID, chatID, part, attempts            int64
		channel, address, text, status, reason, next string
		messageID                                    any
	}
	var saved []savedDelivery
	for rows.Next() {
		var delivery savedDelivery
		if err := rows.Scan(&delivery.id, &delivery.jobID, &delivery.channel, &delivery.address, &delivery.chatID, &delivery.part,
			&delivery.text, &delivery.status, &delivery.attempts, &delivery.reason, &delivery.next, &delivery.messageID); err != nil {
			t.Fatal(err)
		}
		saved = append(saved, delivery)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].id != 31 || saved[0].channel != "telegram" || saved[0].address != "77" || saved[0].chatID != 77 || saved[0].status != "sent" || saved[0].attempts != 1 || saved[0].messageID != "901" ||
		saved[1].id != 32 || saved[1].text != "uncertain text" || saved[1].status != "uncertain" || saved[1].attempts != 3 || saved[1].reason != "outcome unknown" || saved[1].messageID != "opaque:32" {
		t.Fatalf("migration changed legacy deliveries: %+v", saved)
	}
	var fkViolations int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&fkViolations); err != nil || fkViolations != 0 {
		t.Fatalf("foreign key check = (%d, %v)", fkViolations, err)
	}
}

func TestMigrationRollsBackOnInvalidLegacyOutbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken-legacy.sqlite")
	db := openLegacyDatabase(t, path)
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF; INSERT INTO deliveries(id,job_id,chat_id,part,text,status,next_attempt_at) VALUES(90,999,7,1,'orphan','pending','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if migrated, err := New(path); err == nil {
		_ = migrated.Close()
		t.Fatal("migration accepted orphan legacy delivery")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	columns := func(table string) map[string]bool {
		t.Helper()
		rows, err := db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := map[string]bool{}
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, dataType string
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
				t.Fatal(err)
			}
			found[name] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return found
	}
	if columns("jobs")["targets"] || columns("deliveries")["channel"] || columns("deliveries")["address"] {
		t.Fatal("failed migration left a partial schema")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM deliveries WHERE id=90 AND job_id=999`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy outbox changed after rollback: count=%d err=%v", count, err)
	}
	var tempTables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='deliveries_new'`).Scan(&tempTables); err != nil || tempTables != 0 {
		t.Fatalf("temporary migration table remains: count=%d err=%v", tempTables, err)
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
