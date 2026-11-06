package personal

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func scheduleSuggestionFixture(t *testing.T, mode WindowMode) (*Store, Schedule, Memory) {
	t.Helper()
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "suggestions.db"), time.Minute)
	createEvent := openUserEvent(t, s, 1, 1, "Create a schedule with the Go project context")
	input := ScheduleInput{
		Title: "Project update", Goal: "Review the latest project update", Context: "Project uses Go.",
		Cron: "* * * * *", Timezone: "UTC", WindowMode: mode,
	}
	if mode == WindowSinceSuccess {
		input.RequiredSources = []SourceRequirement{{ID: "feed", Label: "Project feed"}}
	}
	schedule, _, err := s.CreateSchedule(ctx, createEvent.ID, "create-schedule", input, scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, createEvent, TurnOutput{ScheduleID: schedule.ID}, nil)
	memoryEvent := openUserEvent(t, s, 2, 2, "Remember that the current project uses Rust.")
	memory, err := s.WriteMemory(ctx, memoryEvent.ID, "remember-language", MemoryChange{
		Operation: "put", Key: "project.language", Value: "Rust", Evidence: "Rust",
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, memoryEvent, TurnOutput{Text: "Memory saved."}, []string{"Memory saved."})
	return s, schedule, memory
}

func scheduleSuggestionCallback(t *testing.T, s *Store, scheduleID int64) Event {
	t.Helper()
	schedule, err := s.Schedule(context.Background(), scheduleID)
	if err != nil || schedule == nil || schedule.NextRunAt == nil {
		t.Fatalf("schedule before callback = (%+v, %v)", schedule, err)
	}
	occurrence, run := dispatchTestRun(t, s, *schedule.NextRunAt)
	if occurrence.ScheduleID != scheduleID {
		t.Fatalf("callback occurrence belongs to schedule %d, want %d", occurrence.ScheduleID, scheduleID)
	}
	return dispatchTestFinish(t, s, run)
}

func scheduleSuggestionProposal(schedule Schedule, memory Memory) ScheduleContextProposal {
	return ScheduleContextProposal{
		ScheduleRevision: schedule.Revision,
		MemoryRefs:       []ScheduleMemoryRef{{Key: memory.Key, Version: memory.Version, Evidence: memory.Value}},
		Replacements:     []ContextReplacement{{Old: "Go", New: memory.Value, MemoryKey: memory.Key}},
	}
}

func finishScheduleSuggestionCallback(t *testing.T, s *Store, callback Event, silent bool) {
	t.Helper()
	ctx := context.Background()
	task, err := s.Task(ctx, callback.TaskID)
	if err != nil || task == nil {
		t.Fatalf("callback task = (%+v, %v)", task, err)
	}
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, task.Revision, TaskFailed, "The scheduled review ended with a result."); err != nil {
		t.Fatal(err)
	}
	output := TurnOutput{TaskID: task.ID, Text: "The scheduled review ended with a result.", Silent: silent}
	chunks := []string{"The scheduled review ended with a result."}
	if silent {
		output.Text, chunks = "", nil
	}
	finishEvent(t, s, callback, output, chunks)
}

