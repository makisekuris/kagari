package personal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
)

func invokeScheduleTool(t *testing.T, tools []tool.BaseTool, name string, args any) scheduleToolReply {
	t.Helper()
	ctx := context.Background()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.Name != name {
			continue
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatalf("tool %q is not invokable", name)
		}
		output, err := invokable.InvokableRun(ctx, string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		var reply scheduleToolReply
		if err := json.Unmarshal([]byte(output), &reply); err != nil {
			t.Fatalf("decode %s reply %q: %v", name, output, err)
		}
		return reply
	}
	t.Fatalf("tool %q not found", name)
	return scheduleToolReply{}
}

func scheduleToolsForEvent(t *testing.T, store *Store, event Event, timezone string) (*personalTools, []tool.BaseTool) {
	t.Helper()
	p := &personalTools{
		store: store, event: event, timezone: timezone,
		taskIDs: make(map[int64]struct{}), scheduleIDs: make(map[int64]struct{}),
	}
	tools, err := p.buildScheduleTools()
	if err != nil {
		t.Fatal(err)
	}
	return p, tools
}

func TestBuildScheduleToolsCreateUpdateCancelAndAuthorization(t *testing.T) {
	ctx := context.Background()
	store := testStore(t, filepath.Join(t.TempDir(), "personal.db"), time.Hour)
	event := openUserEvent(t, store, 201, 301, "Create a schedule")
	p, tools := scheduleToolsForEvent(t, store, event, "America/Los_Angeles")
	if len(tools) != 5 {
		t.Fatalf("schedule tool count = %d, want 5", len(tools))
	}

	input := ScheduleInput{Title: "Daily digest", Goal: "Summarize new releases", Cron: "0 9 * * *"}
	created := invokeScheduleTool(t, tools, "create_schedule", input)
	if !created.OK || created.Schedule == nil {
		t.Fatalf("create_schedule reply = %+v", created)
	}
	if created.Schedule.Timezone != "America/Los_Angeles" || created.Schedule.Status != ScheduleActive || created.Schedule.NextRunAt == nil {
		t.Fatalf("created schedule did not use defaults: %+v", created.Schedule)
	}
	if !created.Schedule.NextRunAt.After(created.Schedule.CreatedAt) {
		t.Fatalf("first run %s is not later than creation %s", created.Schedule.NextRunAt, created.Schedule.CreatedAt)
	}
	if p.scheduleID() != created.Schedule.ID {
		t.Fatalf("associated schedule ID = %d, want %d", p.scheduleID(), created.Schedule.ID)
	}
	if replay := invokeScheduleTool(t, tools, "create_schedule", input); !replay.OK || !replay.Replayed || replay.Schedule.ID != created.Schedule.ID {
		t.Fatalf("replayed create_schedule reply = %+v", replay)
	}
	if tasks, err := store.Tasks(ctx, 20); err != nil || len(tasks) != 0 {
		t.Fatalf("creating a schedule immediately queued tasks: tasks=%+v err=%v", tasks, err)
	}

	title := "Daily release digest"
	updated := invokeScheduleTool(t, tools, "update_schedule", updateScheduleArgs{
		ScheduleID: created.Schedule.ID, ExpectedRevision: created.Schedule.Revision,
		Patch: SchedulePatch{Title: &title},
	})
	if !updated.OK || updated.Schedule == nil || updated.Schedule.Title != title || updated.Schedule.Revision != 2 {
		t.Fatalf("update_schedule reply = %+v", updated)
	}
	staleTitle := "Stale title"
	stale := invokeScheduleTool(t, tools, "update_schedule", updateScheduleArgs{
		ScheduleID: created.Schedule.ID, ExpectedRevision: created.Schedule.Revision,
		Patch: SchedulePatch{Title: &staleTitle},
	})
	if stale.OK || stale.Error == "" {
		t.Fatalf("stale update_schedule reply = %+v", stale)
	}
	if !p.mustNotify {
		t.Fatal("failed update_schedule did not require a user notification")
	}

	cancelled := invokeScheduleTool(t, tools, "cancel_schedule", cancelScheduleArgs{
		ScheduleID: updated.Schedule.ID, ExpectedRevision: updated.Schedule.Revision,
	})
	if !cancelled.OK || cancelled.Schedule == nil || cancelled.Schedule.Status != ScheduleCancelled {
		t.Fatalf("cancel_schedule reply = %+v", cancelled)
	}
	if listed := invokeScheduleTool(t, tools, "list_schedules", listSchedulesArgs{}); !listed.OK || len(listed.Schedules) != 0 {
		t.Fatalf("default list_schedules reply = %+v", listed)
	}
	if listed := invokeScheduleTool(t, tools, "list_schedules", listSchedulesArgs{IncludeCancelled: true}); !listed.OK || len(listed.Schedules) != 1 {
		t.Fatalf("include_cancelled list_schedules reply = %+v", listed)
	}

	finishEvent(t, store, event, TurnOutput{Text: "Schedule saved"}, []string{"Schedule saved"})
	_, callbackTools := scheduleToolsForEvent(t, store, Event{ID: event.ID, Kind: EventTask}, "UTC")
	if denied := invokeScheduleTool(t, callbackTools, "create_schedule", input); denied.OK || denied.Error == "" {
		t.Fatalf("task callback created a schedule: %+v", denied)
	}
	if schedules, err := store.Schedules(ctx, true, 0, 20); err != nil || len(schedules) != 1 {
		t.Fatalf("denied mutation changed schedules: schedules=%+v err=%v", schedules, err)
	}
}

