package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T, path string, budget time.Duration) *Store {
	t.Helper()
	s, err := NewStore(path, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openUserEvent(t *testing.T, s *Store, updateID, messageID int64, text string) Event {
	t.Helper()
	id, err := s.AcceptInput(context.Background(), updateID, &Input{MessageID: messageID, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.ClaimEvent(context.Background())
	if err != nil || event == nil || event.ID != id {
		t.Fatalf("ClaimEvent() = (%+v, %v), want event %d", event, err, id)
	}
	return *event
}

func createTask(t *testing.T, s *Store, event Event, key, title, goal, contextText string) (Task, Run) {
	t.Helper()
	task, run, err := s.CreateTask(context.Background(), event.ID, key, title, TaskInput{Goal: goal, Context: contextText})
	if err != nil {
		t.Fatal(err)
	}
	return task, run
}

func finishEvent(t *testing.T, s *Store, event Event, output TurnOutput, chunks []string) {
	t.Helper()
	if err := s.CompleteEvent(context.Background(), event.ID, output, chunks); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRestartOwnerOffsetAndV1Preservation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "personal.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE jobs(id INTEGER PRIMARY KEY, payload TEXT); INSERT INTO jobs VALUES(1,'keep')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, path, 30*time.Minute)
	if err := s.BindOwner(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := s.BindOwner(ctx, 42); err != nil {
		t.Fatalf("same owner rebind: %v", err)
	}
	if err := s.BindOwner(ctx, 43); err == nil {
		t.Fatal("different owner was accepted")
	}
	event := openUserEvent(t, s, 100, 900, "Make a durable task")
	task, _ := createTask(t, s, event, "create", "research", "Compare options", "scope")
	finishEvent(t, s, event, TurnOutput{}, nil)
	var associatedTask sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT task_id FROM personal_events WHERE id=?`, event.ID).Scan(&associatedTask); err != nil || associatedTask.Valid {
		t.Fatalf("zero TaskID created an association: (%+v,%v)", associatedTask, err)
	}
	if id, err := s.AcceptInput(ctx, 100, &Input{MessageID: 901, Text: "duplicate"}); err != nil || id != 0 {
		t.Fatalf("duplicate update = (%d,%v), want ignored", id, err)
	}
	if _, err := s.AcceptInput(ctx, 101, &Input{MessageID: 900, Text: "duplicate Telegram message"}); err == nil {
		t.Fatal("duplicate external message ID was accepted")
	}
	if offset, err := s.Offset(ctx); err != nil || offset != 101 {
		t.Fatalf("failed input advanced offset: (%d,%v)", offset, err)
	}
	if id, err := s.AcceptInput(ctx, 105, nil); err != nil || id != 0 {
		t.Fatalf("offset-only update = (%d,%v)", id, err)
	}
	if offset, err := s.Offset(ctx); err != nil || offset != 106 {
		t.Fatalf("Offset() = (%d,%v), want 106", offset, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := testStore(t, path, 30*time.Minute)
	if err := s2.BindOwner(ctx, 43); err == nil {
		t.Fatal("restart changed the bound owner")
	}
	if offset, err := s2.Offset(ctx); err != nil || offset != 106 {
		t.Fatalf("restarted Offset() = (%d,%v)", offset, err)
	}
	got, err := s2.Task(ctx, task.ID)
	if err != nil || got == nil || got.Goal != "Compare options" || got.Context != "scope" || got.BudgetMS != int64((30*time.Minute)/time.Millisecond) {
		t.Fatalf("persisted Task() = (%+v,%v)", got, err)
	}
	var payload string
	if err := s2.db.QueryRowContext(ctx, `SELECT payload FROM jobs WHERE id=1`).Scan(&payload); err != nil || payload != "keep" {
		t.Fatalf("legacy V1 row = (%q,%v)", payload, err)
	}
}

func TestTaskBudgetContinuesAcrossRestartAndCallbackAccounting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "personal.db")
	s := testStore(t, path, 30*time.Minute)
	event := openUserEvent(t, s, 1, 101, "Build a report")
	task, firstRun := createTask(t, s, event, "create", "report", "Build a careful report", "Use public sources")
	if run, err := s.ClaimRun(ctx, RunTask); err != nil || run != nil {
		t.Fatalf("claimed before source event completed: (%+v,%v)", run, err)
	}
	finishEvent(t, s, event, TurnOutput{TaskID: task.ID}, nil)
	run, err := s.ClaimRun(ctx, RunTask)
	if err != nil || run == nil || run.ID != firstRun.ID || run.RemainingMS != int64((30*time.Minute)/time.Millisecond) {
		t.Fatalf("ClaimRun() = (%+v,%v)", run, err)
	}
	if _, err := s.AccountRun(ctx, run.ID, 7*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccountRun(ctx, run.ID, 4*time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task(ctx, task.ID); got.UsedMS != int64((7*time.Minute)/time.Millisecond) {
		t.Fatalf("absolute run accounting double charged: %+v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, path, 30*time.Minute)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, task.ID)
	if err != nil || got.Status != TaskQueued || got.UsedMS != int64((7*time.Minute)/time.Millisecond) || got.BudgetMS != int64((30*time.Minute)/time.Millisecond) {
		t.Fatalf("recovered task accounting = (%+v,%v)", got, err)
	}
	run, err = s.ClaimRun(ctx, RunTask)
	if err != nil || run == nil || run.ID != firstRun.ID {
		t.Fatalf("reclaimed run = (%+v,%v)", run, err)
	}
	if _, err := s.AccountRun(ctx, run.ID, 8*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Outcome: "review", Summary: "First stage", NextStep: "Check two sources"}, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.Task(ctx, task.ID)
	if err != nil || got.Status != TaskAwaitingReview || got.UsedMS != int64((8*time.Minute)/time.Millisecond) || got.Summary != "First stage" {
		t.Fatalf("finished stage task = (%+v,%v)", got, err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask || callback.RunID != run.ID {
		t.Fatalf("task callback = (%+v,%v)", callback, err)
	}
	if err := s.AccountEvent(ctx, callback.ID, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.AccountEvent(ctx, callback.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, path, 30*time.Minute)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	callback, err = s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask {
		t.Fatalf("recovered callback = (%+v,%v)", callback, err)
	}
	if err := s.AccountEvent(ctx, callback.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	continued, nextRun, err := s.ContinueTask(ctx, callback.ID, "continue", task.ID, 1, "Check two sources", false)
	if err != nil {
		t.Fatal(err)
	}
	if continued.Goal != task.Goal || continued.Context != task.Context || continued.BudgetMS != task.BudgetMS || continued.UsedMS != int64((11*time.Minute)/time.Millisecond) {
		t.Fatalf("continue reset or changed task: %+v", continued)
	}
	var frozen TaskInput
	if err := json.Unmarshal([]byte(nextRun.Input), &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.Goal != task.Goal || frozen.Context != task.Context || frozen.Step != "Check two sources" || frozen.Previous == "" {
		t.Fatalf("continued input did not freeze current checkpoint: %+v", frozen)
	}
	finishEvent(t, s, *callback, TurnOutput{TaskID: task.ID}, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, path, 30*time.Minute)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = s.Task(ctx, task.ID)
	if err != nil || got.Status != TaskQueued || got.UsedMS != int64((11*time.Minute)/time.Millisecond) || got.BudgetMS != int64((30*time.Minute)/time.Millisecond) {
		t.Fatalf("continued task changed budget across restart: (%+v,%v)", got, err)
	}
	runs, err := s.Runs(ctx, task.ID, 10)
	if err != nil || len(runs) != 2 || runs[0].Result == "" || runs[1].Status != RunQueued || runs[1].RemainingMS != int64((19*time.Minute)/time.Millisecond) {
		t.Fatalf("persisted stage/run history = (%+v,%v)", runs, err)
	}
}

func TestCallbackAndRecoveryStopRunsAtCumulativeBudget(t *testing.T) {
	ctx := context.Background()
	t.Run("callback accounting cancels continuation", func(t *testing.T) {
		s := testStore(t, ":memory:", time.Minute)
		event := openUserEvent(t, s, 1, 111, "budget callback")
		task, _ := createTask(t, s, event, "create", "budget", "goal", "context")
		finishEvent(t, s, event, TurnOutput{}, nil)
		run, err := s.ClaimRun(ctx, RunTask)
		if err != nil || run == nil {
			t.Fatalf("claim run = (%+v,%v)", run, err)
		}
		if err := s.FinishStage(ctx, run.ID, StageResult{Outcome: "review", Summary: "checkpoint"}, ""); err != nil {
			t.Fatal(err)
		}
		callback, err := s.ClaimEvent(ctx)
		if err != nil || callback == nil {
			t.Fatalf("claim callback = (%+v,%v)", callback, err)
		}
		_, nextRun, err := s.ContinueTask(ctx, callback.ID, "continue", task.ID, 1, "one more step", false)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Run(ctx, nextRun.ID); got.Status != RunQueued {
			t.Fatalf("continuation was not queued: %+v", got)
		}
		if err := s.AccountEvent(ctx, callback.ID, time.Minute); err != nil {
			t.Fatal(err)
		}
		gotTask, _ := s.Task(ctx, task.ID)
		gotRun, _ := s.Run(ctx, nextRun.ID)
		if gotTask.Status != TaskWaitingBudget || gotTask.UsedMS != gotTask.BudgetMS || gotRun.Status != RunCancelled {
			t.Fatalf("budget exhaustion left continuation runnable: task=%+v run=%+v", gotTask, gotRun)
		}
		if claimed, err := s.ClaimRun(ctx, RunTask); err != nil || claimed != nil {
			t.Fatalf("exhausted run was claimed: (%+v,%v)", claimed, err)
		}
		finishEvent(t, s, *callback, TurnOutput{TaskID: task.ID, Text: "budget exhausted"}, []string{"budget exhausted"})
		if delivery, err := s.ClaimDelivery(ctx); err != nil || delivery == nil || delivery.Text != "budget exhausted" {
			t.Fatalf("callback could not notify budget state: (%+v,%v)", delivery, err)
		}
	})

	t.Run("recovery records exhausted interrupted stage once", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "personal.db")
		s := testStore(t, path, time.Minute)
		event := openUserEvent(t, s, 1, 121, "recover budget")
		task, _ := createTask(t, s, event, "create", "recover", "goal", "context")
		finishEvent(t, s, event, TurnOutput{}, nil)
		first, err := s.ClaimRun(ctx, RunTask)
		if err != nil || first == nil {
			t.Fatalf("first run = (%+v,%v)", first, err)
		}
		if _, err := s.AccountRun(ctx, first.ID, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishStage(ctx, first.ID, StageResult{Outcome: "review", Summary: "saved stage one"}, ""); err != nil {
			t.Fatal(err)
		}
		callback, err := s.ClaimEvent(ctx)
		if err != nil || callback == nil {
			t.Fatalf("first callback = (%+v,%v)", callback, err)
		}
		_, second, err := s.ContinueTask(ctx, callback.ID, "continue", task.ID, 1, "stage two", false)
		if err != nil {
			t.Fatal(err)
		}
		finishEvent(t, s, *callback, TurnOutput{TaskID: task.ID}, nil)
		claimed, err := s.ClaimRun(ctx, RunTask)
		if err != nil || claimed == nil || claimed.ID != second.ID {
			t.Fatalf("second run = (%+v,%v)", claimed, err)
		}
		if _, err := s.AccountRun(ctx, second.ID, 50*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s = testStore(t, path, time.Minute)
		if err := s.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		gotTask, err := s.Task(ctx, task.ID)
		if err != nil || gotTask.Status != TaskWaitingBudget || gotTask.UsedMS != gotTask.BudgetMS || gotTask.Summary != "saved stage one" {
			t.Fatalf("recovered exhausted task = (%+v,%v)", gotTask, err)
		}
		gotRun, err := s.Run(ctx, second.ID)
		if err != nil || gotRun.Status != RunFailed || gotRun.ElapsedMS != int64((50*time.Second)/time.Millisecond) || gotRun.Error == "" {
			t.Fatalf("recovered exhausted run = (%+v,%v)", gotRun, err)
		}
		var callbacks int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_events WHERE run_id=?`, second.ID).Scan(&callbacks); err != nil || callbacks != 1 {
			t.Fatalf("recovery callback count = (%d,%v)", callbacks, err)
		}
		if claimed, err := s.ClaimRun(ctx, RunTask); err != nil || claimed != nil {
			t.Fatalf("exhausted recovered stage was queued: (%+v,%v)", claimed, err)
		}
		recoveredCallback, err := s.ClaimEvent(ctx)
		if err != nil || recoveredCallback == nil || recoveredCallback.RunID != second.ID {
			t.Fatalf("recovery callback = (%+v,%v)", recoveredCallback, err)
		}
		runs, err := s.Runs(ctx, task.ID, 10)
		if err != nil || len(runs) != 2 || runs[0].Result == "" {
			t.Fatalf("prior stage was lost: (%+v,%v)", runs, err)
		}
	})
}

func TestBudgetExtensionRequiresExplicitUserContinuation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 131, "budget extension")
	task, _ := createTask(t, s, event, "create", "budget", "fixed goal", "fixed context")
	finishEvent(t, s, event, TurnOutput{}, nil)
	run, err := s.ClaimRun(ctx, RunTask)
	if err != nil || run == nil {
		t.Fatalf("claim = (%+v,%v)", run, err)
	}
	if _, err := s.AccountRun(ctx, run.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Outcome: "review", Summary: "partial"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatalf("callback = (%+v,%v)", callback, err)
	}
	if _, _, err := s.ContinueTask(ctx, callback.ID, "automatic-extension", task.ID, 1, "next", false); err == nil {
		t.Fatal("task callback continued a budget-waiting task")
	}
	finishEvent(t, s, *callback, TurnOutput{TaskID: task.ID}, nil)
	user := openUserEvent(t, s, 2, 132, "continue with another grant")
	continued, next, err := s.ContinueTask(ctx, user.ID, "extend", task.ID, 1, "next", true)
	if err != nil {
		t.Fatal(err)
	}
	if continued.BudgetMS != 2*int64(time.Minute/time.Millisecond) || continued.UsedMS != int64(time.Minute/time.Millisecond) ||
		continued.Goal != task.Goal || continued.Context != task.Context || next.RemainingMS != int64(time.Minute/time.Millisecond) {
		t.Fatalf("explicit extension reset or changed the task: task=%+v run=%+v", continued, next)
	}
	finishEvent(t, s, user, TurnOutput{}, nil)
}

