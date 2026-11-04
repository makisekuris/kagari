package personal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"kagari/internal/domain"
)

func TestThinkBoundsTaskCallbackAndKeepsFullRunAvailable(t *testing.T) {
	const fullText = "FULL_SOURCE_SENTINEL"
	result := StageResult{
		Summary: "Report saved.",
		Sources: []domain.Source{{ID: "source-1", URL: "https://example.com/report", Title: "Report source", Status: "ok", Content: strings.Repeat(fullText, 300)}},
		Files:   []string{"reports/report.md"},
	}
	fullResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	currentRun := Run{ID: 31, TaskID: 7, Kind: RunTask, Status: RunCompleted, Result: string(fullResult)}
	repo := newToolRepo()
	repo.tasks[7] = Task{ID: 7, Title: "Write report", Goal: "Save the report", Status: TaskAwaitingReview}
	repo.runs[currentRun.ID] = currentRun
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("get_run", map[string]any{"run_id": currentRun.ID}, "get-full-run"),
		responseText(`{"text":"Saved the report at reports/report.md.","notify":true}`),
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
	if len(bounded.Sources) != 1 || bounded.Sources[0].ID != "source-1" || bounded.Sources[0].URL != "https://example.com/report" || bounded.Sources[0].Title != "Report source" || bounded.Sources[0].Status != "ok" || bounded.Sources[0].Content != "" {
		t.Fatalf("bounded callback result lost source metadata or retained its body: %+v", bounded.Sources)
	}
	if currentRun.Result != string(fullResult) || repo.runs[currentRun.ID].Result != string(fullResult) {
		t.Fatal("bounding the callback mutated the original run result")
	}
	if len(fixture.requests) < 2 || !strings.Contains(requestInput(fixture.requests[1]), fullText) {
		t.Fatal("get_run did not make the complete stored source text available to the model")
	}
}

func TestBoundedTaskResultNoFilesAndLegacyInputs(t *testing.T) {
	t.Run("no files and long summary", func(t *testing.T) {
		original := StageResult{
			Summary: strings.Repeat("summary ", 400),
			Sources: []domain.Source{{ID: "source-2", URL: "https://example.com", Title: "Useful source", Status: "ok", Content: strings.Repeat("body", 2000)}},
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
		if len(bounded.Sources) != 1 || bounded.Sources[0].Title != "Useful source" || bounded.Sources[0].Content != "" {
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

	t.Run("memory review keeps the old raw cap", func(t *testing.T) {
		raw := `{"review":"` + strings.Repeat("x", maxTaskRunes+100) + `"}`
		input := TurnInput{CurrentRun: &Run{Kind: RunMemoryReview, Result: raw}}
		bounded := boundedTurnInput(input)
		if bounded.CurrentRun.Result != limitRunes(raw, maxTaskRunes) || input.CurrentRun.Result != raw {
			t.Fatal("memory review result handling changed or mutated the input")
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
	if runeLen(boundedRaw) > maxTaskRunes || len(bounded.Files) == 0 || !strings.Contains(bounded.Summary, "file paths were omitted") {
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

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
