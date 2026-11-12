package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/execution"
	"kagari/internal/personal/internal/testkit"
)

func TestTaskExecutorBoundsLongSummaryBeforeRuntimePersists(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "personal.db")
	s := testkit.Store(t, database, time.Minute)
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(database+".workspaces", storeID)
	event := testkit.OpenUserEvent(t, s, 1, 1, "Write a report")
	task, run := testkit.CreateTask(t, s, event, "report", "Report", "Write report.md", "")
	testkit.FinishEvent(t, s, event, personal.TurnOutput{TaskID: task.ID}, nil)

	const report = "# Report\n\nComplete report content.\n"
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("write_file", map[string]string{"file_path": "report.md", "content": report}, "write-report"),
		testkit.ResponseText(strings.Repeat("长", personal.MaxTaskChars+1)),
	}}
	runtime := testRuntime(s)
	runtime.Execute = execution.TaskExecutor(testkit.ReadingEngine(fixture, nil), s, directory)
	if worked, err := runtime.task(ctx); err != nil || !worked {
		t.Fatalf("task worker: worked=%v, err=%v", worked, err)
	}

	persisted, err := s.Run(ctx, run.ID)
	if err != nil || persisted == nil || persisted.Status != personal.RunCompleted {
		t.Fatalf("stage was not completed: %+v, %v", persisted, err)
	}
	var result personal.StageResult
	if err := json.Unmarshal([]byte(persisted.Result), &result); err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(result.Summary)); got == 0 || got > personal.MaxTaskChars || !strings.HasSuffix(result.Summary, "…") {
		t.Fatalf("persisted summary must fit the task limit and mark truncation: length=%d", got)
	}
	if len(result.Files) != 1 || result.Files[0] != "report.md" {
		t.Fatalf("saved file references=%v", result.Files)
	}
	content, err := os.ReadFile(filepath.Join(directory, "tasks", strconv.FormatInt(task.ID, 10), "report.md"))
	if err != nil || string(content) != report {
		t.Fatalf("report content=%q, err=%v", content, err)
	}
}