func TestTaskCallbackAuthorityRevisionCancelAndExplicitContinue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", 30*time.Minute)
	event := openUserEvent(t, s, 1, 201, "Create two related tasks")
	taskA, _ := createTask(t, s, event, "a", "alpha", "Goal A", "Context A")
	taskB, _ := createTask(t, s, event, "b", "beta", "Goal B", "Context B")
	finishEvent(t, s, event, TurnOutput{}, nil)
	runA, err := s.ClaimRun(ctx, RunTask)
	if err != nil || runA == nil || runA.TaskID != taskA.ID {
		t.Fatalf("run A = (%+v,%v)", runA, err)
	}
	runB, err := s.ClaimRun(ctx, RunTask)
	if err != nil || runB == nil || runB.TaskID != taskB.ID {
		t.Fatalf("run B = (%+v,%v)", runB, err)
	}
	if err := s.FinishStage(ctx, runA.ID, StageResult{Outcome: "review", Summary: "A result"}, ""); err != nil {
		t.Fatal(err)
	}
	callbackA, err := s.ClaimEvent(ctx)
	if err != nil || callbackA == nil || callbackA.TaskID != taskA.ID {
		t.Fatalf("callback A = (%+v,%v)", callbackA, err)
	}
	if _, _, err := s.ContinueTask(ctx, callbackA.ID, "wrong-task", taskB.ID, 1, "change B", false); err == nil {
		t.Fatal("callback changed another task")
	}
	if _, _, err := s.ReviseTask(ctx, callbackA.ID, "revise", taskB.ID, 1, TaskInput{Goal: "arbitrary", Context: ""}); err == nil {
		t.Fatal("callback revised a task goal")
	}
	if _, err := s.SettleTask(ctx, callbackA.ID, taskB.ID, 1, TaskCompleted, "wrong"); err == nil {
		t.Fatal("callback settled another task")
	}
	userRevision := openUserEvent(t, s, 2, 202, "Revise alpha")
	updated, revisedRun, err := s.ReviseTask(ctx, userRevision.ID, "revise-a", taskA.ID, 1, TaskInput{Goal: "Goal A v2", Context: "Context A v2"})
	if err != nil || updated.Revision != 2 || revisedRun.Status != RunQueued {
		t.Fatalf("ReviseTask() = (%+v,%+v,%v)", updated, revisedRun, err)
	}
	if _, _, err := s.ContinueTask(ctx, callbackA.ID, "late", taskA.ID, 1, "old callback", false); err == nil {
		t.Fatal("stale callback continued a superseded revision")
	}
	finishEvent(t, s, userRevision, TurnOutput{}, nil)
	if err := s.FinishStage(ctx, runA.ID, StageResult{Outcome: "done", Summary: "late old result"}, ""); err != nil {
		t.Fatal(err)
	}
	gotA, _ := s.Task(ctx, taskA.ID)
	if gotA.Revision != 2 || gotA.Goal != "Goal A v2" || gotA.Status != TaskQueued {
		t.Fatalf("late completion revived old task: %+v", gotA)
	}
	activeRevisionRun, err := s.ClaimRun(ctx, RunTask)
	if err != nil || activeRevisionRun == nil || activeRevisionRun.TaskID != taskA.ID || activeRevisionRun.Revision != 2 {
		t.Fatalf("revision 2 run claim = (%+v,%v)", activeRevisionRun, err)
	}
	activeRevisionEvent := openUserEvent(t, s, 3, 203, "Revise alpha again")
	updatedAgain, revisedAgain, err := s.ReviseTask(ctx, activeRevisionEvent.ID, "revise-a-again", taskA.ID, 2,
		TaskInput{Goal: "Goal A v3", Context: "Context A v3"})
	if err != nil || updatedAgain.Revision != 3 || revisedAgain.Status != RunQueued {
		t.Fatalf("second revision = (%+v,%+v,%v)", updatedAgain, revisedAgain, err)
	}
	if err := s.FinishStage(ctx, activeRevisionRun.ID, StageResult{Outcome: "done", Summary: "late revision 2 result"}, ""); err != nil {
		t.Fatal(err)
	}
	gotA, _ = s.Task(ctx, taskA.ID)
	if gotA.Revision != 3 || gotA.Goal != "Goal A v3" || gotA.Status != TaskQueued {
		t.Fatalf("running old revision overwrote new one: %+v", gotA)
	}
	finishEvent(t, s, activeRevisionEvent, TurnOutput{}, nil)
	if err := s.FinishStage(ctx, runB.ID, StageResult{Outcome: "review", Summary: "B result"}, ""); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, *callbackA, TurnOutput{}, nil)
	callbackB, err := s.ClaimEvent(ctx)
	if err != nil || callbackB == nil || callbackB.TaskID != taskB.ID {
		t.Fatalf("callback B = (%+v,%v)", callbackB, err)
	}
	if _, err := s.SettleTask(ctx, callbackB.ID, taskA.ID, 3, TaskCompleted, "wrong"); err == nil {
		t.Fatal("callback B settled task A")
	}
	if _, err := s.SettleTask(ctx, callbackB.ID, taskB.ID, 1, TaskCompleted, "B done"); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, *callbackB, TurnOutput{}, nil)
	active, err := s.ClaimRun(ctx, RunTask)
	if err != nil || active == nil || active.TaskID != taskA.ID || active.Revision != 3 {
		t.Fatalf("revised run claim = (%+v,%v)", active, err)
	}
	cancelEvent := openUserEvent(t, s, 4, 204, "Cancel alpha")
	cancelled, err := s.CancelTask(ctx, cancelEvent.ID, taskA.ID, 3)
	if err != nil || cancelled.Status != TaskCancelled {
		t.Fatalf("CancelTask() = (%+v,%v)", cancelled, err)
	}
	if err := s.FinishStage(ctx, active.ID, StageResult{Outcome: "done", Summary: "late"}, ""); err != nil {
		t.Fatal(err)
	}
	gotA, _ = s.Task(ctx, taskA.ID)
	if gotA.Status != TaskCancelled {
		t.Fatalf("late result revived cancellation: %+v", gotA)
	}
	finishEvent(t, s, cancelEvent, TurnOutput{}, nil)
	continueEvent := openUserEvent(t, s, 5, 205, "Continue alpha")
	continued, run, err := s.ContinueTask(ctx, continueEvent.ID, "continue-after-cancel", taskA.ID, 3, "resume same goal", false)
	if err != nil || continued.Status != TaskQueued || continued.Goal != "Goal A v3" || run.Revision != 3 {
		t.Fatalf("explicit continue after cancel = (%+v,%+v,%v)", continued, run, err)
	}
	finishEvent(t, s, continueEvent, TurnOutput{}, nil)
}

