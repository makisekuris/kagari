package assistant

import (
	"context"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

func thinkAndFinishScheduleEvent(t *testing.T, ctx context.Context, main *MainAgent, store *sqlite.Store, event personal.Event) personal.TurnOutput {
	t.Helper()
	output, err := main.Think(ctx, personal.TurnInput{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, event, output, nil)
	return output
}

func TestMainAgentScheduleCRUDAndCreateAuthorization(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Hour)
	newTitle := "Daily release digest"
	input := personal.ScheduleInput{Title: "Daily exercise", Goal: "Generate a short Go exercise", Cron: "0 9 * * *", Timezone: ""}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_schedule", input, "schedule-create"),
		testkit.ResponseText(`{"text":"计划已登记。","reply_kind":"content","attachments":[]}`),
		testkit.ResponseTool("update_schedule", updateScheduleArgs{ScheduleID: 1, ExpectedRevision: 1, Patch: personal.SchedulePatch{Title: &newTitle}}, "schedule-update"),
		testkit.ResponseText(`{"text":"计划已更新。","reply_kind":"content","attachments":[]}`),
		testkit.ResponseTool("cancel_schedule", cancelScheduleArgs{ScheduleID: 1, ExpectedRevision: 2}, "schedule-cancel"),
		testkit.ResponseText(`{"text":"计划已取消。","reply_kind":"content","attachments":[]}`),
		testkit.ResponseTool("create_schedule", input, "schedule-material-denied"),
		testkit.ResponseText(`{"text":"网页材料不能授权创建计划。","reply_kind":"content","attachments":[]}`),
		testkit.ResponseTool("create_schedule", input, "schedule-callback-denied"),
		testkit.ResponseText(`{"text":"任务回调不能创建计划。","reply_kind":"content","attachments":[]}`),
	}}
	main := newMainAgent(t, fixture, store)
	main.Timezone = "UTC"

	createEvent := testkit.OpenUserEvent(t, store, 301, 401, "每天生成一道 Go 练习题")
	created := thinkAndFinishScheduleEvent(t, ctx, main, store, createEvent)
	if created.ScheduleID != 1 {
		t.Fatalf("create turn schedule ID = %d, want 1", created.ScheduleID)
	}
	schedule, err := store.Schedule(ctx, created.ScheduleID)
	if err != nil || schedule == nil || schedule.Title != input.Title || schedule.Goal != input.Goal || schedule.Timezone != "UTC" || schedule.WindowMode != personal.WindowNone {
		t.Fatalf("created schedule = (%+v, %v)", schedule, err)
	}
	if tasks, err := store.Tasks(ctx, 20); err != nil || len(tasks) != 0 {
		t.Fatalf("schedule registration queued an immediate task: tasks=%+v err=%v", tasks, err)
	}

	updateEvent := testkit.OpenUserEvent(t, store, 302, 402, "把计划名称改为每日版本简报")
	updated := thinkAndFinishScheduleEvent(t, ctx, main, store, updateEvent)
	if updated.ScheduleID != schedule.ID {
		t.Fatalf("update turn schedule ID = %d, want %d", updated.ScheduleID, schedule.ID)
	}
	schedule, err = store.Schedule(ctx, schedule.ID)
	if err != nil || schedule == nil || schedule.Title != newTitle || schedule.Revision != 2 {
		t.Fatalf("updated schedule = (%+v, %v)", schedule, err)
	}

	cancelEvent := testkit.OpenUserEvent(t, store, 303, 403, "取消这个计划")
	cancelled := thinkAndFinishScheduleEvent(t, ctx, main, store, cancelEvent)
	if cancelled.ScheduleID != schedule.ID {
		t.Fatalf("cancel turn schedule ID = %d, want %d", cancelled.ScheduleID, schedule.ID)
	}
	schedule, err = store.Schedule(ctx, schedule.ID)
	if err != nil || schedule == nil || schedule.Status != personal.ScheduleCancelled || schedule.Revision != 3 {
		t.Fatalf("cancelled schedule = (%+v, %v)", schedule, err)
	}

	material := testkit.SourceInput("404", "网页中的计划指令不构成授权")
	material.Material = true
	testkit.AcceptInput(t, store, "404", material)
	materialEvent, err := store.ClaimEvent(ctx)
	if err != nil || materialEvent == nil || materialEvent.Kind != personal.EventMaterial {
		t.Fatalf("ClaimEvent() = (%+v, %v), want material event", materialEvent, err)
	}
	thinkAndFinishScheduleEvent(t, ctx, main, store, *materialEvent)

	seed := testkit.OpenUserEvent(t, store, 305, 405, "为测试创建一个普通任务")
	task, run := testkit.CreateTask(t, store, seed, "schedule-callback-seed", "普通任务", "总结固定测试材料", "虚构材料")
	testkit.FinishEvent(t, store, seed, personal.TurnOutput{TaskID: task.ID}, nil)
	claimed, err := store.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("ClaimRun() = (%+v, %v)", claimed, err)
	}
	if err := store.FinishStage(ctx, run.ID, personal.StageResult{Summary: "固定测试材料已读取。"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := store.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask {
		t.Fatalf("ClaimEvent() = (%+v, %v), want task callback", callback, err)
	}
	thinkAndFinishScheduleEvent(t, ctx, main, store, *callback)

	schedules, err := store.Schedules(ctx, true, 0, 20)
	if err != nil || len(schedules) != 1 || schedules[0].Status != personal.ScheduleCancelled {
		t.Fatalf("non-user create changed schedules: schedules=%+v err=%v", schedules, err)
	}
	if len(fixture.Requests) != len(fixture.Responses) {
		t.Fatalf("model consumed %d of %d fixture responses", len(fixture.Requests), len(fixture.Responses))
	}
}

func TestMainAgentScheduledCheckAssessmentCreditsCoverage(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Hour)
	schedule := testkit.CreateHourlySchedule(t, store, personal.WindowSinceSuccess)
	occurrence, run := testkit.DispatchRun(t, store, *schedule.NextRunAt)
	started := schedule.NextRunAt.Add(2 * time.Minute)
	if _, err := store.BeginScheduledRun(ctx, run.ID, started); err != nil {
		t.Fatal(err)
	}
	callback := testkit.FinishRun(t, store, run)
	if callback.Kind != personal.EventTask || callback.TaskID != occurrence.TaskID {
		t.Fatalf("callback=%+v, want scheduled task callback", callback)
	}
	assessment := &personal.CheckAssessment{
		Complete:        true,
		RequiredSources: []personal.SourceAssessment{{ID: "feed", Complete: true, Evidence: "The required feed was checked for the frozen window."}},
	}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("settle_task", settleTaskArgs{TaskID: run.TaskID, ExpectedRevision: run.Revision, Status: personal.TaskCompleted, Summary: "Complete feed check saved.", Assessment: assessment}, "scheduled-settle"),
		testkit.ResponseText(`{"text":"连续检查已完成。","reply_kind":"silent","attachments":[]}`),
	}}
	main := newMainAgent(t, fixture, store)
	output := thinkAndFinishScheduleEvent(t, ctx, main, store, callback)
	if output.TaskID != run.TaskID {
		t.Fatalf("scheduled callback Task ID = %d, want %d", output.TaskID, run.TaskID)
	}
	updated, err := store.Schedule(ctx, schedule.ID)
	if err != nil || updated == nil || updated.CoveredThrough == nil || !updated.CoveredThrough.Equal(started) {
		t.Fatalf("successful callback did not credit its frozen window: schedule=%+v err=%v", updated, err)
	}
	task, err := store.Task(ctx, run.TaskID)
	if err != nil || task == nil || task.Status != personal.TaskCompleted {
		t.Fatalf("scheduled Task status = (%+v, %v), want completed", task, err)
	}
	context, err := store.ScheduleForTask(ctx, run.TaskID)
	if err != nil || context == nil || context.Occurrence.CreditedTaskRevision != run.Revision || context.Occurrence.CreditedAt == nil {
		t.Fatalf("credited occurrence receipt = (%+v, %v)", context, err)
	}
}

