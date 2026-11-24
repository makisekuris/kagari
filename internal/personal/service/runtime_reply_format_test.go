package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

const (
	initialFormatDraft = "initial model draft must not be shown"
	repairFormatDraft  = "repair model draft must not be shown"
)

func TestRuntimeReplyFormatFailurePublishesValidationReason(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		before      []testkit.Response
	}{
		{name: "ordinary reply", input: "请核对这条资料。"},
		{name: "committed schedule receipt", input: "每天早上九点安排晨读。", before: []testkit.Response{testkit.ResponseTool("create_schedule", personal.ScheduleInput{
			Title: "晨读", Goal: "读一篇文章", Cron: "0 9 * * *", Timezone: "Asia/Shanghai",
		}, "format-failure-schedule")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Minute)
			responses := append(tc.before, badFormatResponses()...)
			fixture := &testkit.ResponsesFixture{T: t, Responses: responses}
			r := testRuntime(s)
			r.Think = testMainAgent(t, fixture, s).Think
			r.PresentReply = func(context.Context, personal.Event, string) (string, error) {
				t.Fatal("validation failure was sent through reply presentation")
				return "", nil
			}
			eventID := testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", tc.input))
			if worked, err := r.turn(ctx); !worked || err != nil {
				t.Fatalf("turn = %t, %v", worked, err)
			}
			message := testkit.AssistantMessage(t, s, eventID)
			assertFormatReason(t, message.Text)
			_, items := deliveryFactsForEvent(t, s, eventID)
			if len(items) != 1 || items[0].Text != message.Text {
				t.Fatalf("outbox = %+v, want the same single reply as history", items)
			}
			if len(tc.before) > 0 {
				if message.ScheduleID != 1 || !strings.Contains(message.Text, "计划 #1「晨读」已启用") {
					t.Fatalf("missing committed schedule receipt or scope: %+v", message)
				}
				plans, err := s.Schedules(ctx, false, 0, 20)
				if err != nil || len(plans) != 1 || plans[0].ID != 1 {
					t.Fatalf("committed schedules = %+v, %v", plans, err)
				}
				if worked, err := r.turn(ctx); worked || err != nil {
					t.Fatalf("event replay = %t, %v", worked, err)
				}
			}
			if len(fixture.Requests) != len(tc.before)+2 {
				t.Fatalf("model requests = %d, want %d", len(fixture.Requests), len(tc.before)+2)
			}
		})
	}

	t.Run("committed control and extra query", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, ":memory:", time.Minute)
		seed := testkit.OpenUserEvent(t, s, 1, 1, "创建待取消的研究任务。")
		task, _ := testkit.CreateTask(t, s, seed, "format-control-task", "旧研究", "Summarize the existing research", "")
		testkit.FinishEvent(t, s, seed, personal.TurnOutput{Silent: true}, nil)
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("cancel_task", map[string]any{"task_id": task.ID, "expected_revision": task.Revision}, "format-control-cancel"),
			testkit.ResponseTool("list_schedules", map[string]any{}, "format-control-query"),
		}}
		fixture.Responses = append(fixture.Responses, badFormatResponses()...)
		r := testRuntime(s)
		r.Think = testMainAgent(t, fixture, s).Think
		r.PresentReply = func(context.Context, personal.Event, string) (string, error) {
			t.Fatal("committed control or validation feedback was sent through reply presentation")
			return "", nil
		}
		eventID := testkit.AcceptInput(t, s, "2", testkit.SourceInput("2", "取消旧研究，并查一下定时计划。"))
		if worked, err := r.turn(ctx); !worked || err != nil {
			t.Fatalf("turn = %t, %v", worked, err)
		}
		intents, items := deliveryFactsForEvent(t, s, eventID)
		if len(intents) != 2 || len(items) != 2 || len(intents[0].Scopes) != 1 || intents[0].Scopes[0].TaskID != task.ID {
			t.Fatalf("committed control and feedback = intents:%+v outbox:%+v", intents, items)
		}
		assertFormatReason(t, items[1].Text)
		if strings.Contains(items[0].Text, "reply_kind") || strings.Contains(items[0].Text, "缺少") {
			t.Fatalf("supplement validation reason overwrote the committed control receipt: %q", items[0].Text)
		}
		if len(fixture.Requests) != 4 {
			t.Fatalf("model requests = %d, want two tools, final draft, and repair", len(fixture.Requests))
		}
	})
}

func badFormatResponses() []testkit.Response {
	return []testkit.Response{
		testkit.ResponseText(`{"text":"` + initialFormatDraft + `","attachments":[]}`),
		testkit.ResponseText(`{"text":"` + repairFormatDraft + `","attachments":[]}`),
	}
}

func assertFormatReason(t *testing.T, visible string) {
	t.Helper()
	if !strings.Contains(visible, "reply_kind") || !strings.Contains(visible, "缺少") {
		t.Fatalf("visible output omits the missing reply_kind validation reason: %q", visible)
	}
	if strings.Contains(visible, initialFormatDraft) || strings.Contains(visible, repairFormatDraft) {
		t.Fatalf("model draft escaped to the user: %q", visible)
	}
}
