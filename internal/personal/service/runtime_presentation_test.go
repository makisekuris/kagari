package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRuntimePublishPresentationContract(t *testing.T) {
	t.Run("final text is presented once and persisted consistently", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "presented.db"), time.Minute)
		event := testkit.OpenUserEvent(t, s, 1, 1, "Check the result.")
		task, _ := testkit.CreateTask(t, s, event, "task", "Result", "Check the result.", "")
		file, err := s.SaveAttachment(ctx, "result.txt", "text/plain", strings.NewReader("result"))
		if err != nil {
			t.Fatal(err)
		}
		r := testRuntime(s)
		r.Prepare = channel.PrepareText
		draft := "Internal draft with a promise and https://example.com/result."
		presented := "Confirmed: `**literal**` https://example.com/result. " + strings.Repeat("evidence ", 400)
		calls := 0
		r.PresentReply = func(_ context.Context, gotEvent personal.Event, gotDraft string) (string, error) {
			calls++
			if gotEvent.ID != event.ID || gotDraft != draft {
				t.Fatalf("presenter input event=%+v draft=%q", gotEvent, gotDraft)
			}
			return presented, nil
		}
		if err := r.publish(ctx, event, personal.TurnOutput{Text: draft, TaskID: task.ID, Attachments: []string{file.ID}}); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("presenter calls = %d, want 1", calls)
		}
		want := channel.PlainTextMarkdown(presented)
		if !strings.Contains(want, "**literal**") {
			t.Fatalf("plain-text rendering lost protected literal: %q", want[:min(len(want), 80)])
		}
		var outbox strings.Builder
		snapshot, err := s.Diagnostic(ctx, event.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		parts, attachmentIDs := 0, 0
		for _, delivery := range snapshot.Deliveries {
			if delivery.EventID != event.ID {
				continue
			}
			parts++
			outbox.WriteString(delivery.Text)
			if delivery.AttachmentID != "" {
				attachmentIDs++
				if delivery.AttachmentID != file.ID {
					t.Errorf("outbox attachment ID = %q, want %q", delivery.AttachmentID, file.ID)
				}
			}
		}
		if parts < 3 || attachmentIDs != 1 || outbox.String() != want {
			t.Fatalf("outbox parts=%d attachments=%d text matches=%t", parts, attachmentIDs, outbox.String() == want)
		}
		message := testkit.AssistantMessage(t, s, event.ID)
		if message.Text != want || message.TaskID != task.ID || len(message.Attachments) != 1 || message.Attachments[0].ID != file.ID {
			t.Fatalf("final history lost reply identity/content: %+v", message)
		}
		var eventTaskID int64
		for _, stored := range snapshot.Events {
			if stored.ID == event.ID {
				eventTaskID = stored.TaskID
			}
		}
		if eventTaskID != task.ID {
			t.Fatalf("event task ID = %d; want %d", eventTaskID, task.ID)
		}
	})

	t.Run("silent and attachment-only replies skip presentation", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "invisible.db"), time.Minute)
		file, err := s.SaveAttachment(ctx, "only.txt", "text/plain", strings.NewReader("file"))
		if err != nil {
			t.Fatal(err)
		}
		r := testRuntime(s)
		calls := 0
		r.PresentReply = func(context.Context, personal.Event, string) (string, error) {
			calls++
			return "unexpected", nil
		}
		attachmentEvent := testkit.OpenUserEvent(t, s, 1, 1, "Send the file.")
		task, _ := testkit.CreateTask(t, s, attachmentEvent, "attachment", "Attachment", "Send the file.", "")
		if err := r.publish(ctx, attachmentEvent, personal.TurnOutput{TaskID: task.ID, Attachments: []string{file.ID}}); err != nil {
			t.Fatal(err)
		}
		delivery, err := s.ClaimDelivery(ctx)
		if err != nil || delivery == nil || delivery.AttachmentID != file.ID {
			t.Fatalf("attachment-only delivery = %+v, %v", delivery, err)
		}
		silentEvent := testkit.OpenUserEvent(t, s, 2, 2, "Continue quietly.")
		if err := r.publish(ctx, silentEvent, personal.TurnOutput{TaskID: task.ID, Silent: true}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := s.Diagnostic(ctx, silentEvent.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, delivery := range snapshot.Deliveries {
			if delivery.EventID == silentEvent.ID {
				t.Fatalf("silent reply queued a delivery: %+v", delivery)
			}
		}
		if calls != 0 {
			t.Fatalf("presenter calls for silent/attachment-only output = %d", calls)
		}
	})

	t.Run("presenter failure falls back without replaying Think", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "fallback.db"), time.Minute)
		r := testRuntime(s)
		var thinkCalls int
		const draft = "The committed result is **complete**: https://example.com/final."
		r.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
			thinkCalls++
			return personal.TurnOutput{Text: draft}, nil
		}
		r.PresentReply = func(context.Context, personal.Event, string) (string, error) {
			return "", errors.New("fixture presenter failure")
		}
		eventID := testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "Please check the result."))
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("turn = %v, %v", worked, err)
		}
		if thinkCalls != 1 {
			t.Fatalf("Think calls = %d, want 1", thinkCalls)
		}
		want := channel.PlainTextMarkdown(draft)
		delivery, err := s.ClaimDelivery(ctx)
		message := testkit.AssistantMessage(t, s, eventID)
		if err != nil || delivery == nil || delivery.Text != want || message.Text != want {
			t.Fatalf("fallback changed committed result: delivery=%+v history=%q, %v", delivery, message.Text, err)
		}
	})

	t.Run("presenter cannot add a source URL", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "added-url.db"), time.Minute)
		r := testRuntime(s)
		const draft = "我已经安排后台核对，结果尚未出炉。"
		r.PresentReply = func(context.Context, personal.Event, string) (string, error) {
			return "已经核对完毕：https://example.com/invented", nil
		}
		event := testkit.OpenUserEvent(t, s, 1, 1, "请核对资料。")
		if err := r.publish(ctx, event, personal.TurnOutput{Text: draft}); err != nil {
			t.Fatal(err)
		}
		delivery, err := s.ClaimDelivery(ctx)
		if err != nil || delivery == nil || delivery.Text != channel.PlainTextMarkdown(draft) || strings.Contains(delivery.Text, "example.com/invented") {
			t.Fatalf("presenter-added source escaped fallback: %+v, %v", delivery, err)
		}
	})
}