func TestMemoryProvenanceCASForgetReplayAndQuietReview(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", 30*time.Minute)
	firstText := "I prefer concise summaries in Chinese."
	first := openUserEvent(t, s, 1, 301, firstText)
	finishEvent(t, s, first, TurnOutput{Silent: true}, nil)
	if id, err := s.AcceptInput(ctx, 1, &Input{MessageID: 302, Text: "duplicate"}); err != nil || id != 0 {
		t.Fatalf("duplicate source = (%d,%v)", id, err)
	}
	var reviews int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE event_id=? AND kind='memory_review'`, first.ID).Scan(&reviews); err != nil || reviews != 1 {
		t.Fatalf("review dedup count = (%d,%v)", reviews, err)
	}
	reviewRun, err := s.ClaimRun(ctx, RunMemory)
	if err != nil || reviewRun == nil || reviewRun.Kind != RunMemoryReview {
		t.Fatalf("memory review claim = (%+v,%v)", reviewRun, err)
	}
	input, err := s.MemoryReviewInput(ctx, reviewRun.ID)
	if err != nil || input.Event.Text != firstText {
		t.Fatalf("review source = (%+v,%v)", input, err)
	}
	change := MemoryChange{Operation: "put", Key: "style.summary", Value: "concise summaries", ExpectedVersion: 0, Evidence: firstText}
	if err := s.FinishMemoryReview(ctx, reviewRun.ID, MemoryReview{Changes: []MemoryChange{change}}, ""); err != nil {
		t.Fatal(err)
	}
	memory, err := s.Memory(ctx, "style.summary")
	if err != nil || memory == nil || memory.Version != 1 || memory.SourceEventID != first.ID || memory.Value != "concise summaries" {
		t.Fatalf("automatically saved memory = (%+v,%v)", memory, err)
	}
	if callback, err := s.ClaimEvent(ctx); err != nil || callback != nil {
		t.Fatalf("successful review notified user: (%+v,%v)", callback, err)
	}
	if delivery, err := s.ClaimDelivery(ctx); err != nil || delivery != nil {
		t.Fatalf("silent event created delivery: (%+v,%v)", delivery, err)
	}
	material, err := s.AcceptInput(ctx, 2, &Input{MessageID: 303, Text: "a forwarded article", Material: true})
	if err != nil {
		t.Fatal(err)
	}
	materialEvent, err := s.ClaimEvent(ctx)
	if err != nil || materialEvent == nil || materialEvent.ID != material {
		t.Fatalf("material event = (%+v,%v)", materialEvent, err)
	}
	finishEvent(t, s, *materialEvent, TurnOutput{Silent: true}, nil)
	var materialReviews int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE event_id=? AND kind='memory_review'`, material).Scan(&materialReviews); err != nil || materialReviews != 0 {
		t.Fatalf("material review count = (%d,%v)", materialReviews, err)
	}
	var role string
	if err := s.db.QueryRowContext(ctx, `SELECT role FROM personal_history WHERE event_id=?`, material).Scan(&role); err != nil || role != "material" {
		t.Fatalf("material history role = (%q,%v)", role, err)
	}

	secondText := "I prefer brief reports from now on."
	second := openUserEvent(t, s, 3, 304, secondText)
	putOne := MemoryChange{Operation: "put", Key: "style.summary", Value: "brief reports", ExpectedVersion: 1, Evidence: secondText}
	putTwo := MemoryChange{Operation: "put", Key: "style.summary", Value: "short reports", ExpectedVersion: 1, Evidence: secondText}
	if _, err := s.QueueMemory(ctx, second.ID, "put-one", putOne); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueMemory(ctx, second.ID, "put-two", putTwo); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, second, TurnOutput{Silent: true}, nil)
	firstExplicit, err := s.ClaimRun(ctx, RunMemory)
	if err != nil || firstExplicit == nil || firstExplicit.Kind != RunMemory {
		t.Fatalf("explicit memory claim = (%+v,%v)", firstExplicit, err)
	}
	if err := s.ApplyMemory(ctx, firstExplicit.ID); err != nil {
		t.Fatal(err)
	}
	memory, _ = s.Memory(ctx, "style.summary")
	if memory.Version != 2 || memory.Value != "brief reports" || memory.SourceEventID != second.ID {
		t.Fatalf("first explicit write = %+v", memory)
	}
	secondExplicit, err := s.ClaimRun(ctx, RunMemory)
	if err != nil || secondExplicit == nil {
		t.Fatalf("second explicit claim = (%+v,%v)", secondExplicit, err)
	}
	if err := s.ApplyMemory(ctx, secondExplicit.ID); err != nil {
		t.Fatal(err)
	}
	failedRun, _ := s.Run(ctx, secondExplicit.ID)
	if failedRun.Status != RunFailed || failedRun.Error == "" {
		t.Fatalf("CAS conflict was not recorded: %+v", failedRun)
	}
	conflictEvent, err := s.ClaimEvent(ctx)
	if err != nil || conflictEvent == nil || conflictEvent.Kind != EventMemory || conflictEvent.RunID != secondExplicit.ID {
		t.Fatalf("CAS conflict callback = (%+v,%v)", conflictEvent, err)
	}
	finishEvent(t, s, *conflictEvent, TurnOutput{}, nil)
	secondReview, err := s.ClaimRun(ctx, RunMemory)
	if err != nil || secondReview == nil || secondReview.Kind != RunMemoryReview {
		t.Fatalf("second automatic review = (%+v,%v)", secondReview, err)
	}
	if err := s.FinishMemoryReview(ctx, secondReview.ID, MemoryReview{}, ""); err != nil {
		t.Fatal(err)
	}

	forgetText := "Forget style.summary; I no longer want that preference stored."
	forgetEvent := openUserEvent(t, s, 4, 305, forgetText)
	forget := MemoryChange{Operation: "forget", Key: "style.summary", ExpectedVersion: 2}
	forgetRun, err := s.QueueMemory(ctx, forgetEvent.ID, "forget", forget)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, forgetEvent, TurnOutput{Silent: true}, nil)
	claimedForget, err := s.ClaimRun(ctx, RunMemory)
	if err != nil || claimedForget == nil || claimedForget.ID != forgetRun.ID {
		t.Fatalf("forget claim = (%+v,%v)", claimedForget, err)
	}
	if err := s.ApplyMemory(ctx, claimedForget.ID); err != nil {
		t.Fatal(err)
	}
	memory, _ = s.Memory(ctx, "style.summary")
	if !memory.Deleted || memory.Version != 3 || memory.SourceEventID != forgetEvent.ID {
		t.Fatalf("forgot memory = %+v", memory)
	}
	forgetReview, err := s.ClaimRun(ctx, RunMemory)
	if err != nil || forgetReview == nil || forgetReview.Kind != RunMemoryReview {
		t.Fatalf("forget review = (%+v,%v)", forgetReview, err)
	}
	input, err = s.MemoryReviewInput(ctx, forgetReview.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundTombstone := false
	for _, item := range input.Memories {
		if item.Key == "style.summary" && item.Deleted && item.Version == 3 {
			foundTombstone = true
		}
	}
	if !foundTombstone {
		t.Fatalf("review omitted tombstone: %+v", input.Memories)
	}
	// An automatic review of the same source cannot undo its explicit forget.
	staleReviewPut := MemoryChange{Operation: "put", Key: "style.summary", Value: "old preference", ExpectedVersion: 2, Evidence: forgetText}
	if err := s.FinishMemoryReview(ctx, forgetReview.ID, MemoryReview{Changes: []MemoryChange{staleReviewPut}}, ""); err != nil {
		t.Fatal(err)
	}
	memory, _ = s.Memory(ctx, "style.summary")
	if !memory.Deleted || memory.Version != 3 {
		t.Fatalf("review resurrected same-event forget: %+v", memory)
	}
	active, err := s.Memories(ctx, "style.summary", 10)
	if err != nil || len(active) != 0 {
		t.Fatalf("search returned forgotten memory: (%+v,%v)", active, err)
	}
	history, err := s.MemoryHistory(ctx, "style.summary", 10)
	if err != nil || len(history) != 3 || !history[len(history)-1].Deleted {
		t.Fatalf("memory version history = (%+v,%v)", history, err)
	}

	// A delayed write sourced from the earlier message must fail against the tombstone.
	oldRaw, _ := json.Marshal(change)
	res, err := s.db.ExecContext(ctx, `INSERT INTO personal_runs(event_id,kind,input,status,created_at) VALUES(?,? ,?,'queued',?)`, first.ID, RunMemory, string(oldRaw), timestamp(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	oldID, _ := res.LastInsertId()
	oldRun, err := s.ClaimRun(ctx, RunMemory)
	if err != nil || oldRun == nil || oldRun.ID != oldID {
		t.Fatalf("old replay claim = (%+v,%v)", oldRun, err)
	}
	if err := s.ApplyMemory(ctx, oldRun.ID); err != nil {
		t.Fatal(err)
	}
	oldResult, _ := s.Run(ctx, oldRun.ID)
	if oldResult.Status != RunFailed {
		t.Fatalf("old replay not rejected: %+v", oldResult)
	}
	memory, _ = s.Memory(ctx, "style.summary")
	if !memory.Deleted || memory.Version != 3 {
		t.Fatalf("old replay resurrected tombstone: %+v", memory)
	}
}

func TestFourConcurrentTaskClaimsAndSingleRunPerTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", 30*time.Minute)
	var tasks []Task
	var runs []Run
	for i := int64(1); i <= 5; i++ {
		event := openUserEvent(t, s, i, 400+i, fmt.Sprintf("task %d", i))
		task, run := createTask(t, s, event, fmt.Sprintf("create-%d", i), fmt.Sprintf("task-%d", i), fmt.Sprintf("goal-%d", i), "")
		finishEvent(t, s, event, TurnOutput{}, nil)
		tasks, runs = append(tasks, task), append(runs, run)
	}
	// Add an extra queued run for task 1; its active run must keep it unclaimable.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO personal_runs(task_id,revision,event_id,kind,input,status,remaining_ms,created_at) VALUES(?,?,?,'task',?,'queued',?,?)`,
		tasks[0].ID, tasks[0].Revision, runs[0].EventID, runs[0].Input, runs[0].RemainingMS, timestamp(time.Now())); err != nil {
		t.Fatal(err)
	}
	claimed := make(chan *Run, 4)
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := s.ClaimRun(ctx, RunTask)
			claimed <- run
			errs <- err
		}()
	}
	wg.Wait()
	close(claimed)
	close(errs)
	seen := map[int64]bool{}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for run := range claimed {
		if run == nil {
			t.Fatal("one of four concurrent claimers found no work")
		}
		if seen[run.TaskID] {
			t.Fatalf("two claimers selected task %d", run.TaskID)
		}
		seen[run.TaskID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("claimed task count = %d, want 4", len(seen))
	}
	// The fifth task remains available; after it is claimed, task 1's duplicate is blocked.
	if run, err := s.ClaimRun(ctx, RunTask); err != nil || run == nil || run.TaskID != tasks[4].ID {
		t.Fatalf("fifth task claim = (%+v,%v)", run, err)
	}
	if run, err := s.ClaimRun(ctx, RunTask); err != nil || run != nil {
		t.Fatalf("duplicate run claimed while task 1 active: (%+v,%v)", run, err)
	}
}

