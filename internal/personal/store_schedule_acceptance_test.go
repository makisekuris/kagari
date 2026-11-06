package personal

import (
	"context"
	"strings"
	"testing"
	"time"
)

func completeScheduledCheck(t *testing.T, s *Store, run Run, evidence string) Event {
	t.Helper()
	ctx := context.Background()
	event := dispatchTestFinish(t, s, run)
	check := &CheckAssessment{Complete: true, RequiredSources: []SourceAssessment{{
		ID: "feed", Complete: true, Evidence: evidence,
	}}}
	if _, err := s.SettleTask(ctx, event.ID, run.TaskID, run.Revision, TaskCompleted, "Checked the feed", check); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{TaskID: run.TaskID, Text: "Check complete"}, []string{"Check complete"})
	return event
}

func TestScheduleLateOlderCheckDoesNotRegressCoverage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	schedule := dispatchTestSchedule(t, s, WindowSinceSuccess)

	olderOccurrence, olderRun := dispatchTestRun(t, s, *schedule.NextRunAt)
	olderStart := schedule.NextRunAt.Add(5 * time.Minute)
	if _, err := s.BeginScheduledRun(ctx, olderRun.ID, olderStart); err != nil {
		t.Fatal(err)
	}
	olderCallback := dispatchTestFinish(t, s, olderRun)
	if _, err := s.SettleTask(ctx, olderCallback.ID, olderRun.TaskID, olderRun.Revision, TaskWaitingInput, "Need a detail"); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, olderCallback, TurnOutput{TaskID: olderRun.TaskID, Text: "Need a detail"}, []string{"Need a detail"})

	current, err := s.Schedule(ctx, schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	newerOccurrence, newerRun := dispatchTestRun(t, s, *current.NextRunAt)
	newerStart := current.NextRunAt.Add(5 * time.Minute)
	if _, err := s.BeginScheduledRun(ctx, newerRun.ID, newerStart); err != nil {
		t.Fatal(err)
	}
	continueEvent := openUserEvent(t, s, 3, 3, "Continue the older check")
	_, continuedRun, err := s.ContinueTask(ctx, continueEvent.ID, "continue-older", olderRun.TaskID, olderRun.Revision, "Finish the original interval", false)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, continueEvent, TurnOutput{TaskID: olderRun.TaskID, Silent: true}, nil)
	claimed, err := s.ClaimRun(ctx, RunTask)
	if err != nil || claimed == nil || claimed.ID != continuedRun.ID {
		t.Fatalf("claim older continuation = (%+v,%v), want run %d", claimed, err, continuedRun.ID)
	}
	if _, err := s.BeginScheduledRun(ctx, claimed.ID, newerStart.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Both runs have crossed the persistent start boundary. Settle the newer interval first.
	completeScheduledCheck(t, s, newerRun, "Checked the newer interval")
	current, err = s.Schedule(ctx, schedule.ID)
	if err != nil || current.CoveredThrough == nil || !current.CoveredThrough.Equal(newerStart) {
		t.Fatalf("newer check coverage = (%+v,%v), want %s", current, err, newerStart)
	}

	completeScheduledCheck(t, s, *claimed, "Finished the older interval late")

	current, err = s.Schedule(ctx, schedule.ID)
	if err != nil || current.CoveredThrough == nil || !current.CoveredThrough.Equal(newerStart) {
		t.Fatalf("late older check regressed coverage: (%+v,%v)", current, err)
	}
	olderContext, err := s.ScheduleForTask(ctx, olderOccurrence.TaskID)
	if err != nil || olderContext == nil || olderContext.Occurrence.CreditedAt != nil {
		t.Fatalf("out-of-order older check was credited: (%+v,%v)", olderContext, err)
	}
	newerContext, err := s.ScheduleForTask(ctx, newerOccurrence.TaskID)
	if err != nil || newerContext == nil || newerContext.Occurrence.CreditedAt == nil {
		t.Fatalf("newer check lost its credit: (%+v,%v)", newerContext, err)
	}
}

