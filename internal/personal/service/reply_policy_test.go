package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func policyResponse(text string, kind personal.ReplyKind) testkit.Response {
	raw, _ := json.Marshal(struct {
		Text        string             `json:"text"`
		ReplyKind   personal.ReplyKind `json:"reply_kind"`
		Attachments []string           `json:"attachments"`
	}{Text: text, ReplyKind: kind, Attachments: []string{}})
	return testkit.ResponseText(string(raw))
}

func TestQueuedTaskRepliesWithInitialAnalysisOrWaitsForResult(t *testing.T) {
	const analysis = "你要求离线使用，因此部署是否依赖远程服务会直接影响这个方案能否采用。"
	for _, tc := range []struct {
		name string
		text string
		kind personal.ReplyKind
	}{
		{name: "receipt", text: "已安排分析，稍后给你结果。", kind: personal.ReplyReceipt},
		{name: "silent", kind: personal.ReplySilent},
		{name: "initial analysis", text: analysis, kind: personal.ReplyContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Hour)
			r := testRuntime(s)
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
				testkit.ResponseTool("create_task", map[string]string{"title": "部署方案核对", "goal": "Check whether the supplied deployment supports offline use."}, "create"),
				policyResponse(tc.text, tc.kind),
				testkit.ResponseTool("settle_task", map[string]any{"task_id": 1, "expected_revision": 1, "status": personal.TaskCompleted, "summary": "无法完全离线使用。", "delivery": personal.DeliveryDraft{Text: "这个方案依赖远程服务，无法完全离线使用。", Attachments: []string{}}}, "settle"),
				policyResponse("", personal.ReplySilent),
			}}
			r.Think = testMainAgent(t, fixture, s).Think
			testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "我需要离线使用，请分析这个方案的部署限制：https://example.com/deploy"))
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("initial turn=%v %v", worked, err)
			}
			initial, err := s.ClaimDelivery(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.kind == personal.ReplyContent {
				if initial == nil || initial.Text != analysis {
					t.Fatalf("initial analysis lost: %+v", initial)
				}
				if err := s.UpdateDelivery(ctx, initial.ID, "sent", channel.Receipt{MessageID: "analysis"}, "", time.Time{}); err != nil {
					t.Fatal(err)
				}
			} else if initial != nil {
				t.Fatalf("queued task sent a mechanical reply: %+v", initial)
			}
			if err := s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if extra, err := s.ClaimDelivery(ctx); err != nil || extra != nil {
				t.Fatalf("recovery produced an extra status reply: %+v %v", extra, err)
			}
			r.Execute = func(context.Context, personal.Run, func(string) error) (personal.StageResult, error) {
				return personal.StageResult{Summary: "这个方案依赖远程服务，无法完全离线使用。"}, nil
			}
			if worked, err := r.task(ctx); err != nil || !worked {
				t.Fatalf("task=%v %v", worked, err)
			}
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("final turn=%v %v", worked, err)
			}
			final, err := s.ClaimDelivery(ctx)
			if err != nil || final == nil || !strings.Contains(final.Text, "无法完全离线使用") {
				t.Fatalf("final result lost: %+v %v", final, err)
			}
		})
	}
}

func TestRuntimeSuppressesMisclassifiedQueueReceipt(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Hour)
	r := testRuntime(s)
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_task", map[string]string{"title": "阅读页面", "goal": "Read https://example.com/one", "context": "Read only the supplied page"}, "read"),
		policyResponse("已收到链接，任务已安排，稍后给你结果。", personal.ReplyContent),
	}}
	r.Think = testMainAgent(t, fixture, s).Think
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/one"))
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("turn=%v %v", worked, err)
	}
	d, err := s.ClaimDelivery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("mechanical receipt escaped into outbox: %q", d.Text)
	}
}