func TestOutboxOrderReceiptsReplyLinkingAndUnknownRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "personal.db")
	s := testStore(t, path, 30*time.Minute)
	event := openUserEvent(t, s, 1, 501, "Please research this")
	task, _ := createTask(t, s, event, "create", "research", "Research this", "")
	finishEvent(t, s, event, TurnOutput{TaskID: task.ID, Text: "first answer second answer third answer"}, []string{"first", "second", "third"})
	first, err := s.ClaimDelivery(ctx)
	if err != nil || first == nil || first.Text != "first" || first.Attempts != 1 {
		t.Fatalf("first delivery = (%+v,%v)", first, err)
	}
	if err := s.UpdateDelivery(ctx, first.ID, "sent", 9001, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDelivery(ctx, first.ID, "sent", 9001, "", time.Time{}); err != nil {
		t.Fatalf("same sent receipt retry: %v", err)
	}
	second, err := s.ClaimDelivery(ctx)
	if err != nil || second == nil || second.Text != "second" {
		t.Fatalf("second delivery = (%+v,%v)", second, err)
	}
	if err := s.UpdateDelivery(ctx, second.ID, "uncertain", 0, "response lost", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
		t.Fatalf("third chunk passed uncertain second: (%+v,%v)", next, err)
	}

	byReceipt, err := s.AcceptInput(ctx, 2, &Input{MessageID: 502, ReplyToID: 9001, Text: "Reply to the first answer"})
	if err != nil {
		t.Fatal(err)
	}
	replyEvent, err := s.ClaimEvent(ctx)
	if err != nil || replyEvent == nil || replyEvent.ID != byReceipt || replyEvent.TaskID != task.ID {
		t.Fatalf("receipt reply link = (%+v,%v)", replyEvent, err)
	}
	finishEvent(t, s, *replyEvent, TurnOutput{}, nil)
	byInbound, err := s.AcceptInput(ctx, 3, &Input{MessageID: 503, ReplyToID: 501, Text: "Reply to my source"})
	if err != nil {
		t.Fatal(err)
	}
	inboundReply, err := s.ClaimEvent(ctx)
	if err != nil || inboundReply == nil || inboundReply.ID != byInbound || inboundReply.TaskID != task.ID {
		t.Fatalf("inbound reply link = (%+v,%v)", inboundReply, err)
	}
	finishEvent(t, s, *inboundReply, TurnOutput{}, nil)

	unknown := openUserEvent(t, s, 4, 504, "unknown send")
	finishEvent(t, s, unknown, TurnOutput{Text: "unknown"}, []string{"unknown"})
	delivery, err := s.ClaimDelivery(ctx)
	if err != nil || delivery == nil || delivery.Text != "unknown" {
		t.Fatalf("unknown delivery = (%+v,%v)", delivery, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, path, 30*time.Minute)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var recovered string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, delivery.ID).Scan(&recovered); err != nil || recovered != "uncertain" {
		t.Fatalf("sending recovery = (%q,%v)", recovered, err)
	}
	if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
		t.Fatalf("uncertain delivery retried after recovery: (%+v,%v)", next, err)
	}
}

