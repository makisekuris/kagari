package personal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"kagari/internal/logging"
)

func TestTaskWorkspaceReportContinuesAfterRestart(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "personal.db")
	s := testStore(t, database, time.Minute)
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(database+".workspaces", storeID)
	event := openUserEvent(t, s, 1, 1, "Write and revise the report")
	task, first := createTask(t, s, event, "report", "report", "Write report.md and refine its third paragraph", "Preserve the other paragraphs.")
	finishEvent(t, s, event, TurnOutput{TaskID: task.ID}, nil)
	reportPath := filepath.Join(directory, "tasks", strconv.FormatInt(task.ID, 10), "report.md")
	original := "# Report\n\nFirst paragraph stays.\n\nSecond paragraph stays.\n\nThird paragraph: initial findings.\n"
	revised := strings.Replace(original, "Third paragraph: initial findings.", "Third paragraph: verified findings.", 1)
	final := strings.Replace(revised, "Third paragraph: verified findings.", "Third paragraph: final findings after restart.", 1)

	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("ls", map[string]string{"path": "."}, "list-first"),
		responseTool("write_file", map[string]string{"file_path": "report.md", "content": original}, "write-report"),
		responseText("Saved report.md; review the third paragraph next."),
		responseTool("read_file", map[string]string{"file_path": "report.md"}, "read-before-edit"),
		responseTool("edit_file", map[string]string{"file_path": "report.md", "old_string": "Third paragraph: initial findings.", "new_string": "Third paragraph: verified findings."}, "edit-report"),
		responseTool("read_file", map[string]string{"file_path": "report.md"}, "read-after-edit"),
		responseText("Updated only the third paragraph of report.md."),
	}}
	runtime := testRuntime(s)
	runtime.Execute = TaskExecutor(readingEngine(fixture, nil), s, directory)
	executeStage := func(r *Runtime) {
		t.Helper()
		if worked, err := r.task(ctx); err != nil || !worked {
			t.Fatalf("task worker: worked=%v, err=%v", worked, err)
		}
	}
	assertReport := func(want string) {
		t.Helper()
		content, err := os.ReadFile(reportPath)
		if err != nil || string(content) != want {
			t.Fatalf("report content: %q, err=%v; want %q", content, err, want)
		}
	}
	assertSavedRun := func(store *Store, runID int64) {
		t.Helper()
		run, err := store.Run(ctx, runID)
		if err != nil || run == nil || run.Status != RunCompleted {
			t.Fatalf("stage was not saved: %+v, %v", run, err)
		}
		var result StageResult
		if err := json.Unmarshal([]byte(run.Result), &result); err != nil || !slices.Equal(result.Files, []string{"report.md"}) {
			t.Fatalf("persisted file references: %+v, %v", result, err)
		}
	}
	continueReport := func(step string) Run {
		t.Helper()
		callback, err := s.ClaimEvent(ctx)
		if err != nil || callback == nil || callback.Kind != EventTask || callback.TaskID != task.ID {
			t.Fatalf("task callback: %+v, %v", callback, err)
		}
		_, next, err := s.ContinueTask(ctx, callback.ID, step, task.ID, task.Revision, step, false)
		if err != nil {
			t.Fatal(err)
		}
		var input TaskInput
		var previous StageResult
		if err := json.Unmarshal([]byte(next.Input), &input); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(input.Previous), &previous); err != nil || !slices.Equal(previous.Files, []string{"report.md"}) {
			t.Fatalf("file reference did not reach continuation: %+v, %v", previous, err)
		}
		finishEvent(t, s, *callback, TurnOutput{TaskID: task.ID}, nil)
		return next
	}

	executeStage(runtime)
	assertReport(original)
	assertSavedRun(s, first.ID)
	second := continueReport("Verify and revise only the third paragraph.")
	executeStage(runtime)
	assertReport(revised)
	assertSavedRun(s, second.ID)
	if !strings.Contains(requestInput(fixture.requests[4]), "Third paragraph: initial findings.") ||
		!strings.Contains(requestInput(fixture.requests[6]), "Third paragraph: verified findings.") {
		t.Fatal("actual file contents were not returned to the model before and after editing")
	}
	assertWorkspaceToolNames(t, fixture.requests[0])

	third := continueReport("Make the final revision of the third paragraph.")
	interrupted, err := s.ClaimRun(ctx, RunTask)
	if err != nil || interrupted == nil || interrupted.ID != third.ID {
		t.Fatalf("claim interrupted stage: %+v, %v", interrupted, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, database, time.Minute)
	reopenedID, err := s.EnsureIdentity(ctx)
	if err != nil || reopenedID != storeID {
		t.Fatalf("workspace identity changed across restart: %q, %v", reopenedID, err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertReport(revised)
	recovered, err := s.Run(ctx, third.ID)
	if err != nil || recovered == nil || recovered.Status != RunQueued {
		t.Fatalf("interrupted stage did not requeue: %+v, %v", recovered, err)
	}
	restartedFixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("ls", map[string]string{"path": "."}, "list-restarted"),
		responseTool("read_file", map[string]string{"file_path": "report.md"}, "read-restarted"),
		responseTool("edit_file", map[string]string{"file_path": "report.md", "old_string": "Third paragraph: verified findings.", "new_string": "Third paragraph: final findings after restart."}, "edit-restarted"),
		responseTool("read_file", map[string]string{"file_path": "report.md"}, "read-final"),
		responseText("Saved the final revision in report.md."),
	}}
	runtime = testRuntime(s)
	runtime.Execute = TaskExecutor(readingEngine(restartedFixture, nil), s, filepath.Join(database+".workspaces", reopenedID))
	executeStage(runtime)
	assertReport(final)
	assertSavedRun(s, third.ID)
	if !strings.Contains(requestInput(restartedFixture.requests[2]), "Third paragraph: verified findings.") ||
		!strings.Contains(requestInput(restartedFixture.requests[4]), "Third paragraph: final findings after restart.") {
		t.Fatal("restarted model did not receive persisted and updated file contents")
	}
}

