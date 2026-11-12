package testkit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/sqlite"
)

var dispatchNow = time.Date(2026, 11, 4, 7, 30, 0, 0, time.UTC)

func ScheduleNow() time.Time {
	return time.Date(2026, 11, 6, 10, 0, 30, 0, time.UTC)
}

func CreateSchedule(t *testing.T, s *sqlite.Store, updateID, messageID int64, key string, input personal.ScheduleInput, now time.Time) (personal.Event, personal.Schedule) {
	t.Helper()
	event := OpenUserEvent(t, s, updateID, messageID, "Create a recurring schedule")
	schedule, replayed, err := s.CreateSchedule(context.Background(), event.ID, key, input, now)
	if err != nil || replayed {
		t.Fatalf("CreateSchedule() = (%+v,%v,%v)", schedule, replayed, err)
	}
	return event, schedule
}

func CreateHourlySchedule(t *testing.T, s *sqlite.Store, mode personal.WindowMode) personal.Schedule {
	t.Helper()
	e := OpenUserEvent(t, s, 1, 1, "Register an hourly plan")
	input := personal.ScheduleInput{Title: "hourly", Goal: "Check the feed", Context: "Only the requested feed", Cron: "0 * * * *", Timezone: "UTC", WindowMode: mode}
	if mode == personal.WindowSinceSuccess {
		input.RequiredSources = []personal.SourceRequirement{{ID: "feed", Label: "Feed"}}
	}
	p, _, err := s.CreateSchedule(context.Background(), e.ID, "plan", input, dispatchNow)
	if err != nil {
		t.Fatal(err)
	}
	FinishEvent(t, s, e, personal.TurnOutput{ScheduleID: p.ID}, nil)
	return p
}

func DispatchRun(t *testing.T, s *sqlite.Store, now time.Time) (personal.ScheduleOccurrence, personal.Run) {
	t.Helper()
	ctx := context.Background()
	o, err := s.DispatchDueSchedule(ctx, now)
	if err != nil || o == nil || o.Outcome != "enqueued" {
		t.Fatalf("dispatch=%+v err=%v", o, err)
	}
	r, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || r == nil {
		t.Fatalf("claim=%+v err=%v", r, err)
	}
	return *o, *r
}

func FinishRun(t *testing.T, s *sqlite.Store, r personal.Run) personal.Event {
	t.Helper()
	ctx := context.Background()
	if err := s.FinishStage(ctx, r.ID, personal.StageResult{Summary: "Saved check results"}, ""); err != nil {
		t.Fatal(err)
	}
	e, err := s.ClaimEvent(ctx)
	if err != nil || e == nil || e.Kind != personal.EventTask {
		t.Fatalf("callback=%+v err=%v", e, err)
	}
	return *e
}

func ScheduleSuggestion(t *testing.T, mode personal.WindowMode) (*sqlite.Store, personal.Schedule, personal.Memory) {
	t.Helper()
	ctx := context.Background()
	s := Store(t, filepath.Join(t.TempDir(), "suggestions.db"), time.Minute)
	createEvent := OpenUserEvent(t, s, 1, 1, "Create a schedule with the Go project context")
	input := personal.ScheduleInput{
		Title: "Project update", Goal: "Review the latest project update", Context: "Project uses Go.",
		Cron: "* * * * *", Timezone: "UTC", WindowMode: mode,
	}
	if mode == personal.WindowSinceSuccess {
		input.RequiredSources = []personal.SourceRequirement{{ID: "feed", Label: "Project feed"}}
	}
	schedule, _, err := s.CreateSchedule(ctx, createEvent.ID, "create-schedule", input, ScheduleNow())
	if err != nil {
		t.Fatal(err)
	}
	FinishEvent(t, s, createEvent, personal.TurnOutput{ScheduleID: schedule.ID}, nil)
	memoryEvent := OpenUserEvent(t, s, 2, 2, "Remember that the current project uses Rust.")
	memory, err := s.WriteMemory(ctx, memoryEvent.ID, "remember-language", personal.MemoryChange{
		Operation: "put", Key: "project.language", Value: "Rust", Evidence: "Rust",
	})
	if err != nil {
		t.Fatal(err)
	}
	FinishEvent(t, s, memoryEvent, personal.TurnOutput{Text: "Memory saved."}, []string{"Memory saved."})
	return s, schedule, memory
}

func SuggestionCallback(t *testing.T, s *sqlite.Store, scheduleID int64) personal.Event {
	t.Helper()
	schedule, err := s.Schedule(context.Background(), scheduleID)
	if err != nil || schedule == nil || schedule.NextRunAt == nil {
		t.Fatalf("schedule before callback = (%+v, %v)", schedule, err)
	}
	occurrence, run := DispatchRun(t, s, *schedule.NextRunAt)
	if occurrence.ScheduleID != scheduleID {
		t.Fatalf("callback occurrence belongs to schedule %d, want %d", occurrence.ScheduleID, scheduleID)
	}
	return FinishRun(t, s, run)
}

func SuggestionProposal(schedule personal.Schedule, memory personal.Memory) personal.ScheduleContextProposal {
	return personal.ScheduleContextProposal{
		ScheduleRevision: schedule.Revision,
		MemoryRefs:       []personal.ScheduleMemoryRef{{Key: memory.Key, Version: memory.Version, Evidence: memory.Value}},
		Replacements:     []personal.ContextReplacement{{Old: "Go", New: memory.Value, MemoryKey: memory.Key}},
	}
}

func FinishSuggestionCallback(t *testing.T, s *sqlite.Store, callback personal.Event, silent bool) {
	t.Helper()
	ctx := context.Background()
	task, err := s.Task(ctx, callback.TaskID)
	if err != nil || task == nil {
		t.Fatalf("callback task = (%+v, %v)", task, err)
	}
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, task.Revision, personal.TaskFailed, "The scheduled review ended with a result."); err != nil {
		t.Fatal(err)
	}
	output := personal.TurnOutput{TaskID: task.ID, Text: "The scheduled review ended with a result.", Silent: silent}
	chunks := []string{"The scheduled review ended with a result."}
	if silent {
		output.Text, chunks = "", nil
	}
	FinishEvent(t, s, callback, output, chunks)
}
