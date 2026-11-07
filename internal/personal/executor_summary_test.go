package personal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTaskExecutorBoundsLongSummaryBeforeRuntimePersists(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "personal.db")
	s := testStore(t, database, time.Minute)
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(database+".workspaces", storeID)
	event := openUserEvent(t, s, 1, 1, "Write a report")
	task, run := createTask(t, s, event, "report", "Report", "Write report.md", "")
	finishEvent(t, s, event, TurnOutput{TaskID: task.ID}, nil)

	const report = "# Report\n\nComplete report content.\n"
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("write_file", map[string]string{"file_path": "report.md", "content": report}, "write-report"),
		responseText(strings.Repeat("长", maxTaskChars+1)),
	}}
	runtime := testRuntime(s)
	runtime.Execute = TaskExecutor(readingEngine(fixture, nil), s, directory)
	if worked, err := runtime.task(ctx); err != nil || !worked {
		t.Fatalf("task worker: worked=%v, err=%v", worked, err)
	}

	persisted, err := s.Run(ctx, run.ID)
	if err != nil || persisted == nil || persisted.Status != RunCompleted {
		t.Fatalf("stage was not completed: %+v, %v", persisted, err)
	}
	var result StageResult
	if err := json.Unmarshal([]byte(persisted.Result), &result); err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(result.Summary)); got != readingEvidenceLimit || !strings.HasSuffix(result.Summary, "…") {
		t.Fatalf("summary length=%d, has truncation marker=%v; want %d runes and truncation marker", got, strings.HasSuffix(result.Summary, "…"), readingEvidenceLimit)
	}
	if len(result.Files) != 1 || result.Files[0] != "report.md" {
		t.Fatalf("saved file references=%v", result.Files)
	}
	content, err := os.ReadFile(filepath.Join(directory, "tasks", strconv.FormatInt(task.ID, 10), "report.md"))
	if err != nil || string(content) != report {
		t.Fatalf("report content=%q, err=%v", content, err)
	}
}
