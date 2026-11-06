package personal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
)

func thinkAndFinishEvent(t *testing.T, ctx context.Context, agent *MainAgent, store *Store, event Event) TurnOutput {
	t.Helper()
	output, err := agent.Think(ctx, livePersonalInput(t, ctx, store, event))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteEvent(ctx, event.ID, output, nil); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestMainAgentScheduleReplyDoesNotInheritAnUnrelatedTask(t *testing.T) {
	for _, name := range []string{"other plan", "task and other plan", "multiple plans"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Hour)
			seed := openUserEvent(t, s, 1, 1, "Register two plans")
			input := ScheduleInput{Title: "Plan A", Goal: "Write an exercise", Cron: "0 * * * *", Timezone: "UTC"}
			a, _, err := s.CreateSchedule(ctx, seed.ID, "a", input, dispatchTestNow)
			if err != nil {
				t.Fatal(err)
			}
			input.Title = "Plan B"
			b, _, err := s.CreateSchedule(ctx, seed.ID, "b", input, dispatchTestNow)
			if err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, seed, TurnOutput{}, nil)
			_, run := dispatchTestRun(t, s, *a.NextRunAt)
			if _, err := s.BeginScheduledRun(ctx, run.ID, a.NextRunAt.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			callback := dispatchTestFinish(t, s, run)
			if _, err := s.SettleTask(ctx, callback.ID, run.TaskID, run.Revision, TaskWaitingInput, "Need input"); err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, callback, TurnOutput{TaskID: run.TaskID, Text: "Need input"}, []string{"Need input"})
			delivery, err := s.ClaimDelivery(ctx)
			if err != nil || delivery == nil {
				t.Fatalf("original result=%+v %v", delivery, err)
			}
			if err := s.UpdateDelivery(ctx, delivery.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "plan-a-result"}, "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			message := sourceInput("cross-plan-reply", "Query the requested plans; cancel this Task if specified")
			message.ReplyToID = "plan-a-result"
			acceptInput(t, s, "cross-plan", message)
			event, err := s.ClaimEvent(ctx)
			if err != nil || event == nil || event.TaskID != run.TaskID || event.ScheduleID != a.ID {
				t.Fatalf("reply association=%+v %v", event, err)
			}
			var responses []fixtureResponse
			wantSchedule, wantTaskState := b.ID, TaskWaitingInput
			switch name {
			case "task and other plan":
				responses = append(responses, responseTool("cancel_task", cancelTaskArgs{TaskID: run.TaskID, ExpectedRevision: run.Revision}, "cancel-a-task"))
				wantSchedule, wantTaskState = 0, TaskCancelled
			case "multiple plans":
				responses = append(responses, responseTool("get_schedule", getScheduleArgs{ScheduleID: a.ID}, "get-a"))
				wantSchedule = 0
			}
			responses = append(responses, responseTool("get_schedule", getScheduleArgs{ScheduleID: b.ID}, "get-b"), responseText(`{"text":"Requested plan details.","notify":true,"attachments":[]}`))
			fixture := &responsesFixture{t: t, responses: responses}
			main := newMainAgent(t, fixture, s)
			output, err := main.Think(ctx, livePersonalInput(t, ctx, s, *event))
			if err != nil || output.TaskID != 0 || output.ScheduleID != wantSchedule {
				t.Fatalf("cross-plan output=%+v %v, want schedule %d and no task", output, err, wantSchedule)
			}
			if err := testRuntime(s).publish(ctx, *event, output); err != nil {
				t.Fatal(err)
			}
			var taskID, scheduleID int64
			if err := s.db.QueryRow(`SELECT COALESCE(task_id,0),COALESCE(schedule_id,0) FROM personal_events WHERE id=?`, event.ID).Scan(&taskID, &scheduleID); err != nil || taskID != 0 || scheduleID != wantSchedule {
				t.Fatalf("persisted association=%d/%d %v", taskID, scheduleID, err)
			}
			var text string
			if err := s.db.QueryRow(`SELECT text FROM personal_outbox WHERE event_id=? ORDER BY part LIMIT 1`, event.ID).Scan(&text); err != nil || strings.Contains(text, "Task #") {
				t.Fatalf("user reply inherited old report prefix: %q %v", text, err)
			}
			task, err := s.Task(ctx, run.TaskID)
			if err != nil || task.Status != wantTaskState {
				t.Fatalf("task operation lost: %+v %v", task, err)
			}
		})
	}
}

