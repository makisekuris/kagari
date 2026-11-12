package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestMainAgentScheduleReplyDoesNotInheritAnUnrelatedTask(t *testing.T) {
	for _, name := range []string{"other plan", "task and other plan", "multiple plans"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := testkit.Store(t, ":memory:", time.Hour)
			seed := testkit.OpenUserEvent(t, store, 1, 1, "Register two plans")
			input := personal.ScheduleInput{Title: "Plan A", Goal: "Write an exercise", Cron: "0 * * * *", Timezone: "UTC"}
			a, _, err := store.CreateSchedule(ctx, seed.ID, "a", input, testkit.ScheduleNow())
			if err != nil {
				t.Fatal(err)
			}
			input.Title = "Plan B"
			b, _, err := store.CreateSchedule(ctx, seed.ID, "b", input, testkit.ScheduleNow())
			if err != nil {
				t.Fatal(err)
			}
			testkit.FinishEvent(t, store, seed, personal.TurnOutput{}, nil)
			_, run := testkit.DispatchRun(t, store, *a.NextRunAt)
			started := a.NextRunAt.Add(time.Minute)
			if _, err := store.BeginScheduledRun(ctx, run.ID, started); err != nil {
				t.Fatal(err)
			}
			callback := testkit.FinishRun(t, store, run)
			if _, err := store.SettleTask(ctx, callback.ID, run.TaskID, run.Revision, personal.TaskWaitingInput, "Need input"); err != nil {
				t.Fatal(err)
			}
			testkit.FinishEvent(t, store, callback, personal.TurnOutput{TaskID: run.TaskID, Text: "Need input"}, []string{"Need input"})
			delivery, err := store.ClaimDelivery(ctx)
			if err != nil || delivery == nil {
				t.Fatalf("original result=%+v %v", delivery, err)
			}
			if err := store.UpdateDelivery(ctx, delivery.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "plan-a-result"}, "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			message := testkit.SourceInput("cross-plan-reply", "Query the requested plans; cancel this Task if specified")
			message.ReplyToID = "plan-a-result"
			testkit.AcceptInput(t, store, "cross-plan", message)
			event, err := store.ClaimEvent(ctx)
			if err != nil || event == nil || event.TaskID != run.TaskID || event.ScheduleID != a.ID {
				t.Fatalf("reply association=%+v %v", event, err)
			}
			var responses []testkit.Response
			wantSchedule, wantTaskState := b.ID, personal.TaskWaitingInput
			switch name {
			case "task and other plan":
				responses = append(responses, testkit.ResponseTool("cancel_task", map[string]any{"task_id": run.TaskID, "expected_revision": run.Revision}, "cancel-a-task"))
				wantSchedule, wantTaskState = 0, personal.TaskCancelled
			case "multiple plans":
				responses = append(responses, testkit.ResponseTool("get_schedule", map[string]any{"schedule_id": a.ID}, "get-a"))
				wantSchedule = 0
			}
			responses = append(responses,
				testkit.ResponseTool("get_schedule", map[string]any{"schedule_id": b.ID}, "get-b"),
				testkit.ResponseText(`{"text":"Requested plan details.","reply_kind":"content","attachments":[]}`),
			)
			fixture := &testkit.ResponsesFixture{T: t, Responses: responses}
			main := testMainAgent(t, fixture, store)
			output, err := main.Think(ctx, personal.TurnInput{Event: *event})
			if err != nil || output.TaskID != 0 || output.ScheduleID != wantSchedule {
				t.Fatalf("cross-plan output=%+v %v, want schedule %d and no task", output, err, wantSchedule)
			}
			if err := testRuntime(store).publish(ctx, *event, output); err != nil {
				t.Fatal(err)
			}
			messages, err := store.History(ctx, event.ID+1, 100)
			if err != nil {
				t.Fatal(err)
			}
			var userMessage, assistantReply *personal.Message
			for i := range messages {
				if messages[i].EventID != event.ID {
					continue
				}
				switch messages[i].Role {
				case "user":
					userMessage = &messages[i]
				case "assistant":
					assistantReply = &messages[i]
				}
			}
			if userMessage == nil || assistantReply == nil || userMessage.TaskID != 0 || userMessage.ScheduleID != wantSchedule || assistantReply.TaskID != 0 || assistantReply.ScheduleID != wantSchedule {
				t.Fatalf("persisted association user=%+v assistant=%+v, want no task and schedule %d", userMessage, assistantReply, wantSchedule)
			}
			published, err := store.ClaimDelivery(ctx)
			if err != nil || published == nil || strings.Contains(published.Text, "任务 #") {
				t.Fatalf("reply inherited old report prefix: %+v %v", published, err)
			}
			if extra, err := store.ClaimDelivery(ctx); err != nil || extra != nil {
				t.Fatalf("unexpected additional delivery=%+v %v", extra, err)
			}
			task, err := store.Task(ctx, run.TaskID)
			if err != nil || task == nil || task.Status != wantTaskState {
				t.Fatalf("task operation lost: %+v %v", task, err)
			}
		})
	}
}