func TestProposeScheduleContextUpdateRequiresItsCurrentCallbackAndReplays(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
	callback := scheduleSuggestionCallback(t, s, schedule.ID)
	proposal := scheduleSuggestionProposal(schedule, memory)
	unsupported := proposal
	unsupported.MemoryRefs = []ScheduleMemoryRef{{Key: memory.Key, Version: memory.Version, Evidence: callback.Text}}
	if _, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "callback-is-not-memory", callback.TaskID, unsupported, scheduleTestNow()); err == nil || !strings.Contains(err.Error(), "exact quote from the current memory value") {
		t.Fatalf("callback text accepted as memory evidence: %v", err)
	}
	if _, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "wrong-task", callback.TaskID+1, proposal, scheduleTestNow()); err == nil {
		t.Fatal("callback proposed a suggestion for another task")
	}
	suggestion, replayed, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "propose", callback.TaskID, proposal, scheduleTestNow())
	if err != nil || replayed || suggestion.ID <= 0 || suggestion.Status != "pending" || suggestion.NewContext != "Project uses Rust." {
		t.Fatalf("propose = (%+v, %v, %v)", suggestion, replayed, err)
	}
	replay, replayed, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "propose", callback.TaskID, proposal, scheduleTestNow().Add(time.Minute))
	if err != nil || !replayed || replay.ID != suggestion.ID || replay.NewContext != suggestion.NewContext {
		t.Fatalf("proposal replay = (%+v, %v, %v)", replay, replayed, err)
	}
	if _, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "propose", callback.TaskID, ScheduleContextProposal{}, scheduleTestNow()); err == nil {
		t.Fatal("proposal request key accepted different input")
	}
	items, err := s.ScheduleSuggestions(ctx, schedule.ID)
	if err != nil || len(items) != 1 || items[0].ID != suggestion.ID {
		t.Fatalf("pending suggestions = (%+v, %v)", items, err)
	}
	if _, _, err := s.ResolveScheduleContextUpdate(ctx, callback.ID, "unauthorized", suggestion.ID, suggestion.Revision, "apply", scheduleTestNow()); err == nil {
		t.Fatal("task callback applied its own suggestion")
	}
	finishScheduleSuggestionCallback(t, s, callback, true)
	replay, replayed, err = s.ProposeScheduleContextUpdate(ctx, callback.ID, "propose", callback.TaskID, proposal, scheduleTestNow().Add(2*time.Minute))
	if err != nil || !replayed || replay.ID != suggestion.ID {
		t.Fatalf("completed callback replay = (%+v, %v, %v)", replay, replayed, err)
	}
}

func TestProposeScheduleContextUpdateAllowsReadableReplacementAndApplies(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
	editScheduleEvent := openUserEvent(t, s, 3, 3, "Adjust the schedule wording")
	contextText := "Project uses Go."
	schedule, _, err := s.UpdateSchedule(ctx, editScheduleEvent.ID, "edit-context", schedule.ID, schedule.Revision, SchedulePatch{Context: &contextText}, scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, editScheduleEvent, TurnOutput{}, nil)

	memoryEvent := openUserEvent(t, s, 4, 4, "The project has migrated to Rust.")
	memory, err = s.WriteMemory(ctx, memoryEvent.ID, "update-language", MemoryChange{
		Operation: "put", Key: memory.Key, Value: "The project has migrated to Rust.", ExpectedVersion: memory.Version, Evidence: "Rust",
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, memoryEvent, TurnOutput{}, nil)

	callback := scheduleSuggestionCallback(t, s, schedule.ID)
	proposal := ScheduleContextProposal{
		ScheduleRevision: schedule.Revision,
		MemoryRefs:       []ScheduleMemoryRef{{Key: memory.Key, Version: memory.Version, Evidence: "Rust"}},
		Replacements:     []ContextReplacement{{Old: "uses Go", New: "uses Rust", MemoryKey: memory.Key}},
	}
	suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "readable-replacement", callback.TaskID, proposal, scheduleTestNow())
	if err != nil || suggestion.NewContext != "Project uses Rust." {
		t.Fatalf("readable replacement = (%+v, %v)", suggestion, err)
	}
	finishScheduleSuggestionCallback(t, s, callback, false)
	userEvent := openUserEvent(t, s, 5, 5, "Apply the Rust context update")
	resolved, _, err := s.ResolveScheduleContextUpdate(ctx, userEvent.ID, "apply-readable", suggestion.ID, suggestion.Revision, "apply", scheduleTestNow().Add(time.Minute))
	if err != nil || resolved.Status != "applied" || resolved.Schedule == nil || resolved.Schedule.Context != "Project uses Rust." {
		t.Fatalf("apply readable replacement = (%+v, %v)", resolved, err)
	}
}