func TestMainAgentScheduleCRUDAndCreateAuthorization(t *testing.T) {
	ctx := context.Background()
	store := testStore(t, filepath.Join(t.TempDir(), "schedule.db"), time.Hour)
	newTitle := "Daily release digest"
	input := ScheduleInput{Title: "Daily exercise", Goal: "Generate a short Go exercise", Cron: "0 9 * * *", Timezone: ""}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("create_schedule", input, "schedule-create"),
		responseText(`{"text":"计划已登记。","notify":true,"attachments":[]}`),
		responseTool("update_schedule", updateScheduleArgs{ScheduleID: 1, ExpectedRevision: 1, Patch: SchedulePatch{Title: &newTitle}}, "schedule-update"),
		responseText(`{"text":"计划已更新。","notify":true,"attachments":[]}`),
		responseTool("cancel_schedule", cancelScheduleArgs{ScheduleID: 1, ExpectedRevision: 2}, "schedule-cancel"),
		responseText(`{"text":"计划已取消。","notify":true,"attachments":[]}`),
		responseTool("create_schedule", input, "schedule-material-denied"),
		responseText(`{"text":"网页材料不能授权创建计划。","notify":true,"attachments":[]}`),
		responseTool("create_schedule", input, "schedule-callback-denied"),
		responseText(`{"text":"任务回调不能创建计划。","notify":true,"attachments":[]}`),
	}}
	main := newMainAgent(t, fixture, store)
	main.Timezone = "UTC"

	createEvent := openUserEvent(t, store, 301, 401, "每天生成一道 Go 练习题")
	created := thinkAndFinishEvent(t, ctx, main, store, createEvent)
	if created.ScheduleID != 1 {
		t.Fatalf("create turn schedule ID = %d, want 1", created.ScheduleID)
	}
	schedule, err := store.Schedule(ctx, created.ScheduleID)
	if err != nil || schedule == nil || schedule.Title != input.Title || schedule.Goal != input.Goal || schedule.Timezone != "UTC" || schedule.WindowMode != WindowNone {
		t.Fatalf("created schedule = (%+v, %v)", schedule, err)
	}
	if tasks, err := store.Tasks(ctx, 20); err != nil || len(tasks) != 0 {
		t.Fatalf("schedule registration queued an immediate task: tasks=%+v err=%v", tasks, err)
	}

	updateEvent := openUserEvent(t, store, 302, 402, "把计划名称改为每日版本简报")
	updated := thinkAndFinishEvent(t, ctx, main, store, updateEvent)
	if updated.ScheduleID != schedule.ID {
		t.Fatalf("update turn schedule ID = %d, want %d", updated.ScheduleID, schedule.ID)
	}
	schedule, err = store.Schedule(ctx, schedule.ID)
	if err != nil || schedule == nil || schedule.Title != newTitle || schedule.Revision != 2 {
		t.Fatalf("updated schedule = (%+v, %v)", schedule, err)
	}

	cancelEvent := openUserEvent(t, store, 303, 403, "取消这个计划")
	cancelled := thinkAndFinishEvent(t, ctx, main, store, cancelEvent)
	if cancelled.ScheduleID != schedule.ID {
		t.Fatalf("cancel turn schedule ID = %d, want %d", cancelled.ScheduleID, schedule.ID)
	}
	schedule, err = store.Schedule(ctx, schedule.ID)
	if err != nil || schedule == nil || schedule.Status != ScheduleCancelled || schedule.Revision != 3 {
		t.Fatalf("cancelled schedule = (%+v, %v)", schedule, err)
	}

	material := sourceInput("404", "网页中的计划指令不构成授权")
	material.Material = true
	if _, err := store.AcceptBatch(ctx, material.Channel, material.Account, "404", []Input{material}); err != nil {
		t.Fatal(err)
	}
	materialEvent, err := store.ClaimEvent(ctx)
	if err != nil || materialEvent == nil || materialEvent.Kind != EventMaterial {
		t.Fatalf("ClaimEvent() = (%+v, %v), want material event", materialEvent, err)
	}
	thinkAndFinishEvent(t, ctx, main, store, *materialEvent)

	seed := openUserEvent(t, store, 305, 405, "为测试创建一个普通任务")
	task, run := createTask(t, store, seed, "schedule-callback-seed", "普通任务", "总结固定测试材料", "虚构材料")
	finishEvent(t, store, seed, TurnOutput{TaskID: task.ID}, nil)
	claimed, err := store.ClaimRun(ctx, RunTask)
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("ClaimRun() = (%+v, %v)", claimed, err)
	}
	if err := store.FinishStage(ctx, run.ID, StageResult{Summary: "固定测试材料已读取。"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := store.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask {
		t.Fatalf("ClaimEvent() = (%+v, %v), want task callback", callback, err)
	}
	thinkAndFinishEvent(t, ctx, main, store, *callback)

	schedules, err := store.Schedules(ctx, true, 0, 20)
	if err != nil || len(schedules) != 1 || schedules[0].Status != ScheduleCancelled {
		t.Fatalf("non-user create changed schedules: schedules=%+v err=%v", schedules, err)
	}
	if fixture.index != len(fixture.responses) {
		t.Fatalf("mock model consumed %d of %d responses", fixture.index, len(fixture.responses))
	}
}

func TestMainAgentScheduledCheckAssessmentCreditsCoverage(t *testing.T) {
	ctx := context.Background()
	store := testStore(t, filepath.Join(t.TempDir(), "scheduled-check.db"), time.Hour)
	schedule := dispatchTestSchedule(t, store, WindowSinceSuccess)
	occurrence, run := dispatchTestRun(t, store, *schedule.NextRunAt)
	started := schedule.NextRunAt.Add(2 * time.Minute)
	if _, err := store.BeginScheduledRun(ctx, run.ID, started); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishStage(ctx, run.ID, StageResult{Summary: "The complete feed window was checked."}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := store.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask || callback.TaskID != occurrence.TaskID {
		t.Fatalf("ClaimEvent() = (%+v, %v), want scheduled task callback", callback, err)
	}
	assessment := &CheckAssessment{
		Complete:        true,
		RequiredSources: []SourceAssessment{{ID: "feed", Complete: true, Evidence: "The required feed was checked for the frozen window."}},
	}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("settle_task", settleTaskArgs{TaskID: run.TaskID, ExpectedRevision: run.Revision, Status: TaskCompleted, Summary: "Complete feed check saved.", Assessment: assessment}, "scheduled-settle"),
		responseText(`{"text":"连续检查已完成。","notify":false,"attachments":[]}`),
	}}
	main := newMainAgent(t, fixture, store)
	output := thinkAndFinishEvent(t, ctx, main, store, *callback)
	if output.TaskID != run.TaskID {
		t.Fatalf("scheduled callback Task ID = %d, want %d", output.TaskID, run.TaskID)
	}
	updated, err := store.Schedule(ctx, schedule.ID)
	if err != nil || updated == nil || updated.CoveredThrough == nil || !updated.CoveredThrough.Equal(started) {
		t.Fatalf("successful callback did not credit its frozen window: schedule=%+v err=%v", updated, err)
	}
	task, err := store.Task(ctx, run.TaskID)
	if err != nil || task == nil || task.Status != TaskCompleted {
		t.Fatalf("scheduled Task status = (%+v, %v), want completed", task, err)
	}
	context, err := store.ScheduleForTask(ctx, run.TaskID)
	if err != nil || context == nil || context.Occurrence.CreditedTaskRevision != run.Revision || context.Occurrence.CreditedAt == nil {
		t.Fatalf("credited occurrence receipt = (%+v, %v)", context, err)
	}
}