func assertWorkspaceToolNames(t *testing.T, request map[string]any) {
	t.Helper()
	var names []string
	for _, raw := range request["tools"].([]any) {
		names = append(names, raw.(map[string]any)["name"].(string))
	}
	slices.Sort(names)
	for _, name := range []string{"edit_file", "ls", "read_attachment", "read_file", "read_url", "write_attachment", "write_file", "read_material", "get_progress", "read_progress", "checkpoint_reading", "read_checkpoints"} {
		if !slices.Contains(names, name) {
			t.Fatalf("missing task tool %s: %v", name, names)
		}
	}
}

func TestTaskWorkspaceKeepsSavedFilesWhenModelFails(t *testing.T) {
	for _, test := range []struct {
		name  string
		final fixtureResponse
	}{{"failed response", fixtureResponse{status: "failed"}}, {"empty summary", responseText(" ")}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, filepath.Join(t.TempDir(), "failed.db"), time.Minute)
			event := openUserEvent(t, s, 1, 1, "Save a report")
			task, run := createTask(t, s, event, "report", "report", "Save report.md", "")
			finishEvent(t, s, event, TurnOutput{TaskID: task.ID}, nil)
			directory := t.TempDir()
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{
				responseTool("write_file", map[string]string{"file_path": "report.md", "content": "Saved before failure."}, "saved"),
				test.final,
			}}
			runtime := testRuntime(s)
			runtime.Execute = TaskExecutor(readingEngine(fixture, nil), s, directory)
			if worked, err := runtime.task(ctx); err != nil || !worked {
				t.Fatalf("task worker: worked=%v err=%v", worked, err)
			}
			persisted, err := s.Run(ctx, run.ID)
			if err != nil || persisted == nil || persisted.Status != RunFailed {
				t.Fatalf("failed stage was not recorded: %+v, %v", persisted, err)
			}
			var result StageResult
			if err := json.Unmarshal([]byte(persisted.Result), &result); err != nil || !slices.Equal(result.Files, []string{"report.md"}) {
				t.Fatalf("failed stage lost its saved file reference: %+v, %v", result, err)
			}
			content, err := os.ReadFile(filepath.Join(directory, "tasks", strconv.FormatInt(task.ID, 10), "report.md"))
			if err != nil || string(content) != "Saved before failure." {
				t.Fatalf("failed stage lost its saved file: %q, %v", content, err)
			}
		})
	}
}

