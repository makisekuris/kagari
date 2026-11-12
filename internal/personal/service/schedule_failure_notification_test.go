package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

func failScheduledRun(t *testing.T, store *sqlite.Store, now time.Time) (personal.ScheduleOccurrence, personal.Run) {
	t.Helper()
	ctx := context.Background()
	occurrence, run := testkit.DispatchRun(t, store, now)
	if _, err := store.BeginScheduledRun(ctx, run.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishStage(ctx, run.ID, personal.StageResult{Summary: "Partial result"}, "Source request failed"); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(store)
	runtime.Think = func(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
		if input.Event.Kind != personal.EventTask || input.Event.TaskID != run.TaskID || input.CurrentRun == nil {
			t.Fatalf("unexpected failure callback: event=%+v run=%+v", input.Event, input.CurrentRun)
		}
		_, err := store.SettleTask(ctx, input.Event.ID, input.Event.TaskID, input.CurrentRun.Revision, personal.TaskFailed, "Source request failed")
		return personal.TurnOutput{TaskID: input.Event.TaskID, Silent: true}, err
	}
	worked, err := runtime.turn(ctx)
	if err != nil || !worked {
		t.Fatalf("runtime did not settle and notify failed run: worked=%t err=%v", worked, err)
	}
	return occurrence, run
}

func TestScheduleEveryFailureRunGetsItsOwnNotification(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Minute)
	schedule := testkit.CreateHourlySchedule(t, store, personal.WindowNone)
	firstOccurrence, firstRun := failScheduledRun(t, store, *schedule.NextRunAt)

	current, err := store.Schedule(ctx, schedule.ID)
	if err != nil || current == nil {
		t.Fatalf("schedule after first failure=(%+v,%v)", current, err)
	}
	secondOccurrence, secondRun := failScheduledRun(t, store, *current.NextRunAt)
	if firstOccurrence.TaskID == secondOccurrence.TaskID || firstRun.TaskID == secondRun.TaskID {
		t.Fatal("separate periods reused a task")
	}
	current, err = store.Schedule(ctx, schedule.ID)
	if err != nil || current.Status != personal.ScheduleActive {
		t.Fatalf("single-run failures paused the plan: (%+v,%v)", current, err)
	}

	for _, run := range []personal.Run{firstRun, secondRun} {
		snapshot, err := store.Diagnostic(ctx, 0, run.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		var notification strings.Builder
		for _, delivery := range snapshot.Deliveries {
			notification.WriteString(delivery.Text)
		}
		if notification.Len() == 0 || !strings.Contains(notification.String(), "Source request failed") {
			t.Fatalf("failure notification missing for task %d: %+v", run.TaskID, snapshot.Deliveries)
		}
	}
}
