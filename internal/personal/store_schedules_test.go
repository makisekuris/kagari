package personal

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func scheduleTestNow() time.Time {
	return time.Date(2026, 11, 6, 10, 0, 30, 0, time.UTC)
}

func scheduleTestInput(mode WindowMode) ScheduleInput {
	return ScheduleInput{
		Title:           "Morning check",
		Goal:            "Review the new release notes",
		Context:         "Focus on compatibility changes",
		Cron:            "* * * * *",
		Timezone:        "UTC",
		WindowMode:      mode,
		RequiredSources: []SourceRequirement{{ID: "release-notes", Label: "Release notes", URL: "https://example.com/releases"}},
	}
}

func createScheduleForTest(t *testing.T, s *Store, updateID, messageID int64, key string, input ScheduleInput, now time.Time) (Event, Schedule) {
	t.Helper()
	event := openUserEvent(t, s, updateID, messageID, "Create a recurring schedule")
	schedule, replayed, err := s.CreateSchedule(context.Background(), event.ID, key, input, now)
	if err != nil || replayed {
		t.Fatalf("CreateSchedule() = (%+v,%v,%v)", schedule, replayed, err)
	}
	return event, schedule
}

func completeScheduleUserEvent(t *testing.T, s *Store, event Event) {
	t.Helper()
	finishEvent(t, s, event, TurnOutput{Silent: true}, nil)
}

