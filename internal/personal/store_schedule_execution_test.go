package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var dispatchTestNow = time.Date(2026, 11, 4, 7, 30, 0, 0, time.UTC)

func dispatchTestSchedule(t *testing.T, s *Store, mode WindowMode) Schedule {
	t.Helper()
	e := openUserEvent(t, s, 1, 1, "Register an hourly plan")
	input := ScheduleInput{Title: "hourly", Goal: "Check the feed", Context: "Only the requested feed", Cron: "0 * * * *", Timezone: "UTC", WindowMode: mode}
	if mode == WindowSinceSuccess {
		input.RequiredSources = []SourceRequirement{{ID: "feed", Label: "Feed"}}
	}
	p, _, err := s.CreateSchedule(context.Background(), e.ID, "plan", input, dispatchTestNow)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, e, TurnOutput{ScheduleID: p.ID}, nil)
	return p
}

func dispatchTestRun(t *testing.T, s *Store, now time.Time) (ScheduleOccurrence, Run) {
	t.Helper()
	ctx := context.Background()
	o, err := s.DispatchDueSchedule(ctx, now)
	if err != nil || o == nil || o.Outcome != "enqueued" {
		t.Fatalf("dispatch=%+v err=%v", o, err)
	}
	r, err := s.ClaimRun(ctx, RunTask)
	if err != nil || r == nil {
		t.Fatalf("claim=%+v err=%v", r, err)
	}
	return *o, *r
}

func dispatchTestFinish(t *testing.T, s *Store, r Run) Event {
	t.Helper()
	ctx := context.Background()
	if err := s.FinishStage(ctx, r.ID, StageResult{Summary: "Saved check results"}, ""); err != nil {
		t.Fatal(err)
	}
	e, err := s.ClaimEvent(ctx)
	if err != nil || e == nil || e.Kind != EventTask {
		t.Fatalf("callback=%+v err=%v", e, err)
	}
	return *e
}

func TestScheduleDispatchAuthorizationCoalescingAndBusy(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	e := openUserEvent(t, s, 1, 1, "Register hourly")
	p, _, err := s.CreateSchedule(ctx, e.ID, "plan", ScheduleInput{Title: "hourly", Goal: "Produce an exercise", Cron: "0 * * * *", Timezone: "UTC", WindowMode: WindowNone}, dispatchTestNow)
	if err != nil {
		t.Fatal(err)
	}
	late := p.NextRunAt.Add(72*time.Hour + 15*time.Minute)
	if o, err := s.DispatchDueSchedule(ctx, late); err != nil || o != nil {
		t.Fatalf("uncompleted user dispatched=%+v %v", o, err)
	}
	finishEvent(t, s, e, TurnOutput{}, nil)
	o, r := dispatchTestRun(t, s, late)
	if o.CoalescedUntil == nil || !o.CoalescedUntil.Equal(late) || !o.ScheduledAt.Equal(*p.NextRunAt) {
		t.Fatalf("coalesced=%+v", o)
	}
	if again, err := s.DispatchDueSchedule(ctx, late); err != nil || again != nil {
		t.Fatalf("duplicate=%+v %v", again, err)
	}
	next, err := s.Schedule(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	busy, err := s.DispatchDueSchedule(ctx, *next.NextRunAt)
	if err != nil || busy == nil || busy.Outcome != "skipped_busy" || busy.TaskID != 0 {
		t.Fatalf("busy=%+v %v", busy, err)
	}
	if r.TaskID != o.TaskID {
		t.Fatal("wrong task")
	}
	var events, tasks, runs int
	for query, count := range map[string]*int{`SELECT COUNT(*) FROM personal_events WHERE kind='schedule'`: &events, `SELECT COUNT(*) FROM personal_tasks`: &tasks, `SELECT COUNT(*) FROM personal_runs WHERE kind='task'`: &runs} {
		if err := s.db.QueryRow(query).Scan(count); err != nil {
			t.Fatal(err)
		}
	}
	if events != 1 || tasks != 1 || runs != 1 {
		t.Fatalf("catchup fanout: %d %d %d", events, tasks, runs)
	}
}

func TestScheduleCancelBoundaryAndRestartedStartedTask(t *testing.T) {
	for _, begin := range []bool{false, true} {
		t.Run(fmt.Sprint(begin), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			p := dispatchTestSchedule(t, s, WindowNone)
			_, r := dispatchTestRun(t, s, *p.NextRunAt)
			start := p.NextRunAt.Add(2 * time.Hour)
			if begin {
				got, err := s.BeginScheduledRun(ctx, r.ID, start)
				if err != nil || got == nil {
					t.Fatalf("begin=%+v %v", got, err)
				}
				if err := s.Recover(ctx); err != nil {
					t.Fatal(err)
				}
			}
			e := openUserEvent(t, s, 2, 2, "Cancel the plan")
			if _, _, err := s.CancelSchedule(ctx, e.ID, "cancel", p.ID, p.Revision, start.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, e, TurnOutput{}, nil)
			if begin {
				claimed, err := s.ClaimRun(ctx, RunTask)
				if err != nil || claimed == nil {
					t.Fatalf("reclaim=%+v %v", claimed, err)
				}
				got, err := s.BeginScheduledRun(ctx, r.ID, start.Add(24*time.Hour))
				if err != nil || got == nil {
					t.Fatalf("restart=%+v %v", got, err)
				}
				input, err := decodeTaskInput(got.Input)
				if err != nil {
					t.Fatal(err)
				}
				if input.Trigger == nil || !input.Trigger.StartedAt.Equal(start) || input.Trigger.Window != nil {
					t.Fatalf("moved first start=%+v", input.Trigger)
				}
			} else {
				got, err := s.BeginScheduledRun(ctx, r.ID, start)
				if err != nil || got != nil {
					t.Fatalf("cancelled began=%+v %v", got, err)
				}
				task, _ := s.Task(ctx, r.TaskID)
				if task.Status != TaskCancelled {
					t.Fatalf("task=%+v", task)
				}
			}
		})
	}
}

