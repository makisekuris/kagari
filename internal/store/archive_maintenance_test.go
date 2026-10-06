package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"kagari/internal/domain"
)

func seedArchiveJob(t *testing.T, s *Store, kind, key string, sub domain.Submission, status string, result []byte) int64 {
	t.Helper()
	raw, err := s.db.ExecContext(context.Background(), `INSERT INTO jobs(kind,key,payload,result,targets,status,next_attempt_at,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, kind, key, mustJSON(t, sub), result, `[]`, status, timestamp(time.Now()), timestamp(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	id, err := raw.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestListArchivePaginationDuplicatesAndOwner(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	first := submission(0, "first", at)
	first.CacheKey = "same-content"
	firstID := completeSubmission(t, s, first)
	second := submission(0, "second", at.Add(time.Minute))
	second.CacheKey = first.CacheKey
	secondID := completeSubmission(t, s, second)
	otherID := completeSubmission(t, s, submission(17, "other", at))
	rich := domain.Result{
		Analysis: domain.Analysis{
			Headings: &domain.AnalysisHeadings{Summary: "摘要", Discussion: "讨论", Evaluation: "评价", Uncertainties: "限制", Sources: "来源"},
			Title:    "second full result",
		},
		Sources:  []domain.Source{{URL: "https://example.test/source", Status: "ok", Content: "evidence text"}},
		Readings: []domain.Reading{{SourceID: "source-1", Role: "evidence", Question: "verify claim"}},
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET result=? WHERE id=?`, mustJSON(t, rich), secondID); err != nil {
		t.Fatal(err)
	}

	page, err := s.ListArchive(ctx, 0, 1, 0)
	if err != nil || len(page) != 1 || page[0].JobID != secondID || page[0].Submission.UserID != 0 || len(page[0].Result.Sources) != 1 || page[0].Result.Sources[0].Content != "evidence text" || len(page[0].Result.Readings) != 1 || page[0].Result.Readings[0].Question != "verify claim" {
		t.Fatalf("first page = (%+v, %v)", page, err)
	}
	page, err = s.ListArchive(ctx, 0, 2, 1)
	if err != nil || len(page) != 1 || page[0].JobID != firstID || page[0].Submission.CacheKey != "same-content" {
		t.Fatalf("second page = (%+v, %v), want earlier duplicate retained", page, err)
	}
	if page, err = s.ListArchive(ctx, 0, 10, 10); err != nil || page == nil || len(page) != 0 {
		t.Fatalf("empty page = (%v, %v), want empty slice", page, err)
	}
	if page, err = s.ListArchive(ctx, 123, 10, 0); err != nil || page == nil || len(page) != 0 {
		t.Fatalf("empty archive = (%v, %v), want empty slice", page, err)
	}
	if entry, err := s.ArchiveEntry(ctx, 0, secondID); err != nil || entry == nil || entry.JobID != secondID {
		t.Fatalf("ArchiveEntry(owner) = (%+v, %v)", entry, err)
	}
	if entry, err := s.ArchiveEntry(ctx, 17, secondID); err != nil || entry != nil {
		t.Fatalf("ArchiveEntry(wrong owner) = (%+v, %v), want nil", entry, err)
	}
	if entry, err := s.ArchiveEntry(ctx, 0, otherID); err != nil || entry != nil {
		t.Fatalf("ArchiveEntry(other owner) = (%+v, %v), want nil", entry, err)
	}
}

