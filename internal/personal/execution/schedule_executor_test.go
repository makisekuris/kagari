package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestScheduledExecutorReceivesFrozenWindowAndConflictingMemory(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Minute)
	createEvent := testkit.OpenUserEvent(t, store, 1, 1, "Create a scheduled task and remember the current language")
	if _, err := store.WriteMemory(ctx, createEvent.ID, "language", personal.MemoryChange{Operation: "put", Key: "project.language", Value: "The project now uses Rust.", Evidence: createEvent.Text}); err != nil {
		t.Fatal(err)
	}
	scheduledAt := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	schedule, _, err := store.CreateSchedule(ctx, createEvent.ID, "scheduled", personal.ScheduleInput{
		Title: "Hourly release check", Goal: "Check the release feed", Context: "This plan uses Go.",
		Cron: "0 * * * *", Timezone: "UTC", WindowMode: personal.WindowSinceSuccess,
		RequiredSources: []personal.SourceRequirement{{ID: "feed", Label: "Release feed"}},
	}, scheduledAt)
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, createEvent, personal.TurnOutput{ScheduleID: schedule.ID}, nil)
	started := scheduledAt.Add(3 * time.Hour)
	occurrence, err := store.DispatchDueSchedule(ctx, started)
	if err != nil || occurrence == nil || occurrence.Outcome != "enqueued" {
		t.Fatalf("DispatchDueSchedule() = (%+v, %v)", occurrence, err)
	}
	run, err := store.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatalf("ClaimRun() = (%+v, %v)", run, err)
	}
	run, err = store.BeginScheduledRun(ctx, run.ID, started)
	if err != nil || run == nil {
		t.Fatalf("BeginScheduledRun() = (%+v, %v)", run, err)
	}
	var input personal.TaskInput
	if err := json.Unmarshal([]byte(run.Input), &input); err != nil {
		t.Fatal(err)
	}
	if input.Context != "This plan uses Go." || input.Trigger == nil || input.Trigger.ScheduleID != schedule.ID ||
		input.Trigger.OccurrenceID != occurrence.ID || input.Trigger.ScheduleRevision != schedule.Revision ||
		input.Trigger.ScopeRevision != schedule.ScopeRevision || input.Trigger.StartedAt == nil || !input.Trigger.StartedAt.Equal(started) ||
		input.Trigger.Window == nil || schedule.BaselineAt == nil || !input.Trigger.Window.Start.Equal(*schedule.BaselineAt) ||
		!input.Trigger.Window.End.Equal(started) || !input.Trigger.CoverageEligible {
		t.Fatalf("scheduled task input did not retain its frozen context and window: %+v", input)
	}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("memory_get", map[string]string{"key": "project.language"}, "current-language"),
		testkit.ResponseText("Saved result; fixed background was retained."),
	}}
	if _, err := runTaskExecutor(t, ctx, testkit.ReadingEngine(fixture, nil), store, *run, nil); err != nil {
		t.Fatal(err)
	}
	triggerJSON, err := json.Marshal(input.Trigger)
	if err != nil {
		t.Fatal(err)
	}
	wireTrigger, err := json.Marshal(string(triggerJSON))
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := testkit.RequestInput(fixture.Requests[0])
	if !strings.Contains(firstRequest, string(wireTrigger[1:len(wireTrigger)-1])) || !strings.Contains(firstRequest, input.Context) {
		t.Fatalf("executor did not receive the actual frozen schedule metadata and context: %s", firstRequest)
	}
	secondRequest := testkit.RequestInput(fixture.Requests[1])
	if !strings.Contains(secondRequest, input.Context) || !strings.Contains(secondRequest, "The project now uses Rust.") {
		t.Fatalf("executor lost fixed task context or conflicting current memory: %s", secondRequest)
	}
}