func TestScheduleCreditedCheckSurvivesTaskRevision(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	schedule := dispatchTestSchedule(t, s, WindowSinceSuccess)
	_, run := dispatchTestRun(t, s, *schedule.NextRunAt)
	started := schedule.NextRunAt.Add(time.Minute)
	if _, err := s.BeginScheduledRun(ctx, run.ID, started); err != nil {
		t.Fatal(err)
	}
	completeScheduledCheck(t, s, run, "Checked the full interval")

	before, err := s.ScheduleForTask(ctx, run.TaskID)
	if err != nil || before == nil || before.Occurrence.CreditedAt == nil || before.Schedule.CoveredThrough == nil {
		t.Fatalf("successful check was not credited: (%+v,%v)", before, err)
	}
	creditedAt, coveredThrough := *before.Occurrence.CreditedAt, *before.Schedule.CoveredThrough

	reviseEvent := openUserEvent(t, s, 2, 2, "Revise the completed task")
	revised, _, err := s.ReviseTask(ctx, reviseEvent.ID, "revise-after-credit", run.TaskID, run.Revision, TaskInput{Goal: "Revisit the source with a new question"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, reviseEvent, TurnOutput{TaskID: run.TaskID, Silent: true}, nil)
	if revised.Revision != run.Revision+1 {
		t.Fatalf("revision = %d, want %d", revised.Revision, run.Revision+1)
	}

	after, err := s.ScheduleForTask(ctx, run.TaskID)
	if err != nil || after == nil || after.Occurrence.CreditedAt == nil || !after.Occurrence.CreditedAt.Equal(creditedAt) ||
		after.Occurrence.CreditedTaskRevision != run.Revision || after.Schedule.CoveredThrough == nil || !after.Schedule.CoveredThrough.Equal(coveredThrough) {
		t.Fatalf("revision revoked an accepted check: (%+v,%v)", after, err)
	}
}

func TestScheduleExplicitCoverageResetCanReuseTheOriginalBaseline(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	schedule := dispatchTestSchedule(t, s, WindowSinceSuccess)
	_, run := dispatchTestRun(t, s, *schedule.NextRunAt)
	started := schedule.NextRunAt.Add(time.Minute)
	if _, err := s.BeginScheduledRun(ctx, run.ID, started); err != nil {
		t.Fatal(err)
	}
	completeScheduledCheck(t, s, run, "Checked the full interval")
	event := openUserEvent(t, s, 2, 2, "Check again from the original start")
	reset, _, err := s.UpdateSchedule(ctx, event.ID, "reset-coverage", schedule.ID, schedule.Revision,
		SchedulePatch{InitialCheckFrom: schedule.BaselineAt}, started.Add(time.Minute))
	if err != nil || reset.ScopeRevision != schedule.ScopeRevision+1 || reset.CoveredThrough != nil || !reset.BaselineAt.Equal(*schedule.BaselineAt) {
		t.Fatalf("explicit reset reused old coverage: %+v %v", reset, err)
	}
	prior, err := s.ScheduleForTask(ctx, run.TaskID)
	if err != nil || prior.Occurrence.CreditedAt == nil {
		t.Fatalf("new scope revoked historical credit: %+v %v", prior, err)
	}
}

func TestScheduleWaitingBudgetAllowsNextRunAndExtensionIsPerTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	budget := int64(1)
	input := ScheduleInput{Title: "hourly", Goal: "Check the feed", Cron: "0 * * * *", Timezone: "UTC", WindowMode: WindowNone, TaskBudgetMS: &budget}
	createEvent := openUserEvent(t, s, 1, 1, "Register a one millisecond plan budget")
	schedule, _, err := s.CreateSchedule(ctx, createEvent.ID, "budget-plan", input, dispatchTestNow)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, createEvent, TurnOutput{ScheduleID: schedule.ID}, nil)

	_, firstRun := dispatchTestRun(t, s, *schedule.NextRunAt)
	if _, err := s.BeginScheduledRun(ctx, firstRun.ID, schedule.NextRunAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccountRun(ctx, firstRun.ID, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, firstRun.ID, StageResult{Summary: "Saved partial work"}, ""); err != nil {
		t.Fatal(err)
	}
	budgetCallback, err := s.ClaimEvent(ctx)
	if err != nil || budgetCallback == nil || budgetCallback.Kind != EventTask {
		t.Fatalf("budget callback = (%+v,%v)", budgetCallback, err)
	}
	waiting, err := s.Task(ctx, firstRun.TaskID)
	if err != nil || waiting.Status != TaskWaitingBudget {
		t.Fatalf("task did not reach stable waiting_budget: (%+v,%v)", waiting, err)
	}
	finishEvent(t, s, *budgetCallback, TurnOutput{TaskID: firstRun.TaskID, Text: "This run needs more time"}, []string{"This run needs more time"})

	current, err := s.Schedule(ctx, schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, secondRun := dispatchTestRun(t, s, *current.NextRunAt)
	secondTask, err := s.Task(ctx, secondRun.TaskID)
	if err != nil || secondTask.BudgetMS != budget {
		t.Fatalf("next occurrence budget = (%+v,%v), want %d", secondTask, err, budget)
	}

	extendEvent := openUserEvent(t, s, 2, 2, "Extend only the first run")
	extended, _, err := s.ContinueTask(ctx, extendEvent.ID, "extend-first", firstRun.TaskID, firstRun.Revision, "Continue with more time", true)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, extendEvent, TurnOutput{TaskID: firstRun.TaskID, Silent: true}, nil)
	if extended.BudgetMS <= budget || extended.Status != TaskQueued {
		t.Fatalf("first task budget was not extended: %+v", extended)
	}
	current, err = s.Schedule(ctx, schedule.ID)
	if err != nil || current.TaskBudgetMS != budget {
		t.Fatalf("extension changed schedule budget: (%+v,%v)", current, err)
	}
	secondTask, err = s.Task(ctx, secondRun.TaskID)
	if err != nil || secondTask.BudgetMS != budget {
		t.Fatalf("extension propagated to the next task: (%+v,%v)", secondTask, err)
	}
}

