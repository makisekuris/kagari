package personal

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStageCheckpointBoundsFilesWithoutChangingPaths(t *testing.T) {
	longPath := strings.Repeat("nested/", maxTaskChars)
	want := "draft/final.md"
	raw := stageCheckpoint(7, StageResult{Summary: "Saved report.", Files: []string{want, longPath}})
	if len([]rune(raw)) > maxTaskChars || !json.Valid([]byte(raw)) {
		t.Fatalf("checkpoint is invalid or oversized: %d runes", len([]rune(raw)))
	}
	var checkpoint struct {
		RunID int64 `json:"run_id"`
		StageResult
	}
	if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.RunID != 7 || len(checkpoint.Files) != 1 || checkpoint.Files[0] != want {
		t.Fatalf("checkpoint files were altered or not pruned: %+v", checkpoint)
	}
}

func TestTaskOutcomeDoesNotNotifyForFilesAlone(t *testing.T) {
	input := TurnInput{
		Event:      Event{Kind: EventTask, TaskID: 12},
		CurrentRun: &Run{TaskID: 12, Result: `{"files":["draft/final.md"]}`},
	}
	if taskOutcomeRequiresNotification(input) {
		t.Fatal("saved files alone forced a task callback notification")
	}
}
