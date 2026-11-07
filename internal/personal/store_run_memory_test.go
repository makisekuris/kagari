package personal

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRunMemoryVersionSurvivesRecoveryAndForget(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	put := func(updateID, messageID int64, text, operation, value string, expected int64) Memory {
		t.Helper()
		event := openUserEvent(t, s, updateID, messageID, text)
		memory, err := s.WriteMemory(ctx, event.ID, "memory-change", MemoryChange{
			Operation: operation, Key: "answer_style", Value: value, ExpectedVersion: expected, Evidence: text,
		})
		if err != nil {
			t.Fatal(err)
		}
		finishEvent(t, s, event, TurnOutput{Silent: true}, nil)
		return memory
	}

	initial := put(1, 101, "I prefer concise answers.", "put", "concise", 0)
	event := openUserEvent(t, s, 2, 102, "Run the requested review.")
	_, _, err := s.CreateTask(ctx, event.ID, "create-task", "Review", TaskInput{Goal: "Review", MemoryRefs: []string{"answer_style"}})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{}, nil)
	run, err := s.ClaimRun(ctx, RunTask)
	if err != nil || run == nil {
		t.Fatalf("ClaimRun() = (%+v, %v)", run, err)
	}

	memoryContext, err := newRunMemoryContext(ctx, s, *run, []string{"answer_style"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := memoryContext.get(ctx, "answer_style"); err != nil || got == nil || *got != initial {
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
	if got, err := memoryContext.get(ctx, "answer_style"); err != nil || got == nil || *got != initial || corrected.Version != 2 {
		t.Fatalf("corrected Run memory = (%+v, %v), corrected=%+v", got, err, corrected)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ClaimRun(ctx, RunTask)
	if err != nil || resumed == nil || resumed.ID != run.ID {
		t.Fatalf("recovered ClaimRun() = (%+v, %v), want run %d", resumed, err, run.ID)
	}
	resumedContext, err := newRunMemoryContext(ctx, s, *resumed, []string{"answer_style"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := resumedContext.get(ctx, "answer_style"); err != nil || got == nil || *got != initial {
		t.Fatalf("recovered Run memory = (%+v, %v), want frozen %+v", got, err, initial)
	}

	forgotten := put(4, 104, "Forget my answer style.", "forget", "", 2)
	republished := put(5, 105, "Remember that detailed answers help.", "put", "detailed again", 3)
	if got, err := resumedContext.get(ctx, "answer_style"); err != nil || got != nil {
		t.Fatalf("forget/re-put exposed a frozen memory: (%+v, %v), forget=%+v republish=%+v", got, err, forgotten, republished)
	}

	if err := s.FinishStage(ctx, resumed.ID, StageResult{Summary: "Finished."}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask {
		t.Fatalf("ClaimEvent() = (%+v, %v), want completed task callback", callback, err)
	}
	finishEvent(t, s, *callback, TurnOutput{Silent: true}, nil)
	nextEvent := openUserEvent(t, s, 6, 106, "Start another review.")
	_, _, err = s.CreateTask(ctx, nextEvent.ID, "create-next-task", "Another review", TaskInput{Goal: "Review again", MemoryRefs: []string{"answer_style"}})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, nextEvent, TurnOutput{}, nil)
	nextRun, err := s.ClaimRun(ctx, RunTask)
	if err != nil || nextRun == nil || nextRun.ID == run.ID {
		t.Fatalf("next ClaimRun() = (%+v, %v), want a fresh Run", nextRun, err)
	}
	nextContext, err := newRunMemoryContext(ctx, s, *nextRun, []string{"answer_style"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := nextContext.get(ctx, "answer_style"); err != nil || got == nil || *got != republished {
		t.Fatalf("new Run memory = (%+v, %v), want current %+v", got, err, republished)
	}
}