func TestMemoryValidationAndDuplicateCompletion(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 601, "I enjoy tea")
	if _, err := s.QueueMemory(ctx, event.ID, "bad", MemoryChange{Operation: "put", Key: "Tea", Value: "tea", Evidence: "tea"}); err == nil {
		t.Fatal("noncanonical memory key accepted")
	}
	if _, err := s.QueueMemory(ctx, event.ID, "bad-evidence", MemoryChange{Operation: "put", Key: "drink", Value: "tea", Evidence: "coffee"}); err == nil {
		t.Fatal("evidence not in source accepted")
	}
	finishEvent(t, s, event, TurnOutput{Silent: true}, nil)
	if err := s.CompleteEvent(ctx, event.ID, TurnOutput{}, nil); err == nil {
		t.Fatal("duplicate event completion accepted")
	}
}

func TestTaskInputAndBudgetValidation(t *testing.T) {
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 701, "task")
	if _, _, err := s.CreateTask(context.Background(), event.ID, "x", "", TaskInput{Goal: "goal"}); err == nil {
		t.Fatal("empty task title accepted")
	}
	if _, _, err := s.CreateTask(context.Background(), event.ID, "x", "title", TaskInput{Goal: string(make([]rune, maxTaskChars+1))}); err == nil {
		t.Fatal("oversized task goal accepted")
	}
	if _, err := NewStore(":memory:", 0); err == nil {
		t.Fatal("zero task budget accepted")
	}
}

