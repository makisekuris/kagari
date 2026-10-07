package store

import (
	"context"
	"testing"
	"time"
)

func TestChatResultsStayOutOfAnalysisCacheAndArchive(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	at := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	seed := func(key, result, status string, received time.Time) int64 {
		t.Helper()
		return seedArchiveJob(t, s, "analyze", key, submission(5, key, received), status, []byte(result))
	}
	legacyID := seed("legacy", `{"body":"legacy"}`, "completed", at)
	analysisID := seed("analysis", `{"kind":"analysis","body":"analysis"}`, "completed", at.Add(time.Minute))
	chatID := seed("chat", `{"kind":"chat","body":"chat"}`, "completed", at.Add(2*time.Minute))
	unknownID := seed("unknown", `{"kind":"other","body":"unknown"}`, "completed", at.Add(3*time.Minute))
	seed("pending-chat", `{"kind":"chat"}`, "pending", at.Add(4*time.Minute))
	seed("failed-chat", `{"kind":"chat"}`, "failed", at.Add(5*time.Minute))

	for _, key := range []string{"chat", "unknown"} {
		if result, err := s.FindResult(ctx, key); err != nil || result != nil {
			t.Fatalf("FindResult(%q) = (%+v, %v), want no reusable analysis", key, result, err)
		}
	}
	for key, want := range map[string]string{"legacy": "legacy", "analysis": "analysis"} {
		if result, err := s.FindResult(ctx, key); err != nil || result == nil || result.Body != want {
			t.Fatalf("FindResult(%q) = (%+v, %v), want %q", key, result, err, want)
		}
	}

	entries, err := s.Entries(ctx, 5, at.Add(-time.Minute), at.Add(time.Hour))
	if err != nil || len(entries) != 2 || entries[0].JobID != legacyID || entries[1].JobID != analysisID {
		t.Fatalf("Entries() = (%+v, %v), want legacy and analysis only", entries, err)
	}
	page, err := s.ListArchive(ctx, 5, 1, 0)
	if err != nil || len(page) != 1 || page[0].JobID != analysisID {
		t.Fatalf("ListArchive() = (%+v, %v), want analysis after filtering before pagination", page, err)
	}
	for _, id := range []int64{legacyID, analysisID} {
		if entry, err := s.ArchiveEntry(ctx, 5, id); err != nil || entry == nil {
			t.Fatalf("ArchiveEntry(%d) = (%+v, %v), want archive entry", id, entry, err)
		}
	}
	for _, id := range []int64{chatID, unknownID} {
		if entry, err := s.ArchiveEntry(ctx, 5, id); err != nil || entry != nil {
			t.Fatalf("ArchiveEntry(%d) = (%+v, %v), want nil", id, entry, err)
		}
		if _, err := s.DeleteArchiveEntry(ctx, 5, id); err == nil {
			t.Fatalf("DeleteArchiveEntry(%d) unexpectedly accepted a non-analysis result", id)
		}
	}
	for _, id := range []int64{chatID, unknownID} {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id=?`, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("job %d count = (%d, %v), want preserved", id, count, err)
		}
	}
	total, pending, failed, err := s.Stats(ctx, 5, at.Add(-time.Minute), at.Add(time.Hour))
	if err != nil || total != 4 || pending != 1 || failed != 1 {
		t.Fatalf("Stats() = (%d, %d, %d, %v), want (4, 1, 1, nil)", total, pending, failed, err)
	}
}
