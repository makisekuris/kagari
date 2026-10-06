package digest

import (
	"encoding/json"
	"kagari/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestPrepareFreezesTextSourcesAndReceiptWindow(t *testing.T) {
	now := time.Now().UTC()
	request := domain.DigestRequest{Version: domain.DigestVersion, UserID: 7, Start: now.Add(-time.Hour), End: now}
	makeEntry := func(id, user int64, at time.Time, key string) domain.ArchiveEntry {
		return domain.ArchiveEntry{JobID: id, Submission: domain.Submission{UserID: user, ReceivedAt: at, CacheKey: key}, Result: domain.Result{Body: "归档判断", Sources: []domain.Source{{ID: "s", URL: "https://example.org", Status: "ok", Content: "source body must not be passed to digest"}}}}
	}
	entries := []domain.ArchiveEntry{makeEntry(2, 7, now.Add(-time.Minute), "same"), makeEntry(1, 7, now.Add(-2*time.Minute), "same"), makeEntry(3, 8, now.Add(-time.Minute), "other"), makeEntry(4, 7, now, "cutoff")}
	input, err := Prepare(request, now, "UTC", "profile", "frozen prompt", entries)
	if err != nil || len(input.Entries) != 1 || input.Entries[0].JobID != 1 || input.Entries[0].Body != "归档判断" || !input.Entries[0].Sources[0].Usable {
		t.Fatalf("input=%+v err=%v", input, err)
	}
	entries[1].Result.Sources[0].URL = "mutated"
	if input.Entries[0].Sources[0].URL != "https://example.org" {
		t.Fatal("snapshot aliases mutable sources")
	}
	raw, _ := json.Marshal(input)
	if strings.Contains(string(raw), "source body") {
		t.Fatal("source full text leaked into digest")
	}
	request.Version = ""
	if _, err := Prepare(request, now, "UTC", "profile", "frozen prompt", entries); err == nil {
		t.Fatal("versionless request accepted")
	}
}