func TestMutationRequestKeysAreIdempotentPerSourceEvent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 702, "I prefer tea and short answers")
	input := TaskInput{Goal: "Prepare a summary", Context: "Keep it short"}
	task1, run1, err := s.CreateTask(ctx, event.ID, "create-key", "summary", input)
	if err != nil {
		t.Fatal(err)
	}
	task2, run2, err := s.CreateTask(ctx, event.ID, "create-key", "summary", input)
	if err != nil || task2.ID != task1.ID || run2.ID != run1.ID {
		t.Fatalf("duplicate create = (%+v,%+v,%v)", task2, run2, err)
	}
	if _, _, err := s.CreateTask(ctx, event.ID, "create-key", "different", input); err == nil {
		t.Fatal("request key reused for different create input")
	}

	revisedInput := TaskInput{Goal: "Prepare a concise summary", Context: "Keep it short"}
	revision1, revisionRun1, err := s.ReviseTask(ctx, event.ID, "revise-key", task1.ID, 1, revisedInput)
	if err != nil {
		t.Fatal(err)
	}
	revision2, revisionRun2, err := s.ReviseTask(ctx, event.ID, "revise-key", task1.ID, 1, revisedInput)
	if err != nil || revision2.Revision != revision1.Revision || revisionRun2.ID != revisionRun1.ID {
		t.Fatalf("duplicate revision = (%+v,%+v,%v)", revision2, revisionRun2, err)
	}
	if revision2.Revision != 2 {
		t.Fatalf("revision dedup incremented twice: %+v", revision2)
	}

	change := MemoryChange{Operation: "put", Key: "drink.favorite", Value: "tea", ExpectedVersion: 0, Evidence: "I prefer tea"}
	memory1, err := s.QueueMemory(ctx, event.ID, "memory-key", change)
	if err != nil {
		t.Fatal(err)
	}
	memory2, err := s.QueueMemory(ctx, event.ID, "memory-key", change)
	if err != nil || memory2.ID != memory1.ID {
		t.Fatalf("duplicate memory request = (%+v,%v)", memory2, err)
	}
	if _, err := s.QueueMemory(ctx, event.ID, "memory-key", MemoryChange{Operation: "forget", Key: change.Key, ExpectedVersion: 1}); err == nil {
		t.Fatal("memory request key reused for different operation")
	}
	finishEvent(t, s, event, TurnOutput{}, nil)
	var taskCount, memoryCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_tasks`).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE kind='memory'`).Scan(&memoryCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 || memoryCount != 1 {
		t.Fatalf("deduplicated records: tasks=%d memory-runs=%d", taskCount, memoryCount)
	}
}