func TestRuntimePresentationRespectsReplyKindAndTerminalCallback(t *testing.T) {
	t.Run("direct-user receipt bypasses semantic presentation", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "receipt.db"), time.Minute)
		request := "请重新阅读并比较这三篇资料：https://example.com/one https://example.org/two https://example.net/three"
		testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", request))
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("create_task", map[string]any{"title": "三篇资料比较", "goal": "Read and compare the three supplied references."}, "create"),
			testkit.ResponseText(`{"text":"已安排后台重新阅读，完成后回复你。","reply_kind":"receipt","attachments":[]}`),
		}}
		r := testRuntime(s)
		presenterCalls := 0
		r.PresentReply = func(context.Context, personal.Event, string) (string, error) {
			presenterCalls++
			return "三篇资料已阅读并比较完毕。", nil
		}
		r.Think = testMainAgent(t, fixture, s).Think
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("turn = %v, %v", worked, err)
		}
		if presenterCalls != 0 {
			t.Fatalf("receipt invoked semantic presenter %d times", presenterCalls)
		}
		delivery, err := s.ClaimDelivery(ctx)
		if err != nil || delivery == nil || delivery.Text != "已安排后台重新阅读，完成后回复你。" {
			t.Fatalf("receipt changed before delivery: %+v, %v", delivery, err)
		}
		tasks, err := s.Tasks(ctx, 10)
		if err != nil || len(tasks) != 1 || tasks[0].Status != personal.TaskQueued {
			t.Fatalf("queued task state = %+v, %v", tasks, err)
		}
	})

	t.Run("schedule receipt without deferred work is presented", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "schedule-receipt.db"), time.Minute)
		testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "请登记每天 09:00 UTC 的练习计划。"))
		budgetMS := (10 * time.Minute).Milliseconds()
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("create_schedule", personal.ScheduleInput{Title: "每日练习", Goal: "生成一道 Go 练习题", Cron: "0 9 * * *", Timezone: "UTC", WindowMode: personal.WindowNone, TaskBudgetMS: &budgetMS}, "schedule"),
			testkit.ResponseText(`{"text":"每日练习已登记，独立运行，时限十分钟。","reply_kind":"receipt","attachments":[]}`),
		}}
		r := testRuntime(s)
		presenterCalls := 0
		r.PresentReply = func(_ context.Context, event personal.Event, draft string) (string, error) {
			presenterCalls++
			if event.Kind != personal.EventUser {
				t.Fatalf("presenter event kind = %q", event.Kind)
			}
			return "整理：" + draft, nil
		}
		r.Think = testMainAgent(t, fixture, s).Think
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("schedule turn = %v, %v", worked, err)
		}
		if presenterCalls != 1 {
			t.Fatalf("schedule receipt presenter calls = %d, want 1", presenterCalls)
		}
		plans, err := s.Schedules(ctx, false, 0, 10)
		if err != nil || len(plans) != 1 {
			t.Fatalf("created schedules = %+v, %v", plans, err)
		}
		tasks, err := s.Tasks(ctx, 10)
		if err != nil || len(tasks) != 0 {
			t.Fatalf("schedule created background tasks = %+v, %v", tasks, err)
		}
		delivery, err := s.ClaimDelivery(ctx)
		if err != nil || delivery == nil || !strings.HasPrefix(delivery.Text, "整理：") {
			t.Fatalf("synchronous schedule receipt was not presented: %+v, %v", delivery, err)
		}
	})

	t.Run("content is presented and terminal callback ignores an old receipt kind", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "content-callback.db"), time.Minute)
		r := testRuntime(s)
		var presenterCalls int
		var presenterEvents []personal.EventKind
		r.PresentReply = func(_ context.Context, event personal.Event, draft string) (string, error) {
			presenterCalls++
			presenterEvents = append(presenterEvents, event.Kind)
			return "整理：" + draft, nil
		}

		testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "解释 deadline 的含义。"))
		contentFixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseText(`{"text":"deadline 是完成任务的最后期限。","reply_kind":"content","attachments":[]}`),
		}}
		r.Think = testMainAgent(t, contentFixture, s).Think
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("content turn = %v, %v", worked, err)
		}
		contentDelivery, err := s.ClaimDelivery(ctx)
		if err != nil || contentDelivery == nil || !strings.HasPrefix(contentDelivery.Text, "整理：") {
			t.Fatalf("content was not presented: %+v, %v", contentDelivery, err)
		}

		seed := testkit.OpenUserEvent(t, s, 2, 2, "核对账户价格。")
		task, run := testkit.CreateTask(t, s, seed, "create", "价格核对", "Read the account pricing page.", "")
		testkit.FinishEvent(t, s, seed, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
		claimed, err := s.ClaimRun(ctx, personal.RunTask)
		if err != nil || claimed == nil || claimed.ID != run.ID {
			t.Fatalf("initial run = %+v, %v", claimed, err)
		}
		if err := s.FinishStage(ctx, run.ID, personal.StageResult{Summary: "页面确认月费为 17 元。"}, ""); err != nil {
			t.Fatal(err)
		}
		callbackFixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("settle_task", map[string]any{"task_id": task.ID, "expected_revision": task.Revision, "status": personal.TaskCompleted, "summary": "页面确认月费为 17 元。"}, "settle"),
			testkit.ResponseText(`{"text":"已收到，后台会继续处理。","reply_kind":"receipt","attachments":[]}`),
		}}
		r.Think = testMainAgent(t, callbackFixture, s).Think
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("terminal callback turn = %v, %v", worked, err)
		}
		callbackDelivery, err := s.ClaimDelivery(ctx)
		if err != nil || callbackDelivery == nil || !strings.Contains(callbackDelivery.Text, "整理：") || !strings.Contains(callbackDelivery.Text, "17 元") || strings.Contains(callbackDelivery.Text, "会继续处理") {
			t.Fatalf("terminal callback was not presented from committed result: %+v, %v", callbackDelivery, err)
		}
		if presenterCalls != 2 || len(presenterEvents) != 2 || presenterEvents[0] != personal.EventUser || presenterEvents[1] != personal.EventTask {
			t.Fatalf("presenter calls/events = %d/%v, want one direct content and one terminal callback", presenterCalls, presenterEvents)
		}
	})
}

func TestRuntimeReplyDeferralReadErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "defer-error.db"), time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "登记一个定时计划。")
	readErr := errors.New("fixture CanDeferReply failure")
	r := testRuntime(s)
	r.Store = deferReplyErrorRepository{Repository: s, err: readErr}
	presenterCalls := 0
	r.PresentReply = func(context.Context, personal.Event, string) (string, error) {
		presenterCalls++
		return "unexpected", nil
	}
	err := r.publish(ctx, event, personal.TurnOutput{Text: "已登记。", ReplyKind: personal.ReplyReceipt})
	if !errors.Is(err, readErr) || presenterCalls != 0 {
		t.Fatalf("publish error=%v presenter calls=%d, want deferral read error before presentation", err, presenterCalls)
	}
}

type deferReplyErrorRepository struct {
	Repository
	err error
}

func (r deferReplyErrorRepository) CanDeferReply(context.Context, int64) (bool, error) {
	return false, r.err
}

func TestRuntimePreservesExplicitOwnerCodeLiterals(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "literal.db"), time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "请原样输出这个字符串：\n````\n**literal**\n````\n及 foo_bar(x)。")
	r := testRuntime(s)
	r.PresentReply = func(context.Context, personal.Event, string) (string, error) {
		return "原样：**literal** 与 foo_bar(x)。其他 **正常加粗**。", nil
	}
	if err := r.publish(ctx, event, personal.TurnOutput{Text: "**literal** 与 foo_bar(x)"}); err != nil {
		t.Fatal(err)
	}
	delivery, err := s.ClaimDelivery(ctx)
	const want = "原样：**literal** 与 foo_bar(x)。其他 正常加粗。"
	if err != nil || delivery == nil || delivery.Text != want {
		t.Fatalf("literal delivery = %+v, %v; want %q", delivery, err, want)
	}
	message := testkit.AssistantMessage(t, s, event.ID)
	if message.Text != want {
		t.Fatalf("literal history = %q", message.Text)
	}
}