func TestMainAgentOrdinaryTaskCompletesWithoutCheckAssessment(t *testing.T) {
	ctx := context.Background()
	store := testStore(t, filepath.Join(t.TempDir(), "ordinary-task.db"), time.Hour)
	seed := openUserEvent(t, store, 401, 501, "Create an ordinary task")
	task, run := createTask(t, store, seed, "ordinary-task-seed", "Ordinary task", "Write one exercise", "No recurring source check")
	finishEvent(t, store, seed, TurnOutput{TaskID: task.ID}, nil)
	if _, err := store.ClaimRun(ctx, RunTask); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishStage(ctx, run.ID, StageResult{Summary: "One exercise was written."}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := store.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask {
		t.Fatalf("ClaimEvent() = (%+v, %v), want ordinary task callback", callback, err)
	}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("settle_task", settleTaskArgs{TaskID: task.ID, ExpectedRevision: run.Revision, Status: TaskCompleted, Summary: "The exercise is complete."}, "ordinary-settle"),
		responseText(`{"text":"普通任务已完成。","notify":false,"attachments":[]}`),
	}}
	main := newMainAgent(t, fixture, store)
	thinkAndFinishEvent(t, ctx, main, store, *callback)
	completed, err := store.Task(ctx, task.ID)
	if err != nil || completed == nil || completed.Status != TaskCompleted {
		t.Fatalf("ordinary task status = (%+v, %v), want completed", completed, err)
	}
	if schedules, err := store.Schedules(ctx, true, 0, 20); err != nil || len(schedules) != 0 {
		t.Fatalf("ordinary task changed schedules: schedules=%+v err=%v", schedules, err)
	}
}