func TestArchiveFiltersAndInvalidArguments(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	ready := submission(0, "ready", time.Now().UTC())
	readyID := completeSubmission(t, s, ready)
	pendingID := seedArchiveJob(t, s, "analyze", "pending", submission(0, "pending", ready.ReceivedAt), "pending", nil)
	processingID := seedArchiveJob(t, s, "analyze", "processing", submission(0, "processing", ready.ReceivedAt), "processing", nil)
	failedID := seedArchiveJob(t, s, "analyze", "failed", submission(0, "failed", ready.ReceivedAt), "failed", []byte(`{}`))
	digestID := seedArchiveJob(t, s, "digest", "digest", submission(0, "digest", ready.ReceivedAt), "completed", []byte(`{}`))
	entries, err := s.ListArchive(ctx, 0, 100, 0)
	if err != nil || len(entries) != 1 || entries[0].JobID != readyID {
		t.Fatalf("ListArchive() = (%+v, %v), want completed analyze only", entries, err)
	}
	for _, id := range []int64{pendingID, processingID, failedID, digestID} {
		if entry, err := s.ArchiveEntry(ctx, 0, id); err != nil || entry != nil {
			t.Fatalf("ArchiveEntry(%d) = (%+v, %v), want nil", id, entry, err)
		}
		if deleted, err := s.DeleteArchiveEntry(ctx, 0, id); err == nil || deleted != nil {
			t.Fatalf("DeleteArchiveEntry(%d) = (%v, %v), want error", id, deleted, err)
		}
	}

	for _, tc := range []struct {
		name   string
		userID int64
		limit  int
		offset int
	}{
		{name: "negative user", userID: -1, limit: 1},
		{name: "zero limit", userID: 0, limit: 0},
		{name: "large limit", userID: 0, limit: 101},
		{name: "negative offset", userID: 0, limit: 1, offset: -1},
	} {
		t.Run("list/"+tc.name, func(t *testing.T) {
			if _, err := s.ListArchive(ctx, tc.userID, tc.limit, tc.offset); err == nil {
				t.Fatal("ListArchive() unexpectedly accepted invalid arguments")
			}
		})
	}
	for _, tc := range []struct {
		name, op   string
		userID, id int64
	}{
		{name: "entry user", op: "entry", userID: -1, id: readyID},
		{name: "entry id", op: "entry", userID: 0, id: 0},
		{name: "delete user", op: "delete", userID: -1, id: readyID},
		{name: "delete id", op: "delete", userID: 0, id: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.op == "entry" {
				if _, err := s.ArchiveEntry(ctx, tc.userID, tc.id); err == nil {
					t.Fatal("ArchiveEntry() unexpectedly accepted invalid arguments")
				}
				return
			}
			if _, err := s.DeleteArchiveEntry(ctx, tc.userID, tc.id); err == nil {
				t.Fatal("DeleteArchiveEntry() unexpectedly accepted invalid arguments")
			}
		})
	}
}

func TestDeleteArchiveEntryIsScopedAndPreservesSharedData(t *testing.T) {
	ctx := context.Background()
	_, s := maintenanceFixture(t)
	at := time.Now().UTC()
	targetID := seedArchiveJob(t, s, "analyze", "target", submission(0, "target", at), "completed", []byte(`{}`))
	siblingID := seedArchiveJob(t, s, "analyze", "sibling", submission(0, "sibling", at.Add(time.Minute)), "completed", []byte(`{}`))
	otherID := seedArchiveJob(t, s, "analyze", "other", submission(9, "other", at), "completed", []byte(`{}`))
	digestID := seedArchiveJob(t, s, "digest", "digest", submission(0, "digest", at), "completed", []byte(`{}`))
	for _, delivery := range []struct {
		jobID int64
		part  int
	}{{targetID, 1}, {targetID, 2}, {siblingID, 1}, {otherID, 1}, {digestID, 1}} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO deliveries(job_id,channel,address,part,text,status,next_attempt_at) VALUES(?,'telegram','7',?,'part','sent',?)`, delivery.jobID, delivery.part, timestamp(at)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO source_cache(url,source,cached_at) VALUES('https://cached.test',?,?)`, []byte(`{"source":true}`), timestamp(at)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('keep','value')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE update_state SET next_offset=77 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.DeleteArchiveEntry(ctx, 0, targetID)
	if err != nil || !reflect.DeepEqual(deleted, map[string]int64{"jobs": 1, "deliveries": 2}) {
		t.Fatalf("DeleteArchiveEntry() = (%v, %v), want one job and two deliveries", deleted, err)
	}
	if entry, err := s.ArchiveEntry(ctx, 0, targetID); err != nil || entry != nil {
		t.Fatalf("deleted archive = (%+v, %v), want nil", entry, err)
	}
	for _, id := range []int64{siblingID, otherID, digestID} {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id=?`, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("unrelated job %d count = (%d, %v)", id, count, err)
		}
	}
	var deliveries, cache, meta int
	var offset int64
	for query, dest := range map[string]*int{
		`SELECT count(*) FROM deliveries`:   &deliveries,
		`SELECT count(*) FROM source_cache`: &cache,
		`SELECT count(*) FROM meta`:         &meta,
	} {
		if err := s.db.QueryRowContext(ctx, query).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.QueryRowContext(ctx, `SELECT next_offset FROM update_state WHERE id=1`).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	if deliveries != 3 || cache != 1 || meta != 1 || offset != 77 {
		t.Fatalf("preserved data deliveries=%d cache=%d meta=%d offset=%d", deliveries, cache, meta, offset)
	}
	entries, err := s.Entries(ctx, 0, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(entries) != 1 || entries[0].JobID != siblingID {
		t.Fatalf("digest input after delete = (%+v, %v), want same-owner sibling only", entries, err)
	}
	if deleted, err := s.DeleteArchiveEntry(ctx, 99, otherID); err == nil || deleted != nil {
		t.Fatalf("wrong-owner delete = (%v, %v), want error", deleted, err)
	}
	var otherCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id=?`, otherID).Scan(&otherCount); err != nil || otherCount != 1 {
		t.Fatalf("wrong-owner delete changed other job: count=%d err=%v", otherCount, err)
	}
}