func TestRuntimeReportsMisclassifiedPartialFailure(t *testing.T) {
	for _, kind := range []personal.ReplyKind{personal.ReplyContent, personal.ReplyReceipt, personal.ReplySilent} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Hour)
			r := testRuntime(s)
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
				testkit.ResponseTool("create_task", map[string]string{"title": "第一个页面", "goal": "Read https://example.com/one", "context": "Only first link"}, "first"),
				testkit.ResponseTool("create_task", map[string]string{"title": "第二个页面", "context": "https://example.org/two"}, "second"),
				policyResponse("第二个链接未能创建任务，请补充目标；第一个链接继续阅读。", kind),
			}}
			r.Think = testMainAgent(t, fixture, s).Think
			eventID := testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/one https://example.org/two"))
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("turn=%v %v", worked, err)
			}
			tasks, err := s.Tasks(ctx, 10)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("tasks=%+v %v", tasks, err)
			}
			d, err := s.ClaimDelivery(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if d == nil || (!strings.Contains(d.Text, "第二个页面") || !strings.Contains(d.Text, "未能创建任务")) {
				events, err := s.RecentEvents(ctx, 100)
				if err != nil {
					t.Fatal(err)
				}
				var status personal.EventStatus
				for _, event := range events {
					if event.ID == eventID {
						status = event.Status
					}
				}
				t.Fatalf("necessary failure omitted: reply_kind=%s event_status=%s task_count=%d delivery=%+v", kind, status, len(tasks), d)
			}
		})
	}
}

func TestRuntimePublishesSavedResultDespiteReplyKind(t *testing.T) {
	for _, kind := range []personal.ReplyKind{personal.ReplyContent, personal.ReplyReceipt, personal.ReplySilent} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Hour)
			event := testkit.OpenUserEvent(t, s, 1, 1, "https://example.com/one")
			task, _, err := s.CreateTask(ctx, event.ID, "read", "页面阅读", personal.TaskInput{Goal: "Read the supplied page and report the price"})
			if err != nil {
				t.Fatal(err)
			}
			testkit.FinishEvent(t, s, event, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
			run, err := s.ClaimRun(ctx, personal.RunTask)
			if err != nil || run == nil {
				t.Fatalf("run=%+v %v", run, err)
			}
			if err := s.FinishStage(ctx, run.ID, personal.StageResult{Summary: "页面确认月费17元。"}, ""); err != nil {
				t.Fatal(err)
			}
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
				testkit.ResponseTool("settle_task", map[string]any{"task_id": task.ID, "expected_revision": task.Revision, "status": personal.TaskCompleted, "summary": "页面确认月费17元。", "delivery": personal.DeliveryDraft{Text: "页面确认月费17元。", Attachments: []string{}}}, "settle"),
				policyResponse("我会继续处理，有结果再回复。", kind),
			}}
			r := testRuntime(s)
			r.Think = testMainAgent(t, fixture, s).Think
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("turn=%v %v", worked, err)
			}
			final, err := s.Task(ctx, task.ID)
			if err != nil || final.Status != personal.TaskCompleted {
				t.Fatalf("task=%+v %v", final, err)
			}
			d, err := s.ClaimDelivery(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if d == nil || !strings.Contains(d.Text, "17元") {
				t.Fatalf("completed task has no closing result: reply_kind=%s status=%s reply=%+v", kind, final.Status, d)
			}
		})
	}
}

func TestRuntimeAsksRequiredQuestionDespiteReplyKind(t *testing.T) {
	for _, kind := range []personal.ReplyKind{personal.ReplyContent, personal.ReplyReceipt, personal.ReplySilent} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Hour)
			r := testRuntime(s)
			const question = "第二个链接需要登录，请提供可公开访问的版本。"
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
				testkit.ResponseTool("create_task", map[string]string{"title": "第一个页面", "goal": "Read https://example.com/one"}, "first"),
				testkit.ResponseTool("set_user_questions", map[string]any{"questions": []string{question}}, "question"),
				policyResponse("已收到，稍后回复。", kind),
			}}
			r.Think = testMainAgent(t, fixture, s).Think
			testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/one https://example.org/two"))
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("turn=%v %v", worked, err)
			}
			d, err := s.ClaimDelivery(ctx)
			if err != nil || d == nil || !strings.Contains(d.Text, question) || strings.Contains(d.Text, "已收到") {
				t.Fatalf("required question lost: %+v %v", d, err)
			}
		})
	}
}

