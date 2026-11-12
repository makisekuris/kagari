package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/textpage"
)

func TestRunProjectionBoundsLongProgressWithoutChangingStoredData(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "projection", personal.TaskInput{Goal: "Prepare the report"})
	attachment, err := s.SaveAttachment(ctx, "final.txt", "text/plain", strings.NewReader("final report"))
	if err != nil {
		t.Fatal(err)
	}

	progress := make([]personal.ProgressUpdate, 50)
	for i := range progress {
		progress[i] = personal.ProgressUpdate{
			ID:          fmt.Sprintf("update-%02d", i),
			Status:      "unresolved",
			Description: strings.Repeat("x", 2_000),
		}
	}
	result := personal.StageResult{
		Summary:     "Completed the comparison and saved the report.",
		Progress:    progress,
		Files:       []string{"reports/final.md"},
		Attachments: []channel.Attachment{attachment},
	}
	if err := s.FinishStage(ctx, run.ID, result, ""); err != nil {
		t.Fatal(err)
	}

	stored, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var persisted personal.StageResult
	if err := json.Unmarshal([]byte(stored.Result), &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Progress) != len(progress) {
		t.Fatalf("stored result has %d progress rows, want %d", len(persisted.Progress), len(progress))
	}

	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatalf("ClaimEvent() = (%+v, %v)", callback, err)
	}
	scope := personal.ReadScope{EventID: callback.ID}
	page, err := s.QueryRuns(ctx, scope, run.TaskID, personal.ListPage{})
	if err != nil || len(page.Runs) != 1 {
		t.Fatalf("QueryRuns() = (%+v, %v)", page, err)
	}
	card := page.Runs[0]
	if got := utf8.RuneCountInString(card.Result); got > 2_000 {
		t.Fatalf("run projection has %d runes, want at most 2000", got)
	}
	var projected personal.StageResult
	if err := json.Unmarshal([]byte(card.Result), &projected); err != nil {
		t.Fatalf("projected result is invalid JSON: %v", err)
	}
	if len(projected.Progress) != 0 || len(projected.AppliedInputIDs) != 0 {
		t.Fatalf("large progress/input details were not omitted: %+v", projected)
	}
	if !strings.Contains(projected.Summary, "Completed the comparison and saved the report.") ||
		!strings.Contains(projected.Summary, "read_run") || !strings.Contains(projected.Summary, "section=result") {
		t.Fatalf("projection lost its summary or full-result path: %q", projected.Summary)
	}
	if len(projected.Files) != 1 || projected.Files[0] != "reports/final.md" ||
		len(projected.Attachments) != 1 || projected.Attachments[0].ID != attachment.ID {
		t.Fatalf("projection lost artifact identities: %+v", projected)
	}
	afterProjection, err := s.Run(ctx, run.ID)
	if err != nil || afterProjection.Result != stored.Result {
		t.Fatalf("projection changed the stored result: err=%v", err)
	}

	full, err := s.ReadRecord(ctx, scope, personal.RecordRequest{
		Kind: "run", ID: run.ID, Section: "result", Request: textpage.Request{Limit: textpage.MaxTextLimit},
	})
	if err != nil {
		t.Fatal(err)
	}
	expectedRecordPage := string([]rune(stored.Result)[:full.Page.End])
	if full.Page.TotalRunes != utf8.RuneCountInString(stored.Result) || full.Page.Text != expectedRecordPage {
		t.Fatalf("read_run result was altered: total=%d err=%v", full.Page.TotalRunes, err)
	}

	progressPage, err := s.TaskProgress(ctx, scope, run.TaskID, personal.ProgressQuery{ListPage: personal.ListPage{Limit: 50}})
	if err != nil || len(progressPage.Entries) != len(progress) {
		t.Fatalf("TaskProgress() returned %d entries, err=%v", len(progressPage.Entries), err)
	}
	progressText, err := s.ReadProgress(ctx, scope, run.TaskID, progressPage.Entries[0].Seq, textpage.Request{Limit: textpage.MaxTextLimit})
	var readProgress personal.ProgressEntry
	if err == nil {
		err = json.Unmarshal([]byte(progressText.Page.Text), &readProgress)
	}
	if err != nil || readProgress.ID != progressPage.Entries[0].ID || readProgress.Description != progress[49].Description {
		t.Fatalf("ReadProgress() lost the full entry: err=%v", err)
	}
}
