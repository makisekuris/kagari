package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/domain"
	"kagari/internal/textpage"
)

type modelResultRepo struct {
	*toolRepo
	refs map[string]domain.SourceRef
}

func (r *modelResultRepo) SaveSource(ctx context.Context, runID int64, key string, source domain.Source) (domain.SourceRef, error) {
	ref, err := r.toolRepo.SaveSource(ctx, runID, key, source)
	if err == nil {
		if r.refs == nil {
			r.refs = map[string]domain.SourceRef{}
		}
		r.refs[ref.ID] = ref
	}
	return ref, err
}

func (r *modelResultRepo) ReadMaterial(_ context.Context, _ ReadScope, id, part string, request textpage.Request) (MaterialText, error) {
	if part != "" && part != "body" {
		return MaterialText{}, errors.New("part must be body")
	}
	source, ok := r.sources[id]
	if !ok {
		return MaterialText{}, errors.New("source not found")
	}
	ref := r.refs[id]
	page, err := textpage.Read(source.Content, request, false)
	return MaterialText{Material: Material{ID: id, RunID: ref.RunID, Kind: "source", Version: page.Version, CollectedAt: ref.FetchedAt, Source: &ref}, Page: page}, err
}

func TestThinkBoundsTaskCallbackAndKeepsFullRunAvailable(t *testing.T) {
	const fullText = "FULL_SOURCE_SENTINEL"
	repo := &modelResultRepo{toolRepo: newToolRepo()}
	source := domain.Source{ID: "source-1", URL: "https://example.com/report", Title: "Report source", Status: "ok", Content: strings.Repeat(fullText, 300), FetchedAt: time.Now()}
	ref, err := repo.SaveSource(context.Background(), 31, "fixture-report", source)
	if err != nil {
		t.Fatal(err)
	}
	result := StageResult{
		Summary: "Report saved.",
		Sources: []domain.SourceRef{ref},
		Files:   []string{"reports/report.md"},
	}
	fullResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	currentRun := Run{ID: 31, TaskID: 7, Kind: RunTask, Status: RunCompleted, Result: string(fullResult)}
	repo.tasks[7] = Task{ID: 7, Title: "Write report", Goal: "Save the report", Status: TaskAwaitingReview}
	repo.runs[currentRun.ID] = currentRun
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("get_run", map[string]any{"run_id": currentRun.ID}, "get-full-run"),
		responseTool("read_material", map[string]any{"material_id": ref.ID}, "read-source"),
		responseText(`{"text":"Saved the report at reports/report.md.","notify":true,"attachments":[]}`),
	}}
	_, err = newMainAgent(t, fixture, repo).Think(context.Background(), TurnInput{
		Event: Event{ID: 90, Kind: EventTask, TaskID: 7, RunID: currentRun.ID},
		Tasks: []Task{repo.tasks[7]}, CurrentRun: &currentRun,
	})
	if err != nil {
		t.Fatal(err)
	}

	var sent TurnInput
	inputMessages, ok := fixture.requests[0]["input"].([]any)
	if !ok {
		t.Fatalf("callback input = %#v", fixture.requests[0]["input"])
	}
	for _, item := range inputMessages {
		message, _ := item.(map[string]any)
		if message["role"] == "user" {
			content, _ := message["content"].(string)
			if err := json.Unmarshal([]byte(content), &sent); err != nil {
				t.Fatal(err)
			}
		}
	}
	if sent.CurrentRun == nil {
		t.Fatalf("callback input had no current run: %#v", fixture.requests[0]["input"])
	}
	if runeLen(sent.CurrentRun.Result) > maxTaskRunes {
		t.Fatalf("callback result uses %d runes", runeLen(sent.CurrentRun.Result))
	}
	var bounded StageResult
	if err := json.Unmarshal([]byte(sent.CurrentRun.Result), &bounded); err != nil {
		t.Fatalf("bounded callback result is invalid JSON: %v", err)
	}
	if bounded.Summary != "Report saved." || len(bounded.Files) != 1 || bounded.Files[0] != "reports/report.md" {
		t.Fatalf("bounded callback result lost summary or file reference: %+v", bounded)
	}
	if len(bounded.Sources) != 1 || bounded.Sources[0].ID != ref.ID || bounded.Sources[0].URL != "https://example.com/report" || bounded.Sources[0].Title != "Report source" || bounded.Sources[0].Status != "ok" {
		t.Fatalf("bounded callback result lost source metadata or retained its body: %+v", bounded.Sources)
	}
	if currentRun.Result != string(fullResult) || repo.runs[currentRun.ID].Result != string(fullResult) {
		t.Fatal("bounding the callback mutated the original run result")
	}
	if len(fixture.requests) < 3 || !strings.Contains(requestInput(fixture.requests[2]), fullText) {
		t.Fatal("read_material did not make the stored source text available to the model")
	}
	if got := fixtureSource(t, repo.toolRepo, ref); got.Content != source.Content {
		t.Fatal("persisted source body changed")
	}
}

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
		if runeLen(boundedRaw) > maxTaskRunes || len(bounded.Files) != 0 || bounded.Summary == "" {
			t.Fatalf("bounded no-files result = %d runes, %+v", runeLen(boundedRaw), bounded)
		}
		if len(bounded.Sources) != 1 || bounded.Sources[0].Title != "Useful source" || bounded.Sources[0].TotalRunes != 10_000 {
			t.Fatalf("source metadata was not retained: %+v", bounded.Sources)
		}
		if string(raw) == "" || string(raw) != mustMarshal(t, original) {
			t.Fatal("source compaction mutated the original result")
		}
	})

	t.Run("short JSON remains unchanged", func(t *testing.T) {
		raw := ` { "summary": "saved" } `
		if got := boundedTaskResult(raw); got != raw {
			t.Fatalf("short result changed: %q", got)
		}
	})

	t.Run("invalid legacy result keeps the old rune cap", func(t *testing.T) {
		raw := `{"summary": ` + strings.Repeat("x", maxTaskRunes+100)
		if got := boundedTaskResult(raw); got != limitRunes(raw, maxTaskRunes) {
			t.Fatal("invalid legacy result did not retain the existing raw cap")
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
	if runeLen(boundedRaw) > maxTaskRunes || len(bounded.Files) == 0 || !strings.Contains(bounded.Summary, "file references were omitted") {
		t.Fatalf("oversized file list was not bounded with a marker: %d runes, %+v", runeLen(boundedRaw), bounded)
	}
	for i, path := range bounded.Files {
		if path != result.Files[i] {
			t.Fatalf("file path was altered: %q", path)
		}
	}
	if string(raw) != mustMarshal(t, result) {
		t.Fatal("bounding files mutated the original result")
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
	if !json.Valid([]byte(boundedRaw)) || json.Unmarshal([]byte(boundedRaw), &bounded) != nil {
		t.Fatalf("bounded result is not valid JSON: %q", boundedRaw)
	}
	if runeLen(boundedRaw) > maxTaskRunes || len(bounded.Files) != 1 || bounded.Files[0] != result.Files[0] {
		t.Fatalf("workspace path lost or result over budget: %d runes, %+v", runeLen(boundedRaw), bounded)
	}
	if len(bounded.Attachments) != 1 || bounded.Attachments[0].ID != result.Attachments[0].ID {
		t.Fatalf("attachment ID lost or result over budget: %d runes, %+v", runeLen(boundedRaw), bounded)
	}
}

func TestTaskOutcomeArtifactsDoNotRequireNotification(t *testing.T) {
	results := map[string]StageResult{
		"sources":     {Sources: []domain.SourceRef{{ID: "source-1"}}},
		"files":       {Files: []string{"reports/final.md"}},
		"attachments": {Attachments: []channel.Attachment{{ID: "attachment-1"}}},
	}
	for name, result := range results {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			input := TurnInput{
				Event:      Event{Kind: EventTask, TaskID: 7},
				CurrentRun: &Run{TaskID: 7, Result: string(raw)},
			}
			if taskOutcomeRequiresNotification(input) {
				t.Fatal("task result artifacts alone required notification")
			}
		})
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
	if runeLen(boundedRaw) > maxTaskRunes || len(bounded.Attachments) == 0 || len(bounded.Attachments) >= len(result.Attachments) || !strings.Contains(bounded.Summary, "file references were omitted") {
		t.Fatalf("unexpected bounded references: %s", boundedRaw)
	}
	for i, attachment := range bounded.Attachments {
		if attachment.ID != result.Attachments[i].ID {
			t.Fatalf("attachment ID was changed: %q", attachment.ID)
		}
	}
}