func TestGetScheduleReturnsOccurrenceTaskStatusAndPages(t *testing.T) {
	ctx := context.Background()
	store := testStore(t, filepath.Join(t.TempDir(), "personal.db"), time.Hour)
	event := openUserEvent(t, store, 202, 302, "Create a minute schedule")
	_, tools := scheduleToolsForEvent(t, store, event, "UTC")
	created := invokeScheduleTool(t, tools, "create_schedule", ScheduleInput{
		Title: "Minute check", Goal: "Check the status", Cron: "* * * * *", Timezone: "UTC",
	})
	if !created.OK || created.Schedule == nil || created.Schedule.NextRunAt == nil {
		t.Fatalf("create_schedule reply = %+v", created)
	}
	finishEvent(t, store, event, TurnOutput{Text: "Schedule saved"}, []string{"Schedule saved"})
	occurrence, err := store.DispatchDueSchedule(ctx, *created.Schedule.NextRunAt)
	if err != nil || occurrence == nil || occurrence.Outcome != "enqueued" || occurrence.TaskID <= 0 {
		t.Fatalf("DispatchDueSchedule() = (%+v,%v)", occurrence, err)
	}

	_, tools = scheduleToolsForEvent(t, store, Event{Kind: EventUser}, "UTC")
	got := invokeScheduleTool(t, tools, "get_schedule", getScheduleArgs{ScheduleID: created.Schedule.ID})
	if !got.OK || got.Schedule == nil || got.Schedule.ID != created.Schedule.ID || len(got.Occurrences) != 1 {
		t.Fatalf("get_schedule reply = %+v", got)
	}
	if got.Occurrences[0].TaskID != occurrence.TaskID || got.Occurrences[0].TaskStatus != TaskQueued {
		t.Fatalf("get_schedule omitted the current Task status: %+v", got.Occurrences[0])
	}
	if len(got.Suggestions) != 0 {
		t.Fatalf("unexpected context suggestions: %+v", got.Suggestions)
	}
	page := invokeScheduleTool(t, tools, "get_schedule", getScheduleArgs{
		ScheduleID: created.Schedule.ID, BeforeOccurrenceID: occurrence.ID,
	})
	if !page.OK || len(page.Occurrences) != 0 {
		t.Fatalf("get_schedule page = %+v", page)
	}
	if invalid := invokeScheduleTool(t, tools, "get_schedule", getScheduleArgs{ScheduleID: 0}); invalid.OK {
		t.Fatalf("get_schedule accepted schedule_id=0: %+v", invalid)
	}
	if missing := invokeScheduleTool(t, tools, "get_schedule", getScheduleArgs{ScheduleID: created.Schedule.ID + 100}); missing.OK {
		t.Fatalf("get_schedule accepted an unknown ID: %+v", missing)
	}
	if invalid := invokeScheduleTool(t, tools, "list_schedules", listSchedulesArgs{BeforeID: -1}); invalid.OK {
		t.Fatalf("list_schedules accepted a negative cursor: %+v", invalid)
	}
	if got, err := schedulePageLimit(0); err != nil || got != 20 {
		t.Fatalf("default page size = (%d,%v), want 20", got, err)
	}
	if got, err := schedulePageLimit(1000); err != nil || got != 100 {
		t.Fatalf("capped page size = (%d,%v), want 100", got, err)
	}
}
