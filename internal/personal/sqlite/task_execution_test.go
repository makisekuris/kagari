package sqlite

import (
	"encoding/json"
	"strings"
	"testing"

	"kagari/internal/personal"
)

func TestStageCheckpointBoundsFilesWithoutChangingPaths(t *testing.T) {
	longPath := strings.Repeat("nested/", personal.MaxTaskChars)
	want := "draft/final.md"
	raw := stageCheckpoint(7, personal.StageResult{Summary: "Saved report.", Files: []string{want, longPath}})
	if len([]rune(raw)) > personal.MaxTaskChars || !json.Valid([]byte(raw)) {
		t.Fatalf("checkpoint is invalid or oversized: %d runes", len([]rune(raw)))
	}
	var checkpoint struct {
		RunID int64 `json:"run_id"`
		personal.StageResult
	}
	if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.RunID != 7 || len(checkpoint.Files) != 1 || checkpoint.Files[0] != want {
		t.Fatalf("checkpoint files were altered or not pruned: %+v", checkpoint)
	}
}