func TestScheduledCheckFrozenWindowAndAtomicCredit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	p := dispatchTestSchedule(t, s, WindowSinceSuccess)
	_, r := dispatchTestRun(t, s, p.NextRunAt.Add(72*time.Hour))
	start := p.NextRunAt.Add(74 * time.Hour)
	run, err := s.BeginScheduledRun(ctx, r.ID, start)
	if err != nil {
		t.Fatal(err)
	}
	input, err := decodeTaskInput(run.Input)
	if err != nil {
		t.Fatal(err)
	}
	if input.Trigger == nil || !input.Trigger.Window.Start.Equal(*p.BaselineAt) || !input.Trigger.Window.End.Equal(start) {
		t.Fatalf("window=%+v", input.Trigger)
	}
	e := dispatchTestFinish(t, s, *run)
	if e.ScheduleID != p.ID {
		t.Fatalf("callback schedule lost=%+v", e)
	}
	bad := &CheckAssessment{Complete: true, RequiredSources: []SourceAssessment{{ID: "wrong", Complete: true, Evidence: "looked"}}}
	if _, err := s.SettleTask(ctx, e.ID, r.TaskID, 1, TaskCompleted, "done", bad); err == nil {
		t.Fatal("mismatched assessment accepted")
	}
	state, _ := s.Task(ctx, r.TaskID)
	got, _ := s.Schedule(ctx, p.ID)
	if state.Status != TaskAwaitingReview || got.CoveredThrough != nil {
		t.Fatal("rejected assessment changed state")
	}
	good := &CheckAssessment{Complete: true, RequiredSources: []SourceAssessment{{ID: "feed", Complete: true, Evidence: "All history in frozen window checked"}}}
	if _, err := s.SettleTask(ctx, e.ID, r.TaskID, 1, TaskCompleted, "done", good); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Schedule(ctx, p.ID)
	if got.CoveredThrough == nil || !got.CoveredThrough.Equal(start) {
		t.Fatalf("credit=%+v", got)
	}
	finishEvent(t, s, e, TurnOutput{TaskID: r.TaskID, Text: "done"}, []string{"done"})
	if _, err := s.db.Exec(`UPDATE personal_outbox SET status='failed'`); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Schedule(ctx, p.ID)
	if !got.CoveredThrough.Equal(start) {
		t.Fatal("delivery failure changed credit")
	}
	info, _ := s.ScheduleForTask(ctx, r.TaskID)
	if info.Occurrence.CreditedTaskRevision != 1 || info.Occurrence.CreditedAt == nil {
		t.Fatalf("receipt=%+v", info)
	}
}