func TestMainAgentScheduleMutationOwnsReplyAssociation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
	}{
		{name: "read then create then read", steps: []string{"read", "create", "read"}},
		{name: "create then read", steps: []string{"create", "read"}},
		{name: "read then create", steps: []string{"read", "create"}},
		{name: "multiple changed plans are ambiguous", steps: []string{"create", "create", "read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testkit.Store(t, ":memory:", time.Hour)
			oldEvent, oldSchedule := testkit.CreateSchedule(t, store, 1, 101, "old-plan", personal.ScheduleInput{
				Title: "Old plan", Goal: "Review old plan", Cron: "0 * * * *", Timezone: "UTC",
			}, testkit.ScheduleNow())
			testkit.FinishEvent(t, store, oldEvent, personal.TurnOutput{ScheduleID: oldSchedule.ID}, nil)

			event := testkit.OpenUserEvent(t, store, 2, 102, "Register the requested plan")
			newSchedule := personal.ScheduleInput{Title: "New plan", Goal: "Review the new plan", Cron: "0 * * * *", Timezone: "UTC"}
			var responses []testkit.Response
			created, read := 0, 0
			for _, step := range tc.steps {
				switch step {
				case "read":
					read++
					callID := []string{"read-old-1", "read-old-2"}[read-1]
					responses = append(responses, testkit.ResponseTool("get_schedule", map[string]any{"schedule_id": oldSchedule.ID}, callID))
				case "create":
					created++
					input := newSchedule
					if created == 2 {
						input.Title = "Another new plan"
					}
					responses = append(responses, testkit.ResponseTool("create_schedule", input, []string{"create-new", "create-another"}[created-1]))
				}
			}
			responses = append(responses, testkit.ResponseText(`{"text":"The plan request was handled.","reply_kind":"content","attachments":[]}`))
			fixture := &testkit.ResponsesFixture{T: t, Responses: responses}
			main := testMainAgent(t, fixture, store)
			output, err := main.Think(ctx, personal.TurnInput{Event: event})
			if err != nil {
				t.Fatal(err)
			}
			var wantSchedule int64
			if created == 1 {
				schedules, err := store.Schedules(ctx, true, 0, 20)
				if err != nil {
					t.Fatal(err)
				}
				for _, schedule := range schedules {
					if schedule.CreatedEventID == event.ID {
						wantSchedule = schedule.ID
						break
					}
				}
				if wantSchedule == 0 {
					t.Fatal("created schedule was not associated with the current event")
				}
			}
			if output.TaskID != 0 || output.ScheduleID != wantSchedule {
				t.Fatalf("output=%+v, want schedule %d and no task", output, wantSchedule)
			}
			if len(fixture.Requests) != len(fixture.Responses) {
				t.Fatalf("model consumed %d of %d fixture responses", len(fixture.Requests), len(fixture.Responses))
			}

			if err := testRuntime(store).publish(ctx, event, output); err != nil {
				t.Fatal(err)
			}
			messages, err := store.History(ctx, event.ID+1, 100)
			if err != nil {
				t.Fatal(err)
			}
			var userMessage, assistantReply *personal.Message
			for i := range messages {
				if messages[i].EventID != event.ID {
					continue
				}
				switch messages[i].Role {
				case "user":
					userMessage = &messages[i]
				case "assistant":
					assistantReply = &messages[i]
				}
			}
			if userMessage == nil || assistantReply == nil || userMessage.ScheduleID != wantSchedule || assistantReply.ScheduleID != wantSchedule {
				t.Fatalf("published history association user=%+v assistant=%+v, want schedule %d", userMessage, assistantReply, wantSchedule)
			}
			outbox, err := store.ClaimDelivery(ctx)
			if err != nil || outbox == nil || outbox.Text != output.Text {
				t.Fatalf("published outbox=%+v error=%v, want one exact response", outbox, err)
			}
			if extra, err := store.ClaimDelivery(ctx); err != nil || extra != nil {
				t.Fatalf("unexpected additional outbox=%+v error=%v", extra, err)
			}
		})
	}
}
