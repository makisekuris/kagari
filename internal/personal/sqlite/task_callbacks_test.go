package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"kagari/internal/personal"
)

func newTaskCallback(t *testing.T, s *Store, updateID, messageID int64, budgetText string) (personal.Task, personal.Event) {
	t.Helper()
	ctx := context.Background()
	user := openUserEvent(t, s, updateID, messageID, budgetText)
	task, run := createTask(t, s, user, "create", "callback", "callback goal", "callback context")
	finishEvent(t, s, user, personal.TurnOutput{}, nil)
	claimedRun, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimedRun == nil || claimedRun.ID != run.ID {
		t.Fatalf("claim source run = (%+v,%v)", claimedRun, err)
	}
	if err := s.FinishStage(ctx, run.ID, personal.StageResult{Summary: "source stage"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask {
		t.Fatalf("claim callback = (%+v,%v)", callback, err)
	}
	return task, *callback
}

func TestTaskCallbackStateStaleWhenSameRevisionHasNewerRun(t *testing.T) {
	for _, nextRunState := range []string{"running", "completed"} {
		t.Run(nextRunState, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", 30*time.Minute)
			task, callbackA := newTaskCallback(t, s, 2101, 2101, "source run A")

			user := openUserEvent(t, s, 2102, 2102, "Continue at the same revision")
			_, runB, err := s.ContinueTask(ctx, user.ID, "continue-b", task.ID, task.Revision, "Run B", false)
			if err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, user, personal.TurnOutput{}, nil)
			claimedB, err := s.ClaimRun(ctx, personal.RunTask)
			if err != nil || claimedB == nil || claimedB.ID != runB.ID {
				t.Fatalf("claim run B = (%+v,%v)", claimedB, err)
			}
			if nextRunState == "completed" {
				if err := s.FinishStage(ctx, runB.ID, personal.StageResult{Summary: "run B result"}, ""); err != nil {
					t.Fatal(err)
				}
			}

			state, err := s.TaskCallbackState(ctx, callbackA.ID)
			if err != nil || state != personal.TaskCallbackStale {
				t.Fatalf("callback A state = (%q,%v), want stale", state, err)
			}
			if _, _, err := s.ContinueTask(ctx, callbackA.ID, "late-a", task.ID, task.Revision, "must not continue", false); !errors.Is(err, personal.ErrStaleTaskCallback) {
				t.Fatalf("stale callback mutation error = %v, want ErrStaleTaskCallback", err)
			}
			before, err := s.Task(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeB, err := s.Run(ctx, runB.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AccountEvent(ctx, callbackA.ID, 3*time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := s.CompleteEvent(ctx, callbackA.ID, personal.TurnOutput{TaskID: task.ID, Text: "stale A output"}, []string{"stale A output"}); err != nil {
				t.Fatal(err)
			}
			after, err := s.Task(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterB, err := s.Run(ctx, runB.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != before.Status || after.Summary != before.Summary || after.UsedMS != before.UsedMS || after.Revision != before.Revision {
				t.Fatalf("stale callback changed task: before=%+v after=%+v", before, after)
			}
			if afterB.Status != beforeB.Status || afterB.Result != beforeB.Result || afterB.Error != beforeB.Error {
				t.Fatalf("stale callback changed run B: before=%+v after=%+v", beforeB, afterB)
			}
			var outboxCount, sourceTaskID int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, callbackA.ID).Scan(&outboxCount); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT task_id FROM personal_events WHERE id=?`, callbackA.ID).Scan(&sourceTaskID); err != nil {
				t.Fatal(err)
			}
			if outboxCount != 0 || int64(sourceTaskID) != task.ID {
				t.Fatalf("stale completion created output or lost source task: outbox=%d event_task=%d", outboxCount, sourceTaskID)
			}
		})
	}
}

func TestTaskCallbackStateRevisionAndCancellationAreStale(t *testing.T) {
	for _, change := range []string{"revision", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", 30*time.Minute)
			task, callback := newTaskCallback(t, s, 2201, 2201, "source callback")
			user := openUserEvent(t, s, 2202, 2202, "Change callback target")
			if change == "revision" {
				if _, _, err := s.ReviseTask(ctx, user.ID, "revise", task.ID, task.Revision, personal.TaskInput{Goal: "new goal"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.CancelTask(ctx, user.ID, task.ID, task.Revision); err != nil {
					t.Fatal(err)
				}
			}
			state, err := s.TaskCallbackState(ctx, callback.ID)
			if err != nil || state != personal.TaskCallbackStale {
				t.Fatalf("callback state after %s = (%q,%v), want stale", change, state, err)
			}
		})
	}
}

func TestContinuedTaskCallbackCanAccountAndCompleteAfterBudgetExhaustion(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	task, callback := newTaskCallback(t, s, 2301, 2301, "create continuation")
	_, continuation, err := s.ContinueTask(ctx, callback.ID, "continue-once", task.ID, task.Revision, "continue", false)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.TaskCallbackState(ctx, callback.ID)
	if err != nil || state != personal.TaskCallbackContinued {
		t.Fatalf("callback state after its continuation = (%q,%v), want continued", state, err)
	}
	if _, _, err := s.ContinueTask(ctx, callback.ID, "continue-twice", task.ID, task.Revision, "continue again", false); err == nil || errors.Is(err, personal.ErrStaleTaskCallback) {
		t.Fatalf("continued callback mutation error = %v, want a distinct authorization error", err)
	}
	if err := s.AccountEvent(ctx, callback.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	state, err = s.TaskCallbackState(ctx, callback.ID)
	if err != nil || state != personal.TaskCallbackContinued {
		t.Fatalf("callback state after budget exhaustion = (%q,%v), want continued", state, err)
	}
	gotTask, err := s.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotRun, err := s.Run(ctx, continuation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTask.Status != personal.TaskWaitingBudget || gotTask.UsedMS != gotTask.BudgetMS || gotRun.Status != personal.RunCancelled {
		t.Fatalf("continuation budget accounting = task %+v run %+v", gotTask, gotRun)
	}
	if err := s.CompleteEvent(ctx, callback.ID, personal.TurnOutput{TaskID: task.ID, Text: "budget notification"}, []string{"budget notification"}); err != nil {
		t.Fatal(err)
	}
	if delivery, err := s.ClaimDelivery(ctx); err != nil || delivery == nil || delivery.Text != "budget notification" {
		t.Fatalf("continued callback did not complete its notification: (%+v,%v)", delivery, err)
	}
}

func TestTaskCallbackStatePreservesAuthorizationAndDatabaseErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("wrong event kind", func(t *testing.T) {
		s := testStore(t, ":memory:", 30*time.Minute)
		user := openUserEvent(t, s, 2400, 2400, "not a task callback")
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := requireCallback(ctx, tx, user.ID, 1, 1); err == nil || errors.Is(err, personal.ErrStaleTaskCallback) {
			t.Fatalf("wrong event kind error = %v, want an authorization error", err)
		}
	})

	t.Run("missing callback event", func(t *testing.T) {
		s := testStore(t, ":memory:", 30*time.Minute)
		state, err := s.TaskCallbackState(ctx, 999999)
		if !errors.Is(err, personal.ErrRecordNotFound) || state != "" || errors.Is(err, personal.ErrStaleTaskCallback) {
			t.Fatalf("missing callback event = (%q,%v), want record-not-found error", state, err)
		}
	})

	t.Run("wrong run association", func(t *testing.T) {
		s := testStore(t, ":memory:", 30*time.Minute)
		user := openUserEvent(t, s, 2401, 2401, "two tasks")
		taskA, runA := createTask(t, s, user, "create-a", "A", "goal A", "")
		_, runB := createTask(t, s, user, "create-b", "B", "goal B", "")
		finishEvent(t, s, user, personal.TurnOutput{}, nil)
		if _, err := s.ClaimRun(ctx, personal.RunTask); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimRun(ctx, personal.RunTask); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishStage(ctx, runA.ID, personal.StageResult{Summary: "A"}, ""); err != nil {
			t.Fatal(err)
		}
		callback, err := s.ClaimEvent(ctx)
		if err != nil || callback == nil || callback.TaskID != taskA.ID {
			t.Fatalf("claim callback A = (%+v,%v)", callback, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE personal_events SET run_id=? WHERE id=?`, runB.ID, callback.ID); err != nil {
			t.Fatal(err)
		}
		state, err := s.TaskCallbackState(ctx, callback.ID)
		if err == nil || state == personal.TaskCallbackStale || errors.Is(err, personal.ErrStaleTaskCallback) {
			t.Fatalf("wrong run association = (%q,%v), want authorization error", state, err)
		}
	})

	t.Run("database closed", func(t *testing.T) {
		s := testStore(t, ":memory:", 30*time.Minute)
		_, callback := newTaskCallback(t, s, 2402, 2402, "database error")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		state, err := s.TaskCallbackState(ctx, callback.ID)
		if err == nil || state == personal.TaskCallbackStale || errors.Is(err, personal.ErrStaleTaskCallback) {
			t.Fatalf("closed database state = (%q,%v), want database error", state, err)
		}
	})
}