func TestScheduleManualPauseAfterStartPreservesTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	schedule := dispatchTestSchedule(t, s, WindowNone)
	occurrence, run := dispatchTestRun(t, s, *schedule.NextRunAt)
	started := schedule.NextRunAt.Add(2 * time.Minute)
	if _, err := s.BeginScheduledRun(ctx, run.ID, started); err != nil {
		t.Fatal(err)
	}

	pauseEvent := openUserEvent(t, s, 2, 2, "Pause after the scheduled task started")
	status := SchedulePaused
	paused, _, err := s.UpdateSchedule(ctx, pauseEvent.ID, "pause-after-start", schedule.ID, schedule.Revision,
		SchedulePatch{Status: &status}, started.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, pauseEvent, TurnOutput{ScheduleID: schedule.ID}, nil)
	task, err := s.Task(ctx, run.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	currentRun, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.ScheduleForTask(ctx, run.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != SchedulePaused || paused.NextRunAt != nil || task.Status != TaskRunning || currentRun.Status != RunRunning ||
		info == nil || info.Occurrence.ID != occurrence.ID || info.Occurrence.StartedAt == nil || !info.Occurrence.StartedAt.Equal(started) {
		t.Fatalf("pause cancelled or rewrote started work: schedule=%+v task=%+v run=%+v occurrence=%+v", paused, task, currentRun, info)
	}
	if next, err := s.DispatchDueSchedule(ctx, started.Add(48*time.Hour)); err != nil || next != nil {
		t.Fatalf("paused plan dispatched again: (%+v,%v)", next, err)
	}
}

func TestScheduleInvalidConfigurationAutoPausesOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	schedule := dispatchTestSchedule(t, s, WindowNone)
	if _, err := s.db.ExecContext(ctx, `UPDATE personal_schedules SET cron='not a cron expression' WHERE id=?`, schedule.ID); err != nil {
		t.Fatal(err)
	}

	first, err := s.DispatchDueSchedule(ctx, *schedule.NextRunAt)
	if err != nil || first == nil || first.Outcome != "blocked" || first.Reason != "invalid_schedule" {
		t.Fatalf("invalid schedule dispatch = (%+v,%v)", first, err)
	}
	current, err := s.Schedule(ctx, schedule.ID)
	if err != nil || current.Status != SchedulePaused || current.PauseReason != "invalid_schedule" || current.NextRunAt != nil {
		t.Fatalf("invalid schedule was not paused: (%+v,%v)", current, err)
	}

	if next, err := s.DispatchDueSchedule(ctx, schedule.NextRunAt.Add(24*time.Hour)); err != nil || next != nil {
		t.Fatalf("invalid schedule was retried: (%+v,%v)", next, err)
	}
	history, err := s.ScheduleOccurrences(ctx, schedule.ID, 0, 0)
	if err != nil || len(history) != 1 || history[0].Reason != "invalid_schedule" {
		t.Fatalf("configuration fault history = (%+v,%v)", history, err)
	}
	var notices int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id WHERE e.schedule_id=?`, schedule.ID).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("automatic pause notifications = %d, %v; want one", notices, err)
	}
}

func failScheduledRun(t *testing.T, s *Store, now time.Time) (ScheduleOccurrence, Run) {
	t.Helper()
	ctx := context.Background()
	occurrence, run := dispatchTestRun(t, s, now)
	if _, err := s.BeginScheduledRun(ctx, run.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Partial result"}, "Source request failed"); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(s)
	runtime.Think = func(ctx context.Context, input TurnInput) (TurnOutput, error) {
		if input.Event.Kind != EventTask || input.Event.TaskID != run.TaskID || input.CurrentRun == nil {
			t.Fatalf("unexpected failure callback: event=%+v run=%+v", input.Event, input.CurrentRun)
		}
		_, err := s.SettleTask(ctx, input.Event.ID, input.Event.TaskID, input.CurrentRun.Revision, TaskFailed, "Source request failed")
		return TurnOutput{TaskID: input.Event.TaskID, Silent: true}, err
	}
	worked, err := runtime.turn(ctx)
	if err != nil || !worked {
		t.Fatalf("runtime did not settle and notify failed run: worked=%t err=%v", worked, err)
	}
	return occurrence, run
}

func TestScheduleEveryFailureRunGetsItsOwnNotification(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	schedule := dispatchTestSchedule(t, s, WindowNone)
	firstOccurrence, firstRun := failScheduledRun(t, s, *schedule.NextRunAt)

	current, err := s.Schedule(ctx, schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondOccurrence, secondRun := failScheduledRun(t, s, *current.NextRunAt)
	if firstOccurrence.TaskID == secondOccurrence.TaskID || firstRun.TaskID == secondRun.TaskID {
		t.Fatal("separate periods reused a Task")
	}
	current, err = s.Schedule(ctx, schedule.ID)
	if err != nil || current.Status != ScheduleActive {
		t.Fatalf("single-run failures paused the plan: (%+v,%v)", current, err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT e.task_id,d.text FROM personal_outbox d JOIN personal_events e ON e.id=d.event_id
		WHERE e.task_id IN (?,?) ORDER BY e.task_id`, firstRun.TaskID, secondRun.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	notified := map[int64]string{}
	for rows.Next() {
		var taskID int64
		var text string
		if err := rows.Scan(&taskID, &text); err != nil {
			t.Fatal(err)
		}
		notified[taskID] += text
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(notified) != 2 || !strings.Contains(notified[firstRun.TaskID], "Source request failed") || !strings.Contains(notified[secondRun.TaskID], "Source request failed") {
		t.Fatalf("failure notifications were missing or deduplicated: %v", notified)
	}
}