func TestScheduleOldScopeAndRevisedTaskCannotCredit(t *testing.T) {
	for _, revise := range []bool{false, true} {
		t.Run(fmt.Sprint(revise), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			p := dispatchTestSchedule(t, s, WindowSinceSuccess)
			_, r := dispatchTestRun(t, s, *p.NextRunAt)
			if _, err := s.BeginScheduledRun(ctx, r.ID, p.NextRunAt.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			e := openUserEvent(t, s, 2, 2, "Change the goal")
			if revise {
				_, _, err := s.ReviseTask(ctx, e.ID, "revise", r.TaskID, 1, TaskInput{Goal: "New single task goal"})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				goal := "New plan goal"
				_, _, err := s.UpdateSchedule(ctx, e.ID, "update", p.ID, p.Revision, SchedulePatch{Goal: &goal}, p.NextRunAt.Add(2*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
			}
			finishEvent(t, s, e, TurnOutput{}, nil)
			if revise {
				run, err := s.ClaimRun(ctx, RunTask)
				if err != nil || run == nil {
					t.Fatalf("claim=%+v %v", run, err)
				}
				if _, err := s.BeginScheduledRun(ctx, run.ID, p.NextRunAt.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				r = *run
			}
			callback := dispatchTestFinish(t, s, r)
			var check *CheckAssessment
			if !revise {
				check = &CheckAssessment{Complete: true, RequiredSources: []SourceAssessment{{ID: "feed", Complete: true, Evidence: "old goal checked"}}}
			}
			if _, err := s.SettleTask(ctx, callback.ID, r.TaskID, r.Revision, TaskCompleted, "done", check); err != nil {
				t.Fatal(err)
			}
			got, _ := s.Schedule(ctx, p.ID)
			if got.CoveredThrough != nil {
				t.Fatalf("old scope credited=%+v", got)
			}
		})
	}
}

func TestDispatchRollsBackAllRecords(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	p := dispatchTestSchedule(t, s, WindowNone)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_occurrence BEFORE INSERT ON personal_schedule_occurrences BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DispatchDueSchedule(ctx, *p.NextRunAt); err == nil {
		t.Fatal("injected failure accepted")
	}
	for _, table := range []string{"personal_tasks", "personal_schedule_occurrences"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial %s=%d %v", table, n, err)
		}
	}
	got, err := s.Schedule(ctx, p.ID)
	if err != nil || !got.NextRunAt.Equal(*p.NextRunAt) {
		t.Fatalf("next changed=%+v %v", got, err)
	}
}

func TestTaskTriggerIsProgramOwned(t *testing.T) {
	s := testStore(t, ":memory:", time.Minute)
	e := openUserEvent(t, s, 1, 1, "New task")
	_, _, err := s.CreateTask(context.Background(), e.ID, "fake", "title", TaskInput{Goal: "goal", Trigger: &ScheduleTrigger{ScheduleID: 99}})
	if err == nil {
		t.Fatal("ordinary API accepted a forged trigger")
	}
	raw, _ := json.Marshal(TaskInput{Goal: "goal", Trigger: &ScheduleTrigger{StartedAt: &dispatchTestNow}})
	if _, err := decodeTaskInput(string(raw)); err != nil {
		t.Fatalf("executor rejected legitimate trigger: %v", err)
	}
}

func TestScheduleWaitingInputStopsBlockingAfterCallbackCompletes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	p := dispatchTestSchedule(t, s, WindowNone)
	_, r := dispatchTestRun(t, s, *p.NextRunAt)
	if _, err := s.BeginScheduledRun(ctx, r.ID, p.NextRunAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	e := dispatchTestFinish(t, s, r)
	if _, err := s.SettleTask(ctx, e.ID, r.TaskID, 1, TaskWaitingInput, "Need a selection"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Schedule(ctx, p.ID)
	o, err := s.DispatchDueSchedule(ctx, *got.NextRunAt)
	if err != nil || o == nil || o.Outcome != "skipped_busy" {
		t.Fatalf("active callback no longer blocked: %+v %v", o, err)
	}
	finishEvent(t, s, e, TurnOutput{TaskID: r.TaskID, Text: "Please choose"}, []string{"Please choose"})
	got, _ = s.Schedule(ctx, p.ID)
	o, err = s.DispatchDueSchedule(ctx, *got.NextRunAt)
	if err != nil || o == nil || o.Outcome != "enqueued" || o.TaskID == r.TaskID {
		t.Fatalf("stable waiting blocked: %+v %v", o, err)
	}
}

func TestScheduledCheckCreditRollsBackWithSettlement(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	p := dispatchTestSchedule(t, s, WindowSinceSuccess)
	_, r := dispatchTestRun(t, s, *p.NextRunAt)
	if _, err := s.BeginScheduledRun(ctx, r.ID, p.NextRunAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	e := dispatchTestFinish(t, s, r)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_settle BEFORE UPDATE ON personal_tasks WHEN NEW.status='completed' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	check := &CheckAssessment{Complete: true, RequiredSources: []SourceAssessment{{ID: "feed", Complete: true, Evidence: "checked"}}}
	if _, err := s.SettleTask(ctx, e.ID, r.TaskID, 1, TaskCompleted, "done", check); err == nil {
		t.Fatal("injected failure accepted")
	}
	info, err := s.ScheduleForTask(ctx, r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.Task(ctx, r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskAwaitingReview || info.Schedule.CoveredThrough != nil || info.Occurrence.CheckResult != nil || info.Occurrence.CreditedAt != nil {
		t.Fatalf("partial credit: %+v %+v", task, info)
	}
}

func TestScheduleMissingAttachmentAtBeginPausesOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	f, err := s.SaveAttachment(ctx, "context.txt", "text/plain", strings.NewReader("background"))
	if err != nil {
		t.Fatal(err)
	}
	e := openUserEvent(t, s, 1, 1, "Register plan with attachment")
	p, _, err := s.CreateSchedule(ctx, e.ID, "plan", ScheduleInput{Title: "plan", Goal: "read file", Attachments: []string{f.ID}, Cron: "0 * * * *", Timezone: "UTC"}, dispatchTestNow)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, e, TurnOutput{}, nil)
	_, r := dispatchTestRun(t, s, *p.NextRunAt)
	if err := s.DeleteAttachment(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if run, err := s.BeginScheduledRun(ctx, r.ID, p.NextRunAt.Add(time.Minute)); err != nil || run != nil {
		t.Fatalf("missing attachment began: %+v %v", run, err)
	}
	got, err := s.Schedule(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != SchedulePaused || got.PauseReason != "attachment_unavailable" {
		t.Fatalf("not paused: %+v", got)
	}
	if o, err := s.DispatchDueSchedule(ctx, p.NextRunAt.Add(24*time.Hour)); err != nil || o != nil {
		t.Fatalf("paused plan dispatched: %+v %v", o, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_outbox`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pause notices=%d %v", n, err)
	}
}

func TestScheduleSchemaRejectsVersionTwoWithoutChangingOldData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE personal_schema(version INTEGER PRIMARY KEY CHECK(version=2)); INSERT INTO personal_schema VALUES(2); CREATE TABLE personal_kept(value TEXT); INSERT INTO personal_kept VALUES('keep me')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := NewStore(path, time.Minute); err == nil {
		s.Close()
		t.Fatal("schema 2 accepted")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var kept string
	if err := db.QueryRow(`SELECT value FROM personal_kept`).Scan(&kept); err != nil || kept != "keep me" {
		t.Fatalf("old data=%q %v", kept, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='personal_schedules'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old db changed=%d %v", n, err)
	}
}