func setScheduleCoverageForTest(t *testing.T, s *Store, schedule Schedule, through time.Time) {
	t.Helper()
	if schedule.BaselineAt == nil || through.Before(*schedule.BaselineAt) {
		t.Fatalf("test coverage %s is before baseline %v", through, schedule.BaselineAt)
	}
	if _, err := s.db.Exec(`UPDATE personal_schedules SET covered_through=? WHERE id=?`, timestamp(through), schedule.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulePauseResumeStartsNewCoverageScope(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	createEvent, schedule := createScheduleForTest(t, s, 1, 101, "create", scheduleTestInput(WindowSinceSuccess), now)
	completeScheduleUserEvent(t, s, createEvent)
	covered := schedule.BaselineAt.Add(30 * time.Second)
	setScheduleCoverageForTest(t, s, schedule, covered)

	pauseEvent := openUserEvent(t, s, 2, 102, "Pause the schedule")
	pausedStatus := SchedulePaused
	paused, replayed, err := s.UpdateSchedule(ctx, pauseEvent.ID, "pause", schedule.ID, schedule.Revision,
		SchedulePatch{Status: &pausedStatus}, now.Add(time.Minute))
	if err != nil || replayed {
		t.Fatalf("pause = (%+v,%v,%v)", paused, replayed, err)
	}
	if paused.Status != SchedulePaused || paused.PauseReason != "manual" || paused.NextRunAt != nil ||
		paused.ScopeRevision != schedule.ScopeRevision || paused.CoveredThrough == nil || !paused.CoveredThrough.Equal(covered) {
		t.Fatalf("manual pause changed schedule progress: %+v", paused)
	}
	completeScheduleUserEvent(t, s, pauseEvent)

	resumeEvent := openUserEvent(t, s, 3, 103, "Resume the schedule")
	activeStatus := ScheduleActive
	resumeAt := now.Add(3 * time.Hour)
	resumed, replayed, err := s.UpdateSchedule(ctx, resumeEvent.ID, "resume", schedule.ID, paused.Revision,
		SchedulePatch{Status: &activeStatus}, resumeAt)
	if err != nil || replayed {
		t.Fatalf("resume = (%+v,%v,%v)", resumed, replayed, err)
	}
	if resumed.Status != ScheduleActive || resumed.PauseReason != "" || resumed.NextRunAt == nil || !resumed.NextRunAt.After(resumeAt) ||
		resumed.ScopeRevision != paused.ScopeRevision+1 || resumed.BaselineAt == nil || !resumed.BaselineAt.Equal(resumeAt) || resumed.CoveredThrough != nil {
		t.Fatalf("manual resume did not start a fresh coverage scope: %+v", resumed)
	}
}

func TestScheduleRenameAndTimeChangePreserveCoverage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	event, schedule := createScheduleForTest(t, s, 1, 201, "create", scheduleTestInput(WindowSinceSuccess), now)
	covered := schedule.BaselineAt.Add(20 * time.Second)
	setScheduleCoverageForTest(t, s, schedule, covered)
	newTitle := "Hourly release review"
	newCron := "0 * * * *"
	newTimezone := "Asia/Tokyo"
	newBudget := int64(3 * time.Minute / time.Millisecond)
	updated, replayed, err := s.UpdateSchedule(ctx, event.ID, "rename-time", schedule.ID, schedule.Revision,
		SchedulePatch{Title: &newTitle, Cron: &newCron, Timezone: &newTimezone, TaskBudgetMS: &newBudget}, now.Add(10*time.Second))
	if err != nil || replayed {
		t.Fatalf("UpdateSchedule() = (%+v,%v,%v)", updated, replayed, err)
	}
	if updated.Title != newTitle || updated.Cron != newCron || updated.Timezone != newTimezone || updated.TaskBudgetMS != newBudget ||
		updated.ScopeRevision != schedule.ScopeRevision || updated.BaselineAt == nil || !updated.BaselineAt.Equal(*schedule.BaselineAt) ||
		updated.CoveredThrough == nil || !updated.CoveredThrough.Equal(covered) || updated.NextRunAt == nil || !updated.NextRunAt.After(now) {
		t.Fatalf("rename/time update changed coverage or failed to move next run: %+v", updated)
	}
	if want, err := nextScheduleTime(newCron, newTimezone, now.Add(10*time.Second)); err != nil || !updated.NextRunAt.Equal(want) {
		t.Fatalf("next run = %v, want %v (%v)", updated.NextRunAt, want, err)
	}
	if updated.Revision != schedule.Revision+1 {
		t.Fatalf("revision = %d, want %d", updated.Revision, schedule.Revision+1)
	}
}

func TestScheduleContentChangeResetsCoverageAndCancelsUnstartedWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	event, schedule := createScheduleForTest(t, s, 1, 301, "create", scheduleTestInput(WindowSinceSuccess), now)
	completeScheduleUserEvent(t, s, event)
	covered := schedule.BaselineAt.Add(20 * time.Second)
	setScheduleCoverageForTest(t, s, schedule, covered)
	if _, err := s.db.Exec(`UPDATE personal_schedules SET next_run_at=? WHERE id=?`, timestamp(now), schedule.ID); err != nil {
		t.Fatal(err)
	}
	occurrence, err := s.DispatchDueSchedule(ctx, now)
	if err != nil || occurrence == nil || occurrence.TaskID == 0 {
		t.Fatalf("DispatchDueSchedule() = (%+v,%v)", occurrence, err)
	}

	changeEvent := openUserEvent(t, s, 2, 302, "Change the scheduled goal")
	newGoal := "Review security release notes"
	updated, replayed, err := s.UpdateSchedule(ctx, changeEvent.ID, "change-goal", schedule.ID, schedule.Revision,
		SchedulePatch{Goal: &newGoal}, now.Add(10*time.Second))
	if err != nil || replayed {
		t.Fatalf("UpdateSchedule() = (%+v,%v,%v)", updated, replayed, err)
	}
	if updated.Goal != newGoal || updated.Revision != schedule.Revision+1 || updated.ScopeRevision != schedule.ScopeRevision+1 ||
		updated.CoveredThrough != nil || updated.BaselineAt == nil || updated.NextRunAt == nil || !updated.NextRunAt.After(now.Add(10*time.Second)) {
		t.Fatalf("content change did not establish new progress: %+v", updated)
	}
	var taskStatus, runStatus string
	if err := s.db.QueryRow(`SELECT status FROM personal_tasks WHERE id=?`, occurrence.TaskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT status FROM personal_runs WHERE task_id=? AND kind='task'`, occurrence.TaskID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(TaskCancelled) || runStatus != string(RunCancelled) {
		t.Fatalf("unstarted task/run statuses = %q/%q, want cancelled/cancelled", taskStatus, runStatus)
	}
}

func TestScheduleCombinedContentChangeAndManualResumeUsesOneNewScope(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	event, schedule := createScheduleForTest(t, s, 1, 401, "create", scheduleTestInput(WindowSinceSuccess), now)
	covered := schedule.BaselineAt.Add(20 * time.Second)
	setScheduleCoverageForTest(t, s, schedule, covered)
	if _, err := s.db.Exec(`UPDATE personal_schedules SET status='paused',pause_reason='manual',next_run_at=NULL WHERE id=?`, schedule.ID); err != nil {
		t.Fatal(err)
	}
	newGoal := "Check the migration notes"
	activeStatus := ScheduleActive
	resumeAt := now.Add(2 * time.Hour)
	updated, replayed, err := s.UpdateSchedule(ctx, event.ID, "change-and-resume", schedule.ID, schedule.Revision,
		SchedulePatch{Goal: &newGoal, Status: &activeStatus}, resumeAt)
	if err != nil || replayed {
		t.Fatalf("combined update = (%+v,%v,%v)", updated, replayed, err)
	}
	if updated.Status != ScheduleActive || updated.ScopeRevision != schedule.ScopeRevision+1 || updated.Revision != schedule.Revision+1 ||
		updated.BaselineAt == nil || !updated.BaselineAt.Equal(resumeAt) || updated.CoveredThrough != nil ||
		updated.NextRunAt == nil || !updated.NextRunAt.After(resumeAt) {
		t.Fatalf("combined change and resume did not apply one fresh scope: %+v", updated)
	}
}

func TestScheduleFaultRecoveryPreservesCoverage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	event, schedule := createScheduleForTest(t, s, 1, 451, "create", scheduleTestInput(WindowSinceSuccess), now)
	completeScheduleUserEvent(t, s, event)
	covered := schedule.BaselineAt.Add(20 * time.Second)
	setScheduleCoverageForTest(t, s, schedule, covered)
	if _, err := s.db.Exec(`UPDATE personal_schedules SET status='paused',pause_reason='attachment_unavailable',next_run_at=NULL,revision=revision+1 WHERE id=?`, schedule.ID); err != nil {
		t.Fatal(err)
	}

	resumeEvent := openUserEvent(t, s, 2, 452, "Recover the schedule")
	activeStatus := ScheduleActive
	resumeAt := now.Add(2 * time.Hour)
	resumed, replayed, err := s.UpdateSchedule(ctx, resumeEvent.ID, "recover", schedule.ID, schedule.Revision+1,
		SchedulePatch{Status: &activeStatus}, resumeAt)
	if err != nil || replayed {
		t.Fatalf("fault resume = (%+v,%v,%v)", resumed, replayed, err)
	}
	if resumed.ScopeRevision != schedule.ScopeRevision || resumed.BaselineAt == nil || !resumed.BaselineAt.Equal(*schedule.BaselineAt) ||
		resumed.CoveredThrough == nil || !resumed.CoveredThrough.Equal(covered) || resumed.NextRunAt == nil || !resumed.NextRunAt.After(resumeAt) {
		t.Fatalf("fault recovery discarded valid coverage: %+v", resumed)
	}
}

func makeScheduleDueForTest(t *testing.T, s *Store, schedule Schedule, event Event, now time.Time) *ScheduleOccurrence {
	t.Helper()
	completeScheduleUserEvent(t, s, event)
	if _, err := s.db.Exec(`UPDATE personal_schedules SET next_run_at=? WHERE id=?`, timestamp(now.Add(-time.Minute)), schedule.ID); err != nil {
		t.Fatal(err)
	}
	occurrence, err := s.DispatchDueSchedule(context.Background(), now)
	if err != nil || occurrence == nil || occurrence.TaskID == 0 {
		t.Fatalf("DispatchDueSchedule() = (%+v,%v)", occurrence, err)
	}
	return occurrence
}

func TestCancelScheduleCancelsUnstartedTaskAndKeepsOccurrence(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	createEvent, schedule := createScheduleForTest(t, s, 1, 501, "create", scheduleTestInput(WindowNone), now)
	occurrence := makeScheduleDueForTest(t, s, schedule, createEvent, now.Add(time.Minute))
	cancelEvent := openUserEvent(t, s, 2, 502, "Cancel the schedule")
	cancelled, replayed, err := s.CancelSchedule(ctx, cancelEvent.ID, "cancel", schedule.ID, schedule.Revision, now.Add(2*time.Minute))
	if err != nil || replayed {
		t.Fatalf("CancelSchedule() = (%+v,%v,%v)", cancelled, replayed, err)
	}
	if cancelled.Status != ScheduleCancelled || cancelled.NextRunAt != nil || cancelled.PauseReason != "" || cancelled.Revision != schedule.Revision+1 {
		t.Fatalf("cancelled schedule = %+v", cancelled)
	}
	var taskStatus, runStatus string
	if err := s.db.QueryRow(`SELECT status FROM personal_tasks WHERE id=?`, occurrence.TaskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT status FROM personal_runs WHERE task_id=? AND kind='task'`, occurrence.TaskID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(TaskCancelled) || runStatus != string(RunCancelled) {
		t.Fatalf("cancelled task/run status = %q/%q", taskStatus, runStatus)
	}
	history, err := s.ScheduleOccurrences(ctx, schedule.ID, 0, 0)
	if err != nil || len(history) != 1 || history[0].ID != occurrence.ID || history[0].TaskStatus != TaskCancelled {
		t.Fatalf("cancelled occurrence history = (%+v,%v)", history, err)
	}
	scheduleContext, err := s.ScheduleForTask(ctx, occurrence.TaskID)
	if err != nil || scheduleContext == nil || scheduleContext.Schedule.ID != schedule.ID || scheduleContext.Occurrence.ID != occurrence.ID || scheduleContext.Occurrence.TaskStatus != TaskCancelled {
		t.Fatalf("ScheduleForTask() = (%+v,%v)", scheduleContext, err)
	}
}

func TestScheduleRequestReplayBypassesDeletedAttachments(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	attachment, err := s.SaveAttachment(ctx, "brief.txt", "text/plain", bytes.NewBufferString("release brief"))
	if err != nil {
		t.Fatal(err)
	}
	input := scheduleTestInput(WindowNone)
	input.Attachments = []string{attachment.ID}
	event := openUserEvent(t, s, 1, 601, "Create a schedule with a saved brief")
	created, replayed, err := s.CreateSchedule(ctx, event.ID, "create", input, now)
	if err != nil || replayed || len(created.Attachments) != 1 || created.Attachments[0].Name != attachment.Name {
		t.Fatalf("CreateSchedule() = (%+v,%v,%v)", created, replayed, err)
	}
	completeScheduleUserEvent(t, s, event)
	if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
		t.Fatal(err)
	}
	replayedSchedule, wasReplay, err := s.CreateSchedule(ctx, event.ID, "create", input, now.Add(time.Hour))
	if err != nil || !wasReplay || replayedSchedule.ID != created.ID || replayedSchedule.CreatedAt != created.CreatedAt || len(replayedSchedule.Attachments) != 1 {
		t.Fatalf("replayed CreateSchedule() = (%+v,%v,%v)", replayedSchedule, wasReplay, err)
	}
}

