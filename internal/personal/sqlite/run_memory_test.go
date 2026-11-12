package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"kagari/internal/personal"
	"kagari/internal/personal/agentctx"
)

func TestRunMemoryVersionSurvivesRecoveryAndForget(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	put := func(updateID, messageID int64, text, operation, value string, expected int64) personal.Memory {
		t.Helper()
		event := openUserEvent(t, s, updateID, messageID, text)
		memory, err := s.WriteMemory(ctx, event.ID, "memory-change", personal.MemoryChange{
			Operation: operation, Key: "answer_style", Value: value, ExpectedVersion: expected, Evidence: text,
		})
		if err != nil {
			t.Fatal(err)
		}
		finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
		return memory
	}

	initial := put(1, 101, "I prefer concise answers.", "put", "concise", 0)
	event := openUserEvent(t, s, 2, 102, "Run the requested review.")
	_, _, err := s.CreateTask(ctx, event.ID, "create-task", "Review", personal.TaskInput{Goal: "Review", MemoryRefs: []string{"answer_style"}})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{}, nil)
	run, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatalf("ClaimRun() = (%+v, %v)", run, err)
	}

	memoryTool := memoryGetTool(t, ctx, s, *run)
	got, err := readMemoryTool(ctx, memoryTool)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != initial {
		t.Fatalf("initial Run memory = (%+v, %v), want %+v", got, err, initial)
	}
	var rawVersions string
	if err := s.db.QueryRowContext(ctx, `SELECT memory_versions FROM personal_runs WHERE id=?`, run.ID).Scan(&rawVersions); err != nil {
		t.Fatal(err)
	}
	var versions map[string]int64
	if err := json.Unmarshal([]byte(rawVersions), &versions); err != nil || len(versions) != 1 || versions["answer_style"] != 1 {
		t.Fatalf("stored Run memory versions=%q decoded=%v err=%v", rawVersions, versions, err)
	}

	corrected := put(3, 103, "Please give slightly more detail.", "put", "detailed", 1)
	got, err = readMemoryTool(ctx, memoryTool)
	if err != nil || got == nil || *got != initial || corrected.Version != 2 {
		t.Fatalf("corrected Run memory = (%+v, %v), corrected=%+v", got, err, corrected)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || resumed == nil || resumed.ID != run.ID {
		t.Fatalf("recovered ClaimRun() = (%+v, %v), want run %d", resumed, err, run.ID)
	}
	resumedMemoryTool := memoryGetTool(t, ctx, s, *resumed)
	got, err = readMemoryTool(ctx, resumedMemoryTool)
	if err != nil || got == nil || *got != initial {
		t.Fatalf("recovered Run memory = (%+v, %v), want frozen %+v", got, err, initial)
	}

	forgotten := put(4, 104, "Forget my answer style.", "forget", "", 2)
	republished := put(5, 105, "Remember that detailed answers help.", "put", "detailed again", 3)
	got, err = readMemoryTool(ctx, resumedMemoryTool)
	if err != nil || got != nil {
		t.Fatalf("forget/re-put exposed a frozen memory: (%+v, %v), forget=%+v republish=%+v", got, err, forgotten, republished)
	}

	if err := s.FinishStage(ctx, resumed.ID, personal.StageResult{Summary: "Finished."}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask {
		t.Fatalf("ClaimEvent() = (%+v, %v), want completed task callback", callback, err)
	}
	finishEvent(t, s, *callback, personal.TurnOutput{Silent: true}, nil)
	nextEvent := openUserEvent(t, s, 6, 106, "Start another review.")
	_, _, err = s.CreateTask(ctx, nextEvent.ID, "create-next-task", "Another review", personal.TaskInput{Goal: "Review again", MemoryRefs: []string{"answer_style"}})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, nextEvent, personal.TurnOutput{}, nil)
	nextRun, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || nextRun == nil || nextRun.ID == run.ID {
		t.Fatalf("next ClaimRun() = (%+v, %v), want a fresh Run", nextRun, err)
	}
	nextMemoryTool := memoryGetTool(t, ctx, s, *nextRun)
	got, err = readMemoryTool(ctx, nextMemoryTool)
	if err != nil || got == nil || *got != republished {
		t.Fatalf("new Run memory = (%+v, %v), want current %+v", got, err, republished)
	}
}

func memoryGetTool(t *testing.T, ctx context.Context, s *Store, run personal.Run) tool.InvokableTool {
	t.Helper()
	memoryContext, err := agentctx.NewMemoryContext(ctx, agentctx.MemoryContextConfig{
		Store: s, Run: run, InitialKeys: []string{"answer_style"}, CheckRun: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := memoryContext.Tools()
	if err != nil {
		t.Fatal(err)
	}
	return findProgressTool(t, tools, "memory_get")
}

func readMemoryTool(ctx context.Context, memoryTool tool.InvokableTool) (*personal.Memory, error) {
	raw, err := memoryTool.InvokableRun(ctx, `{"key":"answer_style"}`)
	if err != nil {
		return nil, err
	}
	var result struct {
		Memory *personal.Memory `json:"memory"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, err
	}
	return result.Memory, nil
}
