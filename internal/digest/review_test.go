package digest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
)

func TestPrepareFiltersDeduplicatesAndProjects(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	at := start.Add(time.Hour)
	makeEntry := func(id int64, user int64, received time.Time, key string) domain.ArchiveEntry {
		return domain.ArchiveEntry{JobID: id, Submission: domain.Submission{
			UserID: user, ChatID: 700 + id, Text: "submission secret", ForwardedText: "forwarded secret",
			URLs: []string{"https://submitted.example"}, CacheKey: key, ReceivedAt: received,
		}, Result: domain.Result{Body: "title"}}
	}
	first := makeEntry(9, 7, at, "Exact")
	first.Result.Body = "overview\n\nclaim"
	first.Result.Sources = []domain.Source{{ID: "source-1", Title: "source", RequestedURL: "https://requested.example", URL: "https://final.example", Status: "ok", Content: "raw source body"}}
	entries := []domain.ArchiveEntry{
		first,
		makeEntry(2, 7, at.Add(time.Hour), "exact"),
		makeEntry(3, 7, at.Add(time.Hour), "Exact"), // exact key duplicate of job 9
		makeEntry(4, 7, at.Add(time.Hour), "EXACT"),
		makeEntry(5, 7, at.Add(time.Hour), ""),
		makeEntry(6, 7, at.Add(time.Hour), ""),
		makeEntry(7, 8, at, "other-user"),
		makeEntry(8, 7, start, "lower-bound"),
		makeEntry(10, 7, end, "upper-bound"),
	}
	request := domain.DigestRequest{UserID: 7, Start: start, End: end, Version: domain.DigestVersion}
	asOf := end.Add(time.Hour)
	input, err := Prepare(request, asOf, "UTC", "profile", "instruction", entries)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []int64{8, 9, 2, 4, 5, 6}
	if len(input.Entries) != len(wantIDs) {
		t.Fatalf("Prepare() selected %d entries, want %d: %+v", len(input.Entries), len(wantIDs), input.Entries)
	}
	for i, id := range wantIDs {
		if input.Entries[i].JobID != id {
			t.Fatalf("Prepare() entry %d has job id %d, want %d", i, input.Entries[i].JobID, id)
		}
	}
	if input.UserID != 7 || !input.Start.Equal(start) || !input.Cutoff.Equal(end) || !input.AsOf.Equal(asOf) || input.Profile != "profile" || input.Instruction != "instruction" {
		t.Fatalf("Prepare() lost request context: %+v", input)
	}
	if got := input.Entries[1]; got.Body != "overview\n\nclaim" || !got.Sources[0].Usable {
		t.Fatalf("Prepare() did not project complete analysis and safe source metadata: %+v", got)
	}
	entries[0].Result.Body = "mutated"
	entries[0].Result.Sources[0].Title = "mutated"
	if got := input.Entries[1]; got.Body != "overview\n\nclaim" || got.Sources[0].Title != "source" {
		t.Fatalf("Prepare() retained aliases to caller data: %+v", got)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"submission secret", "forwarded secret", "raw source body", "700", "submitted.example"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("prepared model input leaked %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(string(encoded), "overview") || !strings.Contains(string(encoded), "source-1") {
		t.Fatalf("prepared model input omitted analysis: %s", encoded)
	}
}

func TestPrepareFiltersResultKindsBeforeDeduplication(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	entry := func(id int64, at time.Time, key string, kind domain.ResultKind) domain.ArchiveEntry {
		return domain.ArchiveEntry{
			JobID:      id,
			Submission: domain.Submission{UserID: 7, CacheKey: key, ReceivedAt: at},
			Result:     domain.Result{Kind: kind, Body: "body"},
		}
	}
	entries := []domain.ArchiveEntry{
		entry(1, start, "shared", domain.ResultKindChat),
		entry(2, start.Add(time.Minute), "shared", domain.ResultKindAnalysis),
		entry(3, start, "legacy", "other"),
		entry(4, start.Add(2*time.Minute), "legacy", ""),
	}
	request := domain.DigestRequest{UserID: 7, Start: start, End: end, Version: domain.DigestVersion}
	input, err := Prepare(request, end, "UTC", "", "instruction", entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Entries) != 2 || input.Entries[0].JobID != 2 || input.Entries[1].JobID != 4 {
		t.Fatalf("Prepare() selected %+v, want analysis and legacy analysis only", input.Entries)
	}
}

func TestPrepareRejectsInvalidWindow(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		request domain.DigestRequest
		zone    string
	}{
		{name: "empty interval", request: domain.DigestRequest{Version: domain.DigestVersion, Start: base, End: base}, zone: "UTC"},
		{name: "reversed interval", request: domain.DigestRequest{Version: domain.DigestVersion, Start: base.Add(time.Hour), End: base}, zone: "UTC"},
		{name: "invalid timezone", request: domain.DigestRequest{Version: domain.DigestVersion, Start: base, End: base.Add(time.Hour)}, zone: "No/Such_Zone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Prepare(tc.request, base, tc.zone, "", "instruction", nil); err == nil {
				t.Fatal("Prepare() error = nil")
			}
		})
	}
}

func TestPrepareRequiresCurrentVersionAndInstruction(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	request := domain.DigestRequest{UserID: 7, Start: base, End: base.Add(time.Hour)}
	if _, err := Prepare(request, base, "UTC", "", "instruction", nil); err == nil {
		t.Fatal("Prepare() accepted a versionless request")
	}
	request.Version = domain.DigestVersion
	for _, instruction := range []string{"", " \t\n"} {
		if _, err := Prepare(request, base, "UTC", "", instruction, nil); err == nil {
			t.Fatal("Prepare() accepted a blank instruction")
		}
	}
}

func TestWindowPreservesLocalWallTimeAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2024, 3, 11, 12, 34, 0, 0, loc)
	start, end := Window(cutoff, loc)
	if !end.Equal(cutoff) || !start.Equal(time.Date(2024, 3, 4, 12, 34, 0, 0, loc)) || end.Sub(start) != 167*time.Hour {
		t.Fatalf("Window() = [%s, %s), duration %s", start, end, end.Sub(start))
	}
}