// Opt-in real-model check; all inputs and files are fictional and isolated.
func TestLivePersonalTaskWorkspace(t *testing.T) {
	engine := livePersonalEngine(t)
	engine.Config.Agent.MaxIterations = 6
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	database := filepath.Join(t.TempDir(), "workspace.db")
	s := testStore(t, database, 10*time.Minute)
	storeID, err := s.EnsureIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(database+".workspaces", storeID)
	stages := []struct {
		step, content, tool string
	}{
		{"用 write_file 生成 report.md，包含三段正文：第一段包含“目标不变。”，第二段包含“依据不变。”，第三段包含“初稿结论。”，每句话只出现一次。本阶段只创建文件。", "", "write_file"},
		{"先用 read_file 读取 report.md，再用 edit_file 把“初稿结论”替换为“已核实结论”，保留其他所有内容。", "", "edit_file"},
		{"服务已重启。先用 read_file 读取原有 report.md，再用 edit_file 把“已核实结论”替换为“重启后最终结论”，保留其他所有内容。", "", "edit_file"},
	}
	event := openUserEvent(t, s, 1, 1, "生成并逐阶段修订虚构报告")
	task, _, err := s.CreateTask(ctx, event.ID, "report", "虚构报告", TaskInput{Goal: "生成并逐阶段修订 report.md，只执行当前阶段指令。", Step: stages[0].step})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{TaskID: task.ID}, nil)
	for index, stage := range stages {
		if index == 2 {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = testStore(t, database, 10*time.Minute)
			identity, err := s.EnsureIdentity(ctx)
			if err != nil || identity != storeID {
				t.Fatal("workspace identity did not survive restart")
			}
			if err := s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
		}
		run, err := s.ClaimRun(ctx, RunTask)
		if err != nil || run == nil {
			t.Fatalf("cannot claim report stage %d", index+1)
		}
		core, logs := observer.New(zap.InfoLevel)
		result, executeErr := TaskExecutor(engine, s, directory)(logging.WithLogger(ctx, zap.New(core)), *run, nil)
		if err := s.FinishStage(ctx, run.ID, result, logging.ErrorReason(executeErr)); err != nil {
			t.Fatal(err)
		}
		if executeErr != nil {
			t.Fatalf("report stage %d: %s", index+1, logging.ErrorReason(executeErr))
		}
		calls := make(map[string]bool)
		for _, entry := range logs.FilterMessage("tool.finished").All() {
			fields := entry.ContextMap()
			if fields["outcome"] == "success" {
				name, _ := fields["tool_name"].(string)
				calls[name] = true
			}
		}
		if !calls[stage.tool] || index > 0 && !calls["read_file"] {
			t.Fatalf("stage %d did not invoke the required file tools: %v", index+1, calls)
		}
		content, err := os.ReadFile(filepath.Join(directory, "tasks", strconv.FormatInt(task.ID, 10), "report.md"))
		if err != nil || !slices.Equal(result.Files, []string{"report.md"}) {
			t.Fatalf("stage %d file=%q references=%v error=%v", index+1, content, result.Files, err)
		}
		if index == 0 {
			for _, sentence := range []string{"目标不变。", "依据不变。", "初稿结论。"} {
				if strings.Count(string(content), sentence) != 1 {
					t.Fatalf("generated report lacks the requested unique sentence %q: %q", sentence, content)
				}
			}
			// Compare edits against the actual generated report, including its formatting.
			stages[1].content = strings.Replace(string(content), "初稿结论", "已核实结论", 1)
			stages[2].content = strings.Replace(stages[1].content, "已核实结论", "重启后最终结论", 1)
		} else if string(content) != stage.content {
			t.Fatalf("stage %d altered content outside the requested edit: got %q want %q", index+1, content, stage.content)
		}
		t.Logf("stage %d PASS: real model called %s; report.md and unchanged paragraphs verified", index+1, stage.tool)
		if index < len(stages)-1 {
			callback, err := s.ClaimEvent(ctx)
			if err != nil || callback == nil {
				t.Fatal("report callback is missing")
			}
			if _, _, err := s.ContinueTask(ctx, callback.ID, "next", task.ID, task.Revision, stages[index+1].step, false); err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, *callback, TurnOutput{TaskID: task.ID}, nil)
		}
	}
}
