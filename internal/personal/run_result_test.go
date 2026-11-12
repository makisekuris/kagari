package personal

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"kagari/internal/channel"
	"kagari/internal/domain"
)

func TestBoundedTaskResultNoFilesAndLegacyInputs(t *testing.T) {
	t.Run("no files and long summary", func(t *testing.T) {
		original := StageResult{
			Summary: strings.Repeat("summary ", 400),
			Sources: []domain.SourceRef{{ID: "source-2", URL: "https://example.com", Title: "Useful source", Status: "ok", TotalRunes: 10_000}},
		}
		raw, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		boundedRaw := boundedTaskResult(string(raw))
		var bounded StageResult
		if err := json.Unmarshal([]byte(boundedRaw), &bounded); err != nil {
			t.Fatalf("bounded result is invalid JSON: %v", err)
		}
		if utf8.RuneCountInString(boundedRaw) > maxTaskRunes || len(bounded.Files) != 0 || bounded.Summary == "" {
			t.Fatalf("bounded no-files result = %d runes, %+v", utf8.RuneCountInString(boundedRaw), bounded)
		}
		if len(bounded.Sources) != 1 || bounded.Sources[0].Title != "Useful source" || bounded.Sources[0].TotalRunes != 10_000 {
			t.Fatalf("source metadata was not retained: %+v", bounded.Sources)
		}
	})

	t.Run("short JSON remains unchanged", func(t *testing.T) {
		raw := ` { "summary": "saved" } `
		if got := boundedTaskResult(raw); got != raw {
			t.Fatalf("short result changed: %q", got)
		}
	})

	t.Run("invalid legacy result keeps the old rune cap", func(t *testing.T) {
		raw := `{"summary": ` + strings.Repeat("旧结果🙂", maxTaskRunes)
		got := boundedTaskResult(raw)
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) != maxTaskRunes || !strings.HasPrefix(raw, got) {
			t.Fatal("legacy result must retain its prefix within the Unicode text limit")
		}
	})

}

func TestBoundedTaskResultDropsWholeFilePathsWithMarker(t *testing.T) {
	result := StageResult{Summary: "Saved report."}
	for i := 0; i < 500; i++ {
		result.Files = append(result.Files, fmt.Sprintf("reports/report-%03d-%s.md", i, strings.Repeat("x", 12)))
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	boundedRaw := boundedTaskResult(string(raw))
	var bounded StageResult
	if err := json.Unmarshal([]byte(boundedRaw), &bounded); err != nil {
		t.Fatalf("bounded result is invalid JSON: %v", err)
	}
	if utf8.RuneCountInString(boundedRaw) > maxTaskRunes || len(bounded.Files) == 0 || !strings.Contains(bounded.Summary, "read_run") || !strings.Contains(bounded.Summary, "section=result") {
		t.Fatalf("oversized file list was not bounded with a marker: %d runes, %+v", utf8.RuneCountInString(boundedRaw), bounded)
	}
	for i, path := range bounded.Files {
		if path != result.Files[i] {
			t.Fatalf("file path was altered: %q", path)
		}
	}
}

func TestBoundedTaskResultKeepsFilesAndAttachmentsWithLongSummary(t *testing.T) {
	result := StageResult{
		Summary: strings.Repeat("Detailed result. ", 400),
		Files:   []string{"reports/final report.md"},
		Attachments: []channel.Attachment{
			{ID: "immutable-file-id", Name: "final report.md", MIME: "text/markdown", Size: 2048},
		},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	boundedRaw := boundedTaskResult(string(raw))
	var bounded StageResult
	if json.Unmarshal([]byte(boundedRaw), &bounded) != nil {
		t.Fatalf("bounded result is not valid JSON: %q", boundedRaw)
	}
	if utf8.RuneCountInString(boundedRaw) > maxTaskRunes || len(bounded.Files) != 1 || bounded.Files[0] != result.Files[0] {
		t.Fatalf("workspace path lost or result over budget: %d runes, %+v", utf8.RuneCountInString(boundedRaw), bounded)
	}
	if len(bounded.Attachments) != 1 || bounded.Attachments[0].ID != result.Attachments[0].ID {
		t.Fatalf("attachment ID lost or result over budget: %d runes, %+v", utf8.RuneCountInString(boundedRaw), bounded)
	}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBoundedTaskResultDropsWholeAttachmentsWithMarker(t *testing.T) {
	result := StageResult{Summary: "Saved attachments."}
	for i := 0; i < 50; i++ {
		result.Attachments = append(result.Attachments, channel.Attachment{ID: fmt.Sprintf("attachment-%032d", i), Name: strings.Repeat("report", 30)})
	}
	boundedRaw := boundedTaskResult(mustMarshal(t, result))
	var bounded StageResult
	if err := json.Unmarshal([]byte(boundedRaw), &bounded); err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(boundedRaw) > maxTaskRunes || len(bounded.Attachments) == 0 || len(bounded.Attachments) >= len(result.Attachments) || !strings.Contains(bounded.Summary, "read_run") || !strings.Contains(bounded.Summary, "section=result") {
		t.Fatalf("unexpected bounded references: %s", boundedRaw)
	}
	for i, attachment := range bounded.Attachments {
		if attachment.ID != result.Attachments[i].ID {
			t.Fatalf("attachment ID was changed: %q", attachment.ID)
		}
	}
}