func TestDeleteArchiveEntryRollsBackAndRefusesReadOnly(t *testing.T) {
	ctx := context.Background()
	path, source := maintenanceFixture(t)
	id := seedArchiveJob(t, source, "analyze", "rollback", submission(0, "rollback", time.Now()), "completed", []byte(`{}`))
	if _, err := source.db.ExecContext(ctx, `INSERT INTO deliveries(job_id,channel,address,part,text,status,next_attempt_at) VALUES(?,'telegram','7',1,'part','sent',?)`, id, timestamp(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := source.db.ExecContext(ctx, `CREATE TRIGGER block_archive_job_delete BEFORE DELETE ON jobs BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	deleted, err := source.DeleteArchiveEntry(ctx, 0, id)
	if err == nil || deleted != nil {
		t.Fatalf("triggered delete = (%v, %v), want failure", deleted, err)
	}
	if entry, err := source.ArchiveEntry(ctx, 0, id); err != nil || entry == nil {
		t.Fatalf("rollback lost archive: (%+v, %v)", entry, err)
	}
	var count int
	if err := source.db.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE job_id=?`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rollback delivery count = (%d, %v), want 1", count, err)
	}
	if _, err := source.db.ExecContext(ctx, `DROP TRIGGER block_archive_job_delete`); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	readonly, err := OpenExisting(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	deleted, err = readonly.DeleteArchiveEntry(ctx, 0, id)
	if err == nil || deleted != nil {
		t.Fatalf("read-only delete = (%v, %v), want failure", deleted, err)
	}
	if entry, err := readonly.ArchiveEntry(ctx, 0, id); err != nil || entry == nil {
		t.Fatalf("read-only refusal changed archive: (%+v, %v)", entry, err)
	}
}

func TestDeleteArchiveEntryWaitsForActiveSender(t *testing.T) {
	ctx := context.Background()
	_, s := maintenanceFixture(t)
	id := seedArchiveJob(t, s, "analyze", "active-delivery", submission(7, "active-delivery", time.Now()), "completed", []byte(`{}`))
	if _, err := s.db.ExecContext(ctx, `INSERT INTO deliveries(job_id,channel,address,part,text,status,next_attempt_at) VALUES(?,'telegram','7',1,'part','pending',?)`, id, timestamp(time.Now())); err != nil {
		t.Fatal(err)
	}
	delivery, err := s.ClaimDelivery(ctx)
	if err != nil || delivery == nil || delivery.JobID != id {
		t.Fatalf("ClaimDelivery() = (%+v, %v)", delivery, err)
	}
	if deleted, err := s.DeleteArchiveEntry(ctx, 7, id); !errors.Is(err, ErrArchiveDeliverySending) || deleted != nil {
		t.Fatalf("delete during sending = (%v, %v)", deleted, err)
	}
	if entry, err := s.ArchiveEntry(ctx, 7, id); err != nil || entry == nil {
		t.Fatalf("active sender lost its archive: (%+v, %v)", entry, err)
	}
	if err := s.SentDeliveryReceipt(ctx, delivery.ID, "71"); err != nil {
		t.Fatalf("sender could not finish after rejected deletion: %v", err)
	}
	if deleted, err := s.DeleteArchiveEntry(ctx, 7, id); err != nil || deleted["jobs"] != 1 || deleted["deliveries"] != 1 {
		t.Fatalf("delete after sending = (%v, %v)", deleted, err)
	}
}