func TestRuntimeRecoveredActionDoesNotForceAReceipt(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Hour)
	r := testRuntime(s)
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_task", map[string]string{"action_id": "read-page", "title": "页面阅读"}, "failed"),
		testkit.ResponseTool("create_task", map[string]string{"action_id": "read-page", "title": "页面阅读", "goal": "Read https://example.com/one"}, "corrected"),
		policyResponse("任务已创建，稍后回复。", personal.ReplyContent),
	}}
	r.Think = testMainAgent(t, fixture, s).Think
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/one"))
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("turn=%v %v", worked, err)
	}
	d, err := s.ClaimDelivery(ctx)
	if err != nil || d != nil {
		t.Fatalf("corrected action still sent receipt: %+v %v", d, err)
	}
}

func TestRuntimeKeepsFailureWhenAnotherActionSucceeds(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Hour)
	r := testRuntime(s)
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_task", map[string]string{"action_id": "one", "title": "One", "goal": "Read https://example.com/one"}, "first"),
		testkit.ResponseTool("create_task", map[string]string{"action_id": "two", "title": "Two"}, "second"),
		testkit.ResponseTool("create_task", map[string]string{"action_id": "three", "title": "Three", "goal": "Read https://example.com/three"}, "third"),
		policyResponse("", personal.ReplySilent),
	}}
	r.Think = testMainAgent(t, fixture, s).Think
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/one https://example.com/two https://example.com/three"))
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("turn=%v %v", worked, err)
	}
	d, err := s.ClaimDelivery(ctx)
	if err != nil || d == nil || !strings.Contains(d.Text, "Two") || !strings.Contains(d.Text, "未能创建任务") {
		t.Fatalf("another action hid failure: %+v %v", d, err)
	}
}

func TestQueuedLinkDoesNotDeliverDraftOrUnrequestedAttachment(t *testing.T) {
	for _, draft := range []string{
		"已收到，正在后台处理。",
		`{"text":"已安排阅读。","reply_kind":"content","attachments":["unrequested-file"]}`,
	} {
		t.Run(draft, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Hour)
			r := testRuntime(s)
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
				testkit.ResponseTool("create_task", map[string]string{"title": "页面阅读", "goal": "Read https://example.com/one"}, "read"),
				testkit.ResponseText(draft),
			}}
			r.Think = testMainAgent(t, fixture, s).Think
			testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/one"))
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("turn=%v %v", worked, err)
			}
			if d, err := s.ClaimDelivery(ctx); err != nil || d != nil {
				t.Fatalf("unused draft caused a notification: %+v %v", d, err)
			}
			if len(fixture.Requests) != 2 {
				t.Fatalf("unused intake reply triggered formatting: %d requests", len(fixture.Requests))
			}
		})
	}
}

func TestTaskFailureStillReportedWhenFinalModelResponseFails(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Hour)
	r := testRuntime(s)
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_task", map[string]string{"title": "页面阅读"}, "failed"),
		testkit.ResponseRefusal(),
	}}
	r.Think = testMainAgent(t, fixture, s).Think
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/one"))
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("turn=%v %v", worked, err)
	}
	if d, err := s.ClaimDelivery(ctx); err != nil || d == nil || !strings.Contains(d.Text, "页面阅读") || !strings.Contains(d.Text, "未能创建任务") {
		t.Fatalf("final model failure hid the task failure: %+v %v", d, err)
	}
}
