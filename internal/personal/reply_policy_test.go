package personal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func policyResponse(text string, kind personalReplyKind) fixtureResponse {
	raw, _ := json.Marshal(personalTurnResponse{Text: text, ReplyKind: kind, Attachments: []string{}})
	return responseText(string(raw))
}

func TestRuntimeSuppressesMisclassifiedQueueReceipt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	r := testRuntime(s)
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("create_task", createTaskArgs{Title: "阅读页面", Goal: "Read https://example.com/one", Context: "Read only the supplied page"}, "read"),
		policyResponse("已收到链接，任务已安排，稍后给你结果。", replyContent),
	}}
	r.Think = newMainAgent(t, fixture, s).Think
	eventID := acceptInput(t, s, "1", sourceInput("1", "https://example.com/one"))
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("turn=%v %v", worked, err)
	}
	var hasContent int
	if err := s.db.QueryRow(`SELECT has_content FROM personal_reply_groups WHERE id=?`, eventID).Scan(&hasContent); err != nil {
		t.Fatal(err)
	}
	d, err := s.ClaimDelivery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("mechanical receipt escaped into outbox, has_content=%d: %q", hasContent, d.Text)
	}
}

func TestRuntimeReportsMisclassifiedPartialFailure(t *testing.T) {
	for _, kind := range []personalReplyKind{replyContent, replyReceipt, replySilent} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Hour)
			r := testRuntime(s)
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{
				responseTool("create_task", createTaskArgs{Title: "第一个页面", Goal: "Read https://example.com/one", Context: "Only first link"}, "first"),
				responseTool("create_task", createTaskArgs{Title: "第二个页面", Context: "https://example.org/two"}, "second"),
				policyResponse("第二个链接未能创建任务，请补充目标；第一个链接继续阅读。", kind),
			}}
			r.Think = newMainAgent(t, fixture, s).Think
			eventID := acceptInput(t, s, "1", sourceInput("1", "https://example.com/one https://example.org/two"))
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
				var status string
				if err := s.db.QueryRow(`SELECT status FROM personal_events WHERE id=?`, eventID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("necessary failure omitted: reply_kind=%s event_status=%s task_count=%d delivery=%+v", kind, status, len(tasks), d)
			}
		})
	}
}

func TestRuntimePublishesSavedResultDespiteReplyKind(t *testing.T) {
	for _, kind := range []personalReplyKind{replyContent, replyReceipt, replySilent} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Hour)
			event := openUserEvent(t, s, 1, 1, "https://example.com/one")
			task, _, err := s.CreateTask(ctx, event.ID, "read", "页面阅读", TaskInput{Goal: "Read the supplied page and report the price"})
			if err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, event, TurnOutput{TaskID: task.ID, Silent: true}, nil)
			run, err := s.ClaimRun(ctx, RunTask)
			if err != nil || run == nil {
				t.Fatalf("run=%+v %v", run, err)
			}
			if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "页面确认月费17元。"}, ""); err != nil {
				t.Fatal(err)
			}
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{
				responseTool("settle_task", settleTaskArgs{TaskID: task.ID, ExpectedRevision: task.Revision, Status: TaskCompleted, Summary: "页面确认月费17元。"}, "settle"),
				policyResponse("我会继续处理，有结果再回复。", kind),
			}}
			r := testRuntime(s)
			r.Think = newMainAgent(t, fixture, s).Think
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("turn=%v %v", worked, err)
			}
			final, err := s.Task(ctx, task.ID)
			if err != nil || final.Status != TaskCompleted {
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
	for _, kind := range []personalReplyKind{replyContent, replyReceipt, replySilent} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Hour)
			r := testRuntime(s)
			const question = "第二个链接需要登录，请提供可公开访问的版本。"
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{
				responseTool("create_task", createTaskArgs{Title: "第一个页面", Goal: "Read https://example.com/one"}, "first"),
				responseTool("set_user_questions", setUserQuestionsArgs{Questions: &[]string{question}}, "question"),
				policyResponse("已收到，稍后回复。", kind),
			}}
			r.Think = newMainAgent(t, fixture, s).Think
			acceptInput(t, s, "1", sourceInput("1", "https://example.com/one https://example.org/two"))
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
	s := testStore(t, ":memory:", time.Hour)
	r := testRuntime(s)
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("create_task", createTaskArgs{ActionID: "read-page", Title: "页面阅读"}, "failed"),
		responseTool("create_task", createTaskArgs{ActionID: "read-page", Title: "页面阅读", Goal: "Read https://example.com/one"}, "corrected"),
		policyResponse("任务已创建，稍后回复。", replyContent),
	}}
	r.Think = newMainAgent(t, fixture, s).Think
	acceptInput(t, s, "1", sourceInput("1", "https://example.com/one"))
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
	s := testStore(t, ":memory:", time.Hour)
	r := testRuntime(s)
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("create_task", createTaskArgs{ActionID: "one", Title: "One", Goal: "Read https://example.com/one"}, "first"),
		responseTool("create_task", createTaskArgs{ActionID: "two", Title: "Two"}, "second"),
		responseTool("create_task", createTaskArgs{ActionID: "three", Title: "Three", Goal: "Read https://example.com/three"}, "third"),
		policyResponse("", replySilent),
	}}
	r.Think = newMainAgent(t, fixture, s).Think
	acceptInput(t, s, "1", sourceInput("1", "https://example.com/one https://example.com/two https://example.com/three"))
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
			s := testStore(t, ":memory:", time.Hour)
			r := testRuntime(s)
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{
				responseTool("create_task", createTaskArgs{Title: "页面阅读", Goal: "Read https://example.com/one"}, "read"),
				responseText(draft),
			}}
			r.Think = newMainAgent(t, fixture, s).Think
			acceptInput(t, s, "1", sourceInput("1", "https://example.com/one"))
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("turn=%v %v", worked, err)
			}
			if d, err := s.ClaimDelivery(ctx); err != nil || d != nil {
				t.Fatalf("unused draft caused a notification: %+v %v", d, err)
			}
			if len(fixture.requests) != 2 {
				t.Fatalf("unused intake reply triggered formatting: %d requests", len(fixture.requests))
			}
		})
	}
}

func TestTaskFailureStillReportedWhenFinalModelResponseFails(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	r := testRuntime(s)
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("create_task", createTaskArgs{Title: "页面阅读"}, "failed"),
		responseRefusal(),
	}}
	r.Think = newMainAgent(t, fixture, s).Think
	acceptInput(t, s, "1", sourceInput("1", "https://example.com/one"))
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("turn=%v %v", worked, err)
	}
	if d, err := s.ClaimDelivery(ctx); err != nil || d == nil || !strings.Contains(d.Text, "页面阅读") || !strings.Contains(d.Text, "未能创建任务") {
		t.Fatalf("final model failure hid the task failure: %+v %v", d, err)
	}
}