func TestCancelTaskIdempotenceAndLateStageNoop(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 801, "cancel test")
	task, run := createTask(t, s, event, "create", "cancel", "goal", "")
	finishEvent(t, s, event, TurnOutput{}, nil)
	claimed, err := s.ClaimRun(ctx, RunTask)
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = (%+v,%v)", claimed, err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Outcome: "review", Summary: "ready for callback"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask {
		t.Fatalf("callback = (%+v,%v)", callback, err)
	}
	cancelEvent := openUserEvent(t, s, 2, 802, "cancel while callback is running")
	first, err := s.CancelTask(ctx, cancelEvent.ID, task.ID, 1)
	if err != nil || first.Status != TaskCancelled {
		t.Fatalf("cancel = (%+v,%v)", first, err)
	}
	second, err := s.CancelTask(ctx, cancelEvent.ID, task.ID, 1)
	if err != nil || second.Status != TaskCancelled {
		t.Fatalf("repeat cancel = (%+v,%v)", second, err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Outcome: "done", Summary: "late"}, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Task(ctx, task.ID)
	if got.Status != TaskCancelled || got.Summary != "ready for callback" {
		t.Fatalf("late completion changed cancelled task: %+v", got)
	}
	finishEvent(t, s, cancelEvent, TurnOutput{}, nil)
	if err := s.CompleteEvent(ctx, callback.ID, TurnOutput{TaskID: task.ID, Text: "stale callback"}, []string{"stale callback"}); err != nil {
		t.Fatal(err)
	}
	var outboxCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, callback.ID).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("cancelled callback enqueued stale reply: (%d,%v)", outboxCount, err)
	}
	continueEvent := openUserEvent(t, s, 3, 803, "explicitly continue cancelled task")
	continued, _, err := s.ContinueTask(ctx, continueEvent.ID, "continue", task.ID, 1, "resume", false)
	if err != nil || continued.Status != TaskQueued {
		t.Fatalf("explicit continue after cancel = (%+v,%v)", continued, err)
	}
	finishEvent(t, s, continueEvent, TurnOutput{}, nil)
}
