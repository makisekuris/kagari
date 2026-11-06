package personal

import (
	"context"
	"testing"
	"time"
)

func TestCancelledTaskInputsRemainPendingUntilExplicitContinue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, ids := createActiveTaskWithInputs(t, s, 2000, "Use this note", "https://example.org/source")
	event := openUserEventDirect(t, s, 2003, 3003, "Cancel this task")
	if _, err := s.CancelTask(ctx, event.ID, task.ID, task.Revision); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, id).Scan(&status); err != nil || status != "pending" {
			t.Fatalf("input %d: %s %v", id, status, err)
		}
	}
	if _, err := s.ReadTaskInput(ctx, run.ID, ids[0]); err == nil {
		t.Fatal("cancelled run retained read authority")
	}
	if next, err := s.ClaimRun(ctx, RunTask); err != nil || next != nil {
		t.Fatalf("cancelled task restarted: %+v %v", next, err)
	}
	finishEvent(t, s, event, TurnOutput{}, nil)
	resume := openUserEventDirect(t, s, 2004, 3004, "Continue the same task")
	_, continued, err := s.ContinueTask(ctx, resume.ID, "continue", task.ID, task.Revision, "Process the pending inputs", false)
	if err != nil {
		t.Fatal(err)
	}
	input, err := decodeTaskInput(continued.Input)
	if err != nil || len(input.InputEvents) != len(ids) {
		t.Fatalf("continued inputs=%+v err=%v", input.InputEvents, err)
	}
}

func TestTaskInputReferencesCannotBeSuppliedByRepositoryCallers(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 2100, 3100, "Create a task")
	forged := TaskInput{Goal: "Goal", InputEvents: []TaskInputReference{{EventID: event.ID, Kind: "feedback"}}}
	if _, _, err := s.CreateTask(ctx, event.ID, "forged", "Task", forged); err == nil {
		t.Fatal("accepted caller-supplied assignment")
	}
	task, _, err := s.CreateTask(ctx, event.ID, "valid", "Task", TaskInput{Goal: "Goal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReviseTask(ctx, event.ID, "revise", task.ID, task.Revision, forged); err == nil {
		t.Fatal("accepted revised caller-supplied assignment")
	}
}
