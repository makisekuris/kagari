package personal

import (
	"context"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
)

func TestRecoveredContextDefersLateInputsAndRefreshesMemoryOnNextRun(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	seq := int64(1000)
	memoryEvent := acceptanceOpenEvent(t, s, &seq, "Remember my initial report style.")
	memory, err := s.WriteMemory(ctx, memoryEvent.ID, "initial-style", MemoryChange{
		Operation: "put", Key: "report_style", Value: "FROZEN_STYLE_MARKER", Evidence: memoryEvent.Text,
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, memoryEvent, TurnOutput{}, nil)
	run := acceptanceRunningTask(t, s, &seq, "context-handoff", TaskInput{
		Goal: "Compare saved observations", Context: "Keep the original scope", MemoryRefs: []string{memory.Key},
	})
	if _, err := s.RunMemory(ctx, run.ID, memory.Key); err != nil {
		t.Fatal(err)
	}
	ref, err := s.SaveSource(ctx, run.ID, "before-interruption", domain.Source{
		URL: "https://example.org/evidence", Status: "ok", Content: "EVIDENCE_SURVIVES_RESTART", FetchedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Late material belongs to the next stage; recovery must not expand the saved Run input.
	late := acceptanceOpenEvent(t, s, &seq, "LATE_INPUT_MARKER: use the additional observation and update my report style.")
	if _, err := s.WriteMemory(ctx, late.ID, "correct-style", MemoryChange{
		Operation: "put", Key: memory.Key, Value: "NEXT_RUN_STYLE_MARKER", ExpectedVersion: memory.Version, Evidence: late.Text,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskInputs(ctx, late.ID, "late-material", run.TaskID, run.Revision, []int64{late.ID}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, late, TurnOutput{TaskID: run.TaskID, Silent: true}, nil)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.ClaimRun(ctx, RunTask)
	if err != nil || recovered == nil || recovered.ID != run.ID || recovered.Input != run.Input {
		t.Fatalf("recovered Run=%+v err=%v, want unchanged Run %d", recovered, err, run.ID)
	}
	if _, err := s.ReadTaskInput(ctx, run.ID, late.ID); err == nil {
		t.Fatal("recovered Run gained access to late input")
	}

	first := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("read_material", map[string]any{"material_id": ref.ID, "part": "body"}, "read-saved"),
		responseTool("submit_progress", map[string]any{"updates": []ProgressUpdate{{
			ID: "first-observation", Description: "PERSISTED_PROGRESS_MARKER", Status: "reported",
			Evidence: []EvidenceRef{{Kind: "material", ID: ref.ID}},
		}}}, "save-progress"),
		responseText("Recovered the saved evidence."),
	}}
	workspaceRoot := t.TempDir()
	result, err := TaskExecutor(readingEngine(first, nil), s, workspaceRoot)(ctx, *recovered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.requests) != 3 {
		t.Fatalf("recovered model requests=%d, want 3", len(first.requests))
	}
	for _, request := range first.requests {
		input := requestInput(request)
		if !strings.Contains(input, "FROZEN_STYLE_MARKER") || strings.Contains(input, "NEXT_RUN_STYLE_MARKER") || strings.Contains(input, "LATE_INPUT_MARKER") {
			t.Fatal("recovered model context changed its memory version or absorbed late input")
		}
	}
	if !strings.Contains(requestInput(first.requests[1]), "EVIDENCE_SURVIVES_RESTART") {
		t.Fatal("saved evidence was not readable by the recovered model")
	}
	if err := s.FinishStage(ctx, run.ID, result, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask {
		t.Fatalf("recovered stage callback=%+v err=%v", callback, err)
	}
	if _, err := s.SettleTask(ctx, callback.ID, run.TaskID, run.Revision, TaskCompleted, "Premature completion"); err == nil {
		t.Fatal("pending late input did not block completion")
	}
	_, next, err := s.ContinueTask(ctx, callback.ID, "read-late-input", run.TaskID, run.Revision, "Use the additional observation", false)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, *callback, TurnOutput{TaskID: run.TaskID, Silent: true}, nil)
	active, err := s.ClaimRun(ctx, RunTask)
	if err != nil || active == nil || active.ID != next.ID {
		t.Fatalf("next Run=%+v err=%v", active, err)
	}
	second := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("get_task_input", map[string]any{"event_id": late.ID}, "read-late"),
		responseTool("report_task_inputs", map[string]any{"event_ids": []int64{late.ID}}, "apply-late"),
		responseText("Included the additional observation."),
	}}
	result, err = TaskExecutor(readingEngine(second, nil), s, workspaceRoot)(ctx, *active, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.requests) != 3 {
		t.Fatalf("next model requests=%d, want 3", len(second.requests))
	}
	input := requestInput(second.requests[0])
	for _, want := range []string{"NEXT_RUN_STYLE_MARKER", "PERSISTED_PROGRESS_MARKER", ref.ID} {
		if !strings.Contains(input, want) {
			t.Fatalf("next Run context omitted %q", want)
		}
	}
	if strings.Contains(input, "FROZEN_STYLE_MARKER") || !strings.Contains(requestInput(second.requests[1]), late.Text) {
		t.Fatal("next Run did not refresh memory and read the assigned input")
	}
	if err := s.FinishStage(ctx, active.ID, result, ""); err != nil {
		t.Fatal(err)
	}
	callback, err = s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask {
		t.Fatalf("final callback=%+v err=%v", callback, err)
	}
	completed, err := s.SettleTask(ctx, callback.ID, run.TaskID, run.Revision, TaskCompleted, result.Summary)
	if err != nil || completed.Status != TaskCompleted {
		t.Fatalf("completion after applying input=%+v err=%v", completed, err)
	}
	finishEvent(t, s, *callback, TurnOutput{TaskID: completed.ID, Text: completed.Summary}, nil)
}