func TestScheduleMutationsRequireCurrentUserAuthorization(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	completedEvent, schedule := createScheduleForTest(t, s, 1, 701, "create", scheduleTestInput(WindowNone), now)
	completeScheduleUserEvent(t, s, completedEvent)
	newTitle := "Unauthorized rename"
	if _, _, err := s.UpdateSchedule(ctx, completedEvent.ID, "new-request", schedule.ID, schedule.Revision, SchedulePatch{Title: &newTitle}, now.Add(time.Minute)); err == nil {
		t.Fatal("new mutation reused a completed user event")
	}

	result, err := s.db.Exec(`INSERT INTO personal_events(kind,text,status,created_at) VALUES('material','attached document','running',?)`, timestamp(now))
	if err != nil {
		t.Fatal(err)
	}
	materialEventID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateSchedule(ctx, materialEventID, "material-create", scheduleTestInput(WindowNone), now); err == nil {
		t.Fatal("material event authorized schedule creation")
	}
	if got, err := s.Schedule(ctx, schedule.ID); err != nil || got == nil || got.Revision != schedule.Revision || got.Title != schedule.Title {
		t.Fatalf("unauthorized request changed schedule: (%+v,%v)", got, err)
	}
}

func TestCancelScheduleRollsBackAllChangesWhenTaskCancellationFails(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	createEvent, schedule := createScheduleForTest(t, s, 1, 801, "create", scheduleTestInput(WindowNone), now)
	occurrence := makeScheduleDueForTest(t, s, schedule, createEvent, now.Add(time.Minute))
	if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER reject_task_cancel BEFORE UPDATE OF status ON personal_tasks WHEN OLD.id=%d AND NEW.status='cancelled' BEGIN SELECT RAISE(ABORT,'forced cancel failure'); END`, occurrence.TaskID)); err != nil {
		t.Fatal(err)
	}
	cancelEvent := openUserEvent(t, s, 2, 802, "Cancel the schedule")
	if _, _, err := s.CancelSchedule(ctx, cancelEvent.ID, "cancel", schedule.ID, schedule.Revision, now.Add(2*time.Minute)); err == nil {
		t.Fatal("CancelSchedule succeeded despite the task cancellation failure")
	}
	got, err := s.Schedule(ctx, schedule.ID)
	if err != nil || got == nil || got.Status != ScheduleActive || got.Revision != schedule.Revision {
		t.Fatalf("schedule state after rollback = (%+v,%v)", got, err)
	}
	var taskStatus, runStatus string
	if err := s.db.QueryRow(`SELECT status FROM personal_tasks WHERE id=?`, occurrence.TaskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT status FROM personal_runs WHERE task_id=? AND kind='task'`, occurrence.TaskID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(TaskQueued) || runStatus != string(RunQueued) {
		t.Fatalf("task/run state after rollback = %q/%q", taskStatus, runStatus)
	}
	var requests int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_requests WHERE event_id=? AND request_key='cancel'`, cancelEvent.ID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("failed cancellation saved %d idempotency receipts", requests)
	}
}

func TestScheduleQueriesUseDescendingIDPages(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	firstEvent, first := createScheduleForTest(t, s, 1, 901, "first", scheduleTestInput(WindowNone), now)
	secondEvent, second := createScheduleForTest(t, s, 2, 902, "second", scheduleTestInput(WindowNone), now.Add(time.Second))
	completeScheduleUserEvent(t, s, firstEvent)
	completeScheduleUserEvent(t, s, secondEvent)
	cancelEvent := openUserEvent(t, s, 3, 903, "Cancel the second schedule")
	if _, _, err := s.CancelSchedule(ctx, cancelEvent.ID, "cancel-second", second.ID, second.Revision, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	completeScheduleUserEvent(t, s, cancelEvent)
	items, err := s.Schedules(ctx, false, 0, 20)
	if err != nil || len(items) != 1 || items[0].ID != first.ID {
		t.Fatalf("active schedule list = (%+v,%v)", items, err)
	}
	items, err = s.Schedules(ctx, true, 0, 1)
	if err != nil || len(items) != 1 || items[0].ID != second.ID {
		t.Fatalf("first schedule page = (%+v,%v)", items, err)
	}
	items, err = s.Schedules(ctx, true, second.ID, 1)
	if err != nil || len(items) != 1 || items[0].ID != first.ID {
		t.Fatalf("second schedule page = (%+v,%v)", items, err)
	}
	if got, err := s.ScheduleForTask(ctx, 999); err != nil || got != nil {
		t.Fatalf("ScheduleForTask for ordinary task = (%+v,%v)", got, err)
	}
}

func TestPausedScheduleRejectsCalendarWithoutFutureActivation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	p := dispatchTestSchedule(t, s, WindowNone)
	e := openUserEvent(t, s, 2, 2, "Pause the plan")
	status := SchedulePaused
	paused, _, err := s.UpdateSchedule(ctx, e.ID, "pause", p.ID, p.Revision, SchedulePatch{Status: &status}, dispatchTestNow)
	if err != nil {
		t.Fatal(err)
	}
	cron := "0 0 31 2 *"
	if _, _, err := s.UpdateSchedule(ctx, e.ID, "invalid-calendar", p.ID, paused.Revision, SchedulePatch{Cron: &cron}, dispatchTestNow); err == nil {
		t.Fatal("paused plan accepted a calendar with no future activation")
	}
	current, err := s.Schedule(ctx, p.ID)
	if err != nil || current.Revision != paused.Revision || current.Cron != p.Cron {
		t.Fatalf("invalid calendar changed the plan: %+v %v", current, err)
	}
}

func TestScheduleValidationAndRequestKeyConflict(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := scheduleTestNow()
	event := openUserEvent(t, s, 1, 1001, "Create a schedule")
	input := scheduleTestInput(WindowNone)
	created, _, err := s.CreateSchedule(ctx, event.ID, "create", input, now)
	if err != nil {
		t.Fatal(err)
	}
	changed := input
	changed.Goal = "A different goal"
	if _, _, err := s.CreateSchedule(ctx, event.ID, "create", changed, now); err == nil {
		t.Fatal("request key reuse with different schedule input succeeded")
	}
	badBudget := int64(maxScheduleTaskBudgetMS + 1)
	if _, _, err := s.UpdateSchedule(ctx, event.ID, "large-budget", created.ID, created.Revision, SchedulePatch{TaskBudgetMS: &badBudget}, now); err == nil {
		t.Fatal("schedule budget beyond time.Duration range was accepted")
	}
	emptyTimezone := ""
	if _, _, err := s.UpdateSchedule(ctx, event.ID, "empty-zone", created.ID, created.Revision, SchedulePatch{Timezone: &emptyTimezone}, now); err == nil {
		t.Fatal("empty explicit timezone was accepted")
	}
	badSource := []SourceRequirement{{ID: "same", Label: "first"}, {ID: "same", Label: "second"}}
	if _, _, err := s.UpdateSchedule(ctx, event.ID, "duplicate-sources", created.ID, created.Revision, SchedulePatch{RequiredSources: &badSource}, now); err == nil {
		t.Fatal("duplicate required source IDs were accepted")
	}
	if _, err := s.ScheduleOccurrences(ctx, created.ID, -1, 10); err == nil {
		t.Fatal("negative occurrence cursor was accepted")
	}
}

func TestScheduleRejectsWebURLInSourceIDWithoutWriting(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1101, "Create a schedule")
	input := scheduleTestInput(WindowNone)
	input.RequiredSources = []SourceRequirement{{ID: "HTTPS://status.example.invalid/baihua", Label: "Baihua status"}}
	if _, _, err := s.CreateSchedule(ctx, event.ID, "bad-create", input, scheduleTestNow()); err == nil || !strings.Contains(err.Error(), "must be supplied in url, not only in id") {
		t.Fatalf("CreateSchedule with URL-shaped ID error = %v", err)
	}
	var schedules, creates int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_schedules`).Scan(&schedules); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_requests WHERE event_id=? AND operation='create_schedule'`, event.ID).Scan(&creates); err != nil {
		t.Fatal(err)
	}
	if schedules != 0 || creates != 0 {
		t.Fatalf("rejected create changed storage: schedules=%d requests=%d", schedules, creates)
	}

	valid := scheduleTestInput(WindowNone)
	schedule, replayed, err := s.CreateSchedule(ctx, event.ID, "valid-create", valid, scheduleTestNow())
	if err != nil || replayed {
		t.Fatalf("setup CreateSchedule = (%+v,%v,%v)", schedule, replayed, err)
	}
	badSources := []SourceRequirement{{ID: "https://status.example.invalid/baihua", Label: "Baihua status", URL: " "}}
	if _, _, err := s.UpdateSchedule(ctx, event.ID, "bad-update", schedule.ID, schedule.Revision, SchedulePatch{RequiredSources: &badSources}, scheduleTestNow()); err == nil || !strings.Contains(err.Error(), "must be supplied in url, not only in id") {
		t.Fatalf("UpdateSchedule with URL-shaped ID error = %v", err)
	}
	current, err := s.Schedule(ctx, schedule.ID)
	if err != nil || current == nil || current.Revision != schedule.Revision || len(current.RequiredSources) != 1 || current.RequiredSources[0] != valid.RequiredSources[0] {
		t.Fatalf("rejected update changed schedule: current=%+v error=%v", current, err)
	}
	var updates int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_requests WHERE event_id=? AND operation='update_schedule'`, event.ID).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if updates != 0 {
		t.Fatalf("rejected update wrote %d request rows", updates)
	}
}

func TestScheduleAcceptsWebAndNonWebRequiredSources(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	for i, source := range []SourceRequirement{
		{ID: "internal-feed", Label: "Internal feed"},
		{ID: "status-page", Label: "Status page", URL: "https://status.example.invalid/baihua"},
		{ID: "https://example.invalid/releases", Label: "Legacy release source", URL: "https://example.invalid/releases"},
	} {
		event := openUserEvent(t, s, int64(i+1), int64(i+1201), "Create a schedule")
		input := scheduleTestInput(WindowNone)
		input.RequiredSources = []SourceRequirement{source}
		schedule, replayed, err := s.CreateSchedule(ctx, event.ID, fmt.Sprintf("valid-%d", i), input, scheduleTestNow())
		if err != nil || replayed || len(schedule.RequiredSources) != 1 || schedule.RequiredSources[0] != source {
			t.Fatalf("CreateSchedule with source %+v = (%+v,%v,%v)", source, schedule, replayed, err)
		}
	}
}