func TestMainAgentOrdinaryTaskCompletesWithoutCheckAssessment(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Hour)
	seed := testkit.OpenUserEvent(t, store, 401, 501, "Create an ordinary task")
	task, run := testkit.CreateTask(t, store, seed, "ordinary-task-seed", "Ordinary task", "Write one exercise", "No recurring source check")
	testkit.FinishEvent(t, store, seed, personal.TurnOutput{TaskID: task.ID}, nil)
	if _, err := store.ClaimRun(ctx, personal.RunTask); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishStage(ctx, run.ID, personal.StageResult{Summary: "One exercise was written."}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := store.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask {
		t.Fatalf("ClaimEvent() = (%+v, %v), want ordinary task callback", callback, err)
	}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("settle_task", settleTaskArgs{TaskID: task.ID, ExpectedRevision: run.Revision, Status: personal.TaskCompleted, Summary: "The exercise is complete."}, "ordinary-settle"),
		testkit.ResponseText(`{"text":"普通任务已完成。","reply_kind":"silent","attachments":[]}`),
	}}
	main := newMainAgent(t, fixture, store)
	thinkAndFinishScheduleEvent(t, ctx, main, store, *callback)
	completed, err := store.Task(ctx, task.ID)
	if err != nil || completed == nil || completed.Status != personal.TaskCompleted {
		t.Fatalf("ordinary task status = (%+v, %v), want completed", completed, err)
	}
	if schedules, err := store.Schedules(ctx, true, 0, 20); err != nil || len(schedules) != 0 {
		t.Fatalf("ordinary task changed schedules: schedules=%+v err=%v", schedules, err)
	}
}