func TestProposeScheduleContextUpdateDeduplicatesAfterMemoryVersionChangeAndReplays(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
	callback1 := scheduleSuggestionCallback(t, s, schedule.ID)
	proposal1 := scheduleSuggestionProposal(schedule, memory)
	suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback1.ID, "proposal-one", callback1.TaskID, proposal1, scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishScheduleSuggestionCallback(t, s, callback1, false)

	memoryEvent := openUserEvent(t, s, 3, 3, "Rust is still the current language.")
	memory, err = s.WriteMemory(ctx, memoryEvent.ID, "same-fact-new-version", MemoryChange{
		Operation: "put", Key: memory.Key, Value: memory.Value, ExpectedVersion: memory.Version, Evidence: "Rust",
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, memoryEvent, TurnOutput{}, nil)

	callback2 := scheduleSuggestionCallback(t, s, schedule.ID)
	proposal2 := scheduleSuggestionProposal(schedule, memory)
	deduplicated, replayed, err := s.ProposeScheduleContextUpdate(ctx, callback2.ID, "proposal-two", callback2.TaskID, proposal2, scheduleTestNow().Add(time.Minute))
	if err != nil || replayed || deduplicated.ID != suggestion.ID || deduplicated.OriginEventID != callback1.ID || deduplicated.TaskID != callback1.TaskID {
		t.Fatalf("later-cycle deduplication = (%+v, %v, %v)", deduplicated, replayed, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_schedule_context_suggestions WHERE schedule_id=?`, schedule.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("same fact created %d suggestion rows: %v", count, err)
	}
	finishScheduleSuggestionCallback(t, s, callback2, true)
	replay, replayed, err := s.ProposeScheduleContextUpdate(ctx, callback2.ID, "proposal-two", callback2.TaskID, proposal2, scheduleTestNow().Add(2*time.Minute))
	if err != nil || !replayed || replay.ID != suggestion.ID || replay.OriginEventID != callback1.ID || replay.TaskID != callback1.TaskID {
		t.Fatalf("later-cycle request replay = (%+v, %v, %v)", replay, replayed, err)
	}
}

func TestUnnotifiedScheduleSuggestionMovesToCurrentCallback(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
	callback1 := scheduleSuggestionCallback(t, s, schedule.ID)
	proposal := scheduleSuggestionProposal(schedule, memory)
	suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback1.ID, "proposal-one", callback1.TaskID, proposal, scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishScheduleSuggestionCallback(t, s, callback1, true)
	var notified sql.NullInt64
	if err := s.db.QueryRow(`SELECT notified_event_id FROM personal_schedule_context_suggestions WHERE id=?`, suggestion.ID).Scan(&notified); err != nil || notified.Valid {
		t.Fatalf("silent callback marked suggestion notified: (%+v, %v)", notified, err)
	}
	var outboxCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, callback1.ID).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("silent callback created outbox rows: (%d, %v)", outboxCount, err)
	}

	callback2 := scheduleSuggestionCallback(t, s, schedule.ID)
	updated, replayed, err := s.ProposeScheduleContextUpdate(ctx, callback2.ID, "proposal-two", callback2.TaskID, proposal, scheduleTestNow().Add(time.Minute))
	if err != nil || replayed || updated.ID != suggestion.ID || updated.OriginEventID != callback2.ID || updated.TaskID != callback2.TaskID {
		t.Fatalf("unnotified candidate transfer = (%+v, %v, %v)", updated, replayed, err)
	}
	finishScheduleSuggestionCallback(t, s, callback2, false)
	var notifiedEventID int64
	if err := s.db.QueryRow(`SELECT notified_event_id FROM personal_schedule_context_suggestions WHERE id=?`, suggestion.ID).Scan(&notifiedEventID); err != nil || notifiedEventID != callback2.ID {
		t.Fatalf("transferred candidate notification event = (%d, %v), want %d", notifiedEventID, err, callback2.ID)
	}
	var outboxID int64
	if err := s.db.QueryRow(`SELECT id FROM personal_outbox WHERE suggestion_id=? ORDER BY id LIMIT 1`, suggestion.ID).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	if items, err := s.ReplyScheduleSuggestions(ctx, outboxID); err != nil || len(items) != 0 {
		t.Fatalf("pending suggestion outbox was treated as a sent reply: (%+v, %v)", items, err)
	}
	if _, err := s.db.Exec(`UPDATE personal_outbox SET status='sent' WHERE id=?`, outboxID); err != nil {
		t.Fatal(err)
	}
	items, err := s.ReplyScheduleSuggestions(ctx, outboxID)
	if err != nil || len(items) != 1 || items[0].ID != suggestion.ID {
		t.Fatalf("suggestion reply association = (%+v, %v)", items, err)
	}
	if items, err := s.ReplyScheduleSuggestions(ctx, outboxID+100); err != nil || len(items) != 0 {
		t.Fatalf("unrelated reply outbox = (%+v, %v)", items, err)
	}
}

func TestResolveScheduleSuggestionAppliesWithScopeAndTaskCancellationAtomically(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowSinceSuccess)
	callback := scheduleSuggestionCallback(t, s, schedule.ID)
	proposal := scheduleSuggestionProposal(schedule, memory)
	suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "proposal", callback.TaskID, proposal, scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishScheduleSuggestionCallback(t, s, callback, false)
	setScheduleCoverageForTest(t, s, schedule, schedule.BaselineAt.Add(time.Second))
	current, err := s.Schedule(ctx, schedule.ID)
	if err != nil || current == nil || current.NextRunAt == nil {
		t.Fatalf("schedule before queued occurrence = (%+v, %v)", current, err)
	}
	queued, err := s.DispatchDueSchedule(ctx, *current.NextRunAt)
	if err != nil || queued == nil || queued.Outcome != "enqueued" {
		t.Fatalf("queued occurrence = (%+v, %v)", queued, err)
	}
	userEvent := openUserEvent(t, s, 3, 3, "Apply the displayed schedule suggestion")
	resolved, replayed, err := s.ResolveScheduleContextUpdate(ctx, userEvent.ID, "apply", suggestion.ID, suggestion.Revision, "apply", scheduleTestNow().Add(2*time.Minute))
	if err != nil || replayed || resolved.Status != "applied" || resolved.Schedule == nil || resolved.Schedule.ScopeRevision != schedule.ScopeRevision+1 || resolved.Schedule.CoveredThrough != nil || !strings.Contains(resolved.Schedule.Context, "Rust") {
		t.Fatalf("apply = (%+v, %v, %v)", resolved, replayed, err)
	}
	var taskStatus, runStatus string
	if err := s.db.QueryRow(`SELECT status FROM personal_tasks WHERE id=?`, queued.TaskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT status FROM personal_runs WHERE task_id=? AND kind='task'`, queued.TaskID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(TaskCancelled) || runStatus != string(RunCancelled) {
		t.Fatalf("unstarted scheduled task/run survived context change: %q/%q", taskStatus, runStatus)
	}
	replay, replayed, err := s.ResolveScheduleContextUpdate(ctx, userEvent.ID, "apply", suggestion.ID, suggestion.Revision, "apply", scheduleTestNow().Add(3*time.Minute))
	if err != nil || !replayed || replay.Status != "applied" || replay.Schedule == nil || replay.Schedule.Revision != resolved.Schedule.Revision {
		t.Fatalf("resolution replay = (%+v, %v, %v)", replay, replayed, err)
	}
}

func TestResolveScheduleSuggestionRebasesPlanAndRequiresAnotherUserEvent(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
	callback := scheduleSuggestionCallback(t, s, schedule.ID)
	suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "proposal", callback.TaskID, scheduleSuggestionProposal(schedule, memory), scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishScheduleSuggestionCallback(t, s, callback, false)
	var originalOutbox int64
	if err := s.db.QueryRow(`SELECT id FROM personal_outbox WHERE suggestion_id=?`, suggestion.ID).Scan(&originalOutbox); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE personal_outbox SET status='sent' WHERE id=?`, originalOutbox); err != nil {
		t.Fatal(err)
	}
	renameEvent := openUserEvent(t, s, 3, 3, "Rename the schedule")
	newTitle := "Renamed project update"
	updated, _, err := s.UpdateSchedule(ctx, renameEvent.ID, "rename", schedule.ID, schedule.Revision, SchedulePatch{Title: &newTitle}, scheduleTestNow().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, renameEvent, TurnOutput{}, nil)

	staleEvent := openUserEvent(t, s, 4, 4, "Apply the old suggestion")
	stale, replayed, err := s.ResolveScheduleContextUpdate(ctx, staleEvent.ID, "stale", suggestion.ID, suggestion.Revision, "apply", scheduleTestNow().Add(2*time.Minute))
	if err != nil || replayed || stale.Status != "stale" || stale.Suggestion.ID != suggestion.ID || stale.Suggestion.Revision != suggestion.Revision+1 || stale.Suggestion.ScheduleRevision != updated.Revision || stale.Suggestion.PresentedEventID != staleEvent.ID {
		t.Fatalf("stale plan response = (%+v, %v, %v)", stale, replayed, err)
	}
	if _, _, err := s.ResolveScheduleContextUpdate(ctx, staleEvent.ID, "same-turn", stale.Suggestion.ID, stale.Suggestion.Revision, "apply", scheduleTestNow().Add(2*time.Minute)); err == nil {
		t.Fatal("refreshed suggestion was applied in the same user event")
	}
	finishEvent(t, s, staleEvent, TurnOutput{Text: "The schedule version changed; confirm the refreshed suggestion."}, []string{"The schedule version changed; confirm the refreshed suggestion."})
	if replied, err := s.ReplyScheduleSuggestions(ctx, originalOutbox); err != nil || len(replied) != 0 {
		t.Fatalf("old notification silently pointed at a refreshed revision: %+v %v", replied, err)
	}
	var refreshedOutbox int64
	var exactText string
	if err := s.db.QueryRow(`SELECT id,text FROM personal_outbox WHERE event_id=? AND suggestion_id=?`, staleEvent.ID, suggestion.ID).Scan(&refreshedOutbox, &exactText); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{fmt.Sprintf("版本 %d", stale.Suggestion.Revision), "原背景：" + stale.Suggestion.Replacements[0].Old, "建议改为：" + stale.Suggestion.Replacements[0].New} {
		if !strings.Contains(exactText, expected) {
			t.Fatalf("refreshed notice omitted exact replacement %q: %q", expected, exactText)
		}
	}
	if _, err := s.db.Exec(`UPDATE personal_outbox SET status='sent' WHERE id=?`, refreshedOutbox); err != nil {
		t.Fatal(err)
	}
	if replied, err := s.ReplyScheduleSuggestions(ctx, refreshedOutbox); err != nil || len(replied) != 1 || replied[0].Revision != stale.Suggestion.Revision {
		t.Fatalf("refreshed notification lost its revision: %+v %v", replied, err)
	}

	confirmEvent := openUserEvent(t, s, 5, 5, "Confirm the refreshed suggestion")
	confirmed, replayed, err := s.ResolveScheduleContextUpdate(ctx, confirmEvent.ID, "confirm", stale.Suggestion.ID, stale.Suggestion.Revision, "apply", scheduleTestNow().Add(3*time.Minute))
	if err != nil || replayed || confirmed.Status != "applied" || confirmed.Schedule == nil || !strings.Contains(confirmed.Schedule.Context, "Rust") {
		t.Fatalf("refreshed confirmation = (%+v, %v, %v)", confirmed, replayed, err)
	}
}

func TestScheduleSuggestionRequiresTheRefreshedDiffInOutbox(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
	callback := scheduleSuggestionCallback(t, s, schedule.ID)
	suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "proposal", callback.TaskID, scheduleSuggestionProposal(schedule, memory), scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishScheduleSuggestionCallback(t, s, callback, false)
	edit := openUserEvent(t, s, 3, 3, "Rename the plan")
	title := "Renamed plan"
	if _, _, err := s.UpdateSchedule(ctx, edit.ID, "rename", schedule.ID, schedule.Revision, SchedulePatch{Title: &title}, scheduleTestNow()); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, edit, TurnOutput{}, nil)
	first := openUserEvent(t, s, 4, 4, "Apply the old suggestion")
	refreshed, _, err := s.ResolveScheduleContextUpdate(ctx, first.ID, "refresh", suggestion.ID, suggestion.Revision, "apply", scheduleTestNow())
	if err != nil || refreshed.Status != "stale" {
		t.Fatalf("refresh=%+v %v", refreshed, err)
	}
	finishEvent(t, s, first, TurnOutput{Silent: true}, nil)
	confirm := openUserEvent(t, s, 5, 5, "Apply the new version")
	if _, _, err := s.ResolveScheduleContextUpdate(ctx, confirm.ID, "apply", suggestion.ID, refreshed.Suggestion.Revision, "apply", scheduleTestNow()); err == nil {
		t.Fatal("completed but silent event was accepted as showing the new diff")
	}
	current, err := s.Schedule(ctx, schedule.ID)
	if err != nil || current.Context != schedule.Context {
		t.Fatalf("unshown candidate changed context: %+v %v", current, err)
	}
}

func TestResolveScheduleSuggestionChangedMemoryGetsNewCandidate(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
	callback := scheduleSuggestionCallback(t, s, schedule.ID)
	oldSuggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "proposal", callback.TaskID, scheduleSuggestionProposal(schedule, memory), scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishScheduleSuggestionCallback(t, s, callback, false)
	correction := openUserEvent(t, s, 3, 3, "The current project now uses Kotlin instead of Rust.")
	updatedMemory, err := s.WriteMemory(ctx, correction.ID, "remember-kotlin", MemoryChange{
		Operation: "put", Key: memory.Key, Value: "Kotlin", ExpectedVersion: memory.Version, Evidence: "Kotlin",
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, correction, TurnOutput{Text: "Memory corrected."}, []string{"Memory corrected."})

	staleEvent := openUserEvent(t, s, 4, 4, "Apply the old Rust suggestion")
	if _, err := s.db.Exec(`CREATE TRIGGER reject_refreshed_suggestion BEFORE INSERT ON personal_schedule_context_suggestions BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveScheduleContextUpdate(ctx, staleEvent.ID, "stale", oldSuggestion.ID, oldSuggestion.Revision, "apply", scheduleTestNow().Add(2*time.Minute)); err == nil {
		t.Fatal("failed candidate refresh was reported as a successful resolution")
	}
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_schedule_context_suggestions WHERE id=? AND status='pending'`, oldSuggestion.ID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("failed refresh retired the original candidate: %d %v", pending, err)
	}
	var requests int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_requests WHERE event_id=?`, staleEvent.ID).Scan(&requests); err != nil || requests != 0 {
		t.Fatalf("failed refresh saved a successful request: %d %v", requests, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_refreshed_suggestion`); err != nil {
		t.Fatal(err)
	}
	stale, replayed, err := s.ResolveScheduleContextUpdate(ctx, staleEvent.ID, "stale", oldSuggestion.ID, oldSuggestion.Revision, "apply", scheduleTestNow().Add(2*time.Minute))
	if err != nil || replayed || stale.Status != "stale" || stale.Suggestion.ID == oldSuggestion.ID || stale.Suggestion.ChangeKey == oldSuggestion.ChangeKey || stale.Suggestion.MemoryRefs[0].Version != updatedMemory.Version || stale.Suggestion.PresentedEventID != staleEvent.ID {
		t.Fatalf("changed memory candidate = (%+v, %v, %v)", stale, replayed, err)
	}
	var oldStatus string
	if err := s.db.QueryRow(`SELECT status FROM personal_schedule_context_suggestions WHERE id=?`, oldSuggestion.ID).Scan(&oldStatus); err != nil || oldStatus != "dismissed" {
		t.Fatalf("old candidate was not retired: (%q, %v)", oldStatus, err)
	}
	if !strings.Contains(stale.Suggestion.NewContext, "Kotlin") || !strings.Contains(stale.Suggestion.Replacements[0].New, "Kotlin") {
		t.Fatalf("refreshed candidate omitted current memory: %+v", stale.Suggestion)
	}
	finishEvent(t, s, staleEvent, TurnOutput{Text: "The memory changed; review this new suggestion."}, []string{"The memory changed; review this new suggestion."})

	confirmEvent := openUserEvent(t, s, 5, 5, "Confirm the Kotlin suggestion")
	confirmed, _, err := s.ResolveScheduleContextUpdate(ctx, confirmEvent.ID, "confirm", stale.Suggestion.ID, stale.Suggestion.Revision, "apply", scheduleTestNow().Add(3*time.Minute))
	if err != nil || confirmed.Status != "applied" || confirmed.Schedule == nil || !strings.Contains(confirmed.Schedule.Context, "Kotlin") {
		t.Fatalf("new candidate confirmation = (%+v, %v)", confirmed, err)
	}
}

func TestResolveScheduleSuggestionWithMissingOldAndNewNeedsProposal(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
	callback := scheduleSuggestionCallback(t, s, schedule.ID)
	suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "proposal", callback.TaskID, scheduleSuggestionProposal(schedule, memory), scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishScheduleSuggestionCallback(t, s, callback, false)

	editEvent := openUserEvent(t, s, 3, 3, "Replace the language-specific context")
	contextText := "Project status is unknown."
	updated, _, err := s.UpdateSchedule(ctx, editEvent.ID, "replace-context", schedule.ID, schedule.Revision, SchedulePatch{Context: &contextText}, scheduleTestNow().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, editEvent, TurnOutput{}, nil)

	resolveEvent := openUserEvent(t, s, 4, 4, "Apply the old suggestion")
	resolved, replayed, err := s.ResolveScheduleContextUpdate(ctx, resolveEvent.ID, "stale-context", suggestion.ID, suggestion.Revision, "apply", scheduleTestNow().Add(2*time.Minute))
	if err != nil || replayed || resolved.Status != "stale" || !resolved.NeedsProposal || resolved.Suggestion.PresentedEventID != resolveEvent.ID || resolved.Schedule == nil || resolved.Schedule.Context != updated.Context {
		t.Fatalf("missing old and new fragments = (%+v, %v, %v)", resolved, replayed, err)
	}
}

func TestResolveScheduleSuggestionKeepAndNoChange(t *testing.T) {
	ctx := context.Background()
	t.Run("keep", func(t *testing.T) {
		s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
		callback := scheduleSuggestionCallback(t, s, schedule.ID)
		suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "proposal", callback.TaskID, scheduleSuggestionProposal(schedule, memory), scheduleTestNow())
		if err != nil {
			t.Fatal(err)
		}
		finishScheduleSuggestionCallback(t, s, callback, false)
		userEvent := openUserEvent(t, s, 3, 3, "Keep the current schedule context")
		result, replayed, err := s.ResolveScheduleContextUpdate(ctx, userEvent.ID, "keep", suggestion.ID, suggestion.Revision, "keep", scheduleTestNow().Add(time.Minute))
		if err != nil || replayed || result.Status != "dismissed" || result.Schedule == nil || result.Schedule.Revision != schedule.Revision || result.Schedule.Context != schedule.Context {
			t.Fatalf("keep = (%+v, %v, %v)", result, replayed, err)
		}
		replay, replayed, err := s.ResolveScheduleContextUpdate(ctx, userEvent.ID, "keep", suggestion.ID, suggestion.Revision, "keep", scheduleTestNow().Add(2*time.Minute))
		if err != nil || !replayed || replay.Status != "dismissed" {
			t.Fatalf("keep replay = (%+v, %v, %v)", replay, replayed, err)
		}
	})

	t.Run("already applied context", func(t *testing.T) {
		s, schedule, memory := scheduleSuggestionFixture(t, WindowNone)
		callback := scheduleSuggestionCallback(t, s, schedule.ID)
		suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "proposal", callback.TaskID, scheduleSuggestionProposal(schedule, memory), scheduleTestNow())
		if err != nil {
			t.Fatal(err)
		}
		finishScheduleSuggestionCallback(t, s, callback, false)
		userEvent := openUserEvent(t, s, 3, 3, "Apply the context directly")
		contextText := suggestion.NewContext
		updated, _, err := s.UpdateSchedule(ctx, userEvent.ID, "direct-update", schedule.ID, schedule.Revision, SchedulePatch{Context: &contextText}, scheduleTestNow().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		finishEvent(t, s, userEvent, TurnOutput{}, nil)
		confirm := openUserEvent(t, s, 4, 4, "Apply the suggestion that is already present")
		result, _, err := s.ResolveScheduleContextUpdate(ctx, confirm.ID, "no-change", suggestion.ID, suggestion.Revision, "apply", scheduleTestNow().Add(2*time.Minute))
		if err != nil || result.Status != "no_change" || result.Schedule == nil || result.Schedule.Revision != updated.Revision || result.Schedule.Context != suggestion.NewContext {
			t.Fatalf("already-applied suggestion = (%+v, %v)", result, err)
		}
	})
}
