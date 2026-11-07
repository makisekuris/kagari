package personal

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
)

func TestPresentReplyUsesOnlyDraftAndOwnerRequest(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "presentation.db"), time.Minute)
	event := openUserEvent(t, s, 1, 1, "Please report the final price.")
	task, _ := createTask(t, s, event, "research", "Price check", "Check the displayed monthly price.", "PRIVATE_TASK_CONTEXT")
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseText(`{"omitted_text":"Internal draft:","text":"The monthly price is 17 USD."}`),
		responseText(`{"omitted_text":"Internal draft:","text":"The monthly price is 17 USD."}`),
	}}
	a := &MainAgent{Model: fixture.model(t), Store: s, Timeout: time.Second, Profile: "PRIVATE_PROFILE", Persona: "PRIVATE_PERSONA"}

	cases := []struct {
		event Event
		draft string
		want  string
	}{
		{event: event, draft: "Internal draft: the monthly price is 17 USD.", want: "Please report the final price."},
		{event: Event{Kind: EventTask, TaskID: task.ID}, draft: "Internal draft: the monthly price is 17 USD.", want: task.Goal},
	}
	for _, tc := range cases {
		if text, err := a.PresentReply(ctx, tc.event, tc.draft); err != nil || text != "The monthly price is 17 USD." {
			t.Fatalf("PresentReply() = (%q, %v)", text, err)
		}
	}
	if len(fixture.requests) != 2 {
		t.Fatalf("presenter requests = %d, want 2", len(fixture.requests))
	}
	for i, request := range fixture.requests {
		assertStrictSchema(t, request, "personal_presentation")
		if tools, _ := request["tools"].([]any); len(tools) != 0 {
			t.Fatalf("presenter request %d received %d tools", i, len(tools))
		}
		input := requestInput(request)
		if !strings.Contains(input, cases[i].want) || !strings.Contains(input, "Internal draft") {
			t.Fatalf("presenter request %d omitted its draft or owner request: %s", i, input)
		}
		for _, private := range []string{"PRIVATE_PROFILE", "PRIVATE_PERSONA", "PRIVATE_TASK_CONTEXT"} {
			if strings.Contains(input, private) {
				t.Fatalf("presenter request %d included unrelated context %q", i, private)
			}
		}
	}
}

func TestRuntimePublishPresentationContract(t *testing.T) {
	t.Run("final text is presented once and persisted consistently", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "presented.db"), time.Minute)
		event := openUserEvent(t, s, 1, 1, "Check the result.")
		task, _ := createTask(t, s, event, "task", "Result", "Check the result.", "")
		file, err := s.SaveAttachment(ctx, "result.txt", "text/plain", strings.NewReader("result"))
		if err != nil {
			t.Fatal(err)
		}
		r := testRuntime(s)
		r.Prepare = channel.PrepareText
		draft := "Internal draft with a promise and https://example.com/result."
		presented := "Confirmed: `**literal**` https://example.com/result. " + strings.Repeat("evidence ", 400)
		calls := 0
		r.PresentReply = func(_ context.Context, gotEvent Event, gotDraft string) (string, error) {
			calls++
			if gotEvent.ID != event.ID || gotDraft != draft {
				t.Fatalf("presenter input event=%+v draft=%q", gotEvent, gotDraft)
			}
			return presented, nil
		}
		if err := r.publish(ctx, event, TurnOutput{Text: draft, TaskID: task.ID, Attachments: []string{file.ID}}); err != nil {
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
		rows, err := s.db.QueryContext(ctx, `SELECT text,attachment_id FROM personal_outbox WHERE event_id=? ORDER BY part`, event.ID)
		if err != nil {
			t.Fatal(err)
		}
		parts, attachmentIDs := 0, 0
		for rows.Next() {
			var text string
			var attachmentID sql.NullString
			if err := rows.Scan(&text, &attachmentID); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			parts++
			outbox.WriteString(text)
			if attachmentID.Valid {
				attachmentIDs++
				if attachmentID.String != file.ID {
					t.Errorf("outbox attachment ID = %q, want %q", attachmentID.String, file.ID)
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if parts < 3 || attachmentIDs != 1 || outbox.String() != want {
			t.Fatalf("outbox parts=%d attachments=%d text matches=%t", parts, attachmentIDs, outbox.String() == want)
		}
		history, err := s.History(ctx, event.ID+1, 20)
		if err != nil {
			t.Fatal(err)
		}
		var assistant *Message
		for i := range history {
			if history[i].EventID == event.ID && history[i].Role == "assistant" {
				assistant = &history[i]
			}
		}
		if assistant == nil || assistant.Text != want || assistant.TaskID != task.ID || len(assistant.Attachments) != 1 || assistant.Attachments[0].ID != file.ID {
			t.Fatalf("final history lost reply identity/content: %+v", assistant)
		}
		var eventTaskID int64
		if err := s.db.QueryRowContext(ctx, `SELECT task_id FROM personal_events WHERE id=?`, event.ID).Scan(&eventTaskID); err != nil || eventTaskID != task.ID {
			t.Fatalf("event task ID = %d, %v; want %d", eventTaskID, err, task.ID)
		}
	})

	t.Run("silent and attachment-only replies skip presentation", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "invisible.db"), time.Minute)
		file, err := s.SaveAttachment(ctx, "only.txt", "text/plain", strings.NewReader("file"))
		if err != nil {
			t.Fatal(err)
		}
		r := testRuntime(s)
		calls := 0
		r.PresentReply = func(context.Context, Event, string) (string, error) {
			calls++
			return "unexpected", nil
		}
		attachmentEvent := openUserEvent(t, s, 1, 1, "Send the file.")
		task, _ := createTask(t, s, attachmentEvent, "attachment", "Attachment", "Send the file.", "")
		if err := r.publish(ctx, attachmentEvent, TurnOutput{TaskID: task.ID, Attachments: []string{file.ID}}); err != nil {
			t.Fatal(err)
		}
		var gotAttachment string
		if err := s.db.QueryRowContext(ctx, `SELECT attachment_id FROM personal_outbox WHERE event_id=?`, attachmentEvent.ID).Scan(&gotAttachment); err != nil || gotAttachment != file.ID {
			t.Fatalf("attachment-only outbox ID = %q, %v", gotAttachment, err)
		}
		silentEvent := openUserEvent(t, s, 2, 2, "Continue quietly.")
		if err := r.publish(ctx, silentEvent, TurnOutput{TaskID: task.ID, Silent: true}); err != nil {
			t.Fatal(err)
		}
		var silentOutbox int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, silentEvent.ID).Scan(&silentOutbox); err != nil || silentOutbox != 0 {
			t.Fatalf("silent outbox rows = %d, %v", silentOutbox, err)
		}
		if calls != 0 {
			t.Fatalf("presenter calls for silent/attachment-only output = %d", calls)
		}
	})

	t.Run("presenter failure falls back without replaying Think", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "fallback.db"), time.Minute)
		r := testRuntime(s)
		var thinkCalls int
		const draft = "The committed result is **complete**: https://example.com/final."
		r.Think = func(context.Context, TurnInput) (TurnOutput, error) {
			thinkCalls++
			return TurnOutput{Text: draft}, nil
		}
		r.PresentReply = func(context.Context, Event, string) (string, error) {
			return "", errors.New("fixture presenter failure")
		}
		eventID := acceptInput(t, s, "1", sourceInput("1", "Please check the result."))
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("turn = %v, %v", worked, err)
		}
		if thinkCalls != 1 {
			t.Fatalf("Think calls = %d, want 1", thinkCalls)
		}
		want := channel.PlainTextMarkdown(draft)
		var outboxText, historyText string
		if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_outbox WHERE event_id=?`, eventID).Scan(&outboxText); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_history WHERE event_id=? AND role='assistant'`, eventID).Scan(&historyText); err != nil {
			t.Fatal(err)
		}
		if outboxText != want || historyText != want || !strings.Contains(outboxText, "https://example.com/final") {
			t.Fatalf("fallback changed committed result: outbox=%q history=%q", outboxText, historyText)
		}
	})

	t.Run("presenter cannot add a source URL", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "added-url.db"), time.Minute)
		r := testRuntime(s)
		const draft = "我已经安排后台核对，结果尚未出炉。"
		r.PresentReply = func(context.Context, Event, string) (string, error) {
			return "已经核对完毕：https://example.com/invented", nil
		}
		event := openUserEvent(t, s, 1, 1, "请核对资料。")
		if err := r.publish(ctx, event, TurnOutput{Text: draft}); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != channel.PlainTextMarkdown(draft) || strings.Contains(got, "example.com/invented") {
			t.Fatalf("presenter-added source escaped fallback: %q", got)
		}
	})
}

func TestRuntimePresentationRespectsReplyKindAndTerminalCallback(t *testing.T) {
	t.Run("direct-user receipt bypasses semantic presentation", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "receipt.db"), time.Minute)
		request := "请重新阅读并比较这三篇资料：https://example.com/one https://example.org/two https://example.net/three"
		acceptInput(t, s, "1", sourceInput("1", request))
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("create_task", createTaskArgs{Title: "三篇资料比较", Goal: "Read and compare the three supplied references."}, "create"),
			responseText(`{"text":"已安排后台重新阅读，完成后回复你。","reply_kind":"receipt","attachments":[]}`),
		}}
		r := testRuntime(s)
		presenterCalls := 0
		r.PresentReply = func(context.Context, Event, string) (string, error) {
			presenterCalls++
			return "三篇资料已阅读并比较完毕。", nil
		}
		r.Think = newMainAgent(t, fixture, s).Think
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
		if err != nil || len(tasks) != 1 || tasks[0].Status != TaskQueued {
			t.Fatalf("queued task state = %+v, %v", tasks, err)
		}
	})

	t.Run("schedule receipt without deferred work is presented", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "schedule-receipt.db"), time.Minute)
		acceptInput(t, s, "1", sourceInput("1", "请登记每天 09:00 UTC 的练习计划。"))
		budgetMS := (10 * time.Minute).Milliseconds()
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("create_schedule", ScheduleInput{Title: "每日练习", Goal: "生成一道 Go 练习题", Cron: "0 9 * * *", Timezone: "UTC", WindowMode: WindowNone, TaskBudgetMS: &budgetMS}, "schedule"),
			responseText(`{"text":"每日练习已登记，独立运行，时限十分钟。","reply_kind":"receipt","attachments":[]}`),
		}}
		r := testRuntime(s)
		presenterCalls := 0
		r.PresentReply = func(_ context.Context, event Event, draft string) (string, error) {
			presenterCalls++
			if event.Kind != EventUser {
				t.Fatalf("presenter event kind = %q", event.Kind)
			}
			return "整理：" + draft, nil
		}
		r.Think = newMainAgent(t, fixture, s).Think
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("schedule turn = %v, %v", worked, err)
		}
		if presenterCalls != 1 {
			t.Fatalf("schedule receipt presenter calls = %d, want 1", presenterCalls)
		}
		var scheduleCount int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_schedules`).Scan(&scheduleCount); err != nil || scheduleCount != 1 {
			t.Fatalf("created schedules = %d, %v", scheduleCount, err)
		}
		var taskCount int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_tasks`).Scan(&taskCount); err != nil || taskCount != 0 {
			t.Fatalf("schedule created background tasks = %d, %v", taskCount, err)
		}
		delivery, err := s.ClaimDelivery(ctx)
		if err != nil || delivery == nil || !strings.HasPrefix(delivery.Text, "整理：") {
			t.Fatalf("synchronous schedule receipt was not presented: %+v, %v", delivery, err)
		}
	})

	t.Run("content is presented and terminal callback ignores an old receipt kind", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "content-callback.db"), time.Minute)
		r := testRuntime(s)
		var presenterCalls int
		var presenterEvents []EventKind
		r.PresentReply = func(_ context.Context, event Event, draft string) (string, error) {
			presenterCalls++
			presenterEvents = append(presenterEvents, event.Kind)
			return "整理：" + draft, nil
		}

		acceptInput(t, s, "1", sourceInput("1", "解释 deadline 的含义。"))
		contentFixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseText(`{"text":"deadline 是完成任务的最后期限。","reply_kind":"content","attachments":[]}`),
		}}
		r.Think = newMainAgent(t, contentFixture, s).Think
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("content turn = %v, %v", worked, err)
		}
		contentDelivery, err := s.ClaimDelivery(ctx)
		if err != nil || contentDelivery == nil || !strings.HasPrefix(contentDelivery.Text, "整理：") {
			t.Fatalf("content was not presented: %+v, %v", contentDelivery, err)
		}

		seed := openUserEvent(t, s, 2, 2, "核对账户价格。")
		task, run := createTask(t, s, seed, "create", "价格核对", "Read the account pricing page.", "")
		finishEvent(t, s, seed, TurnOutput{TaskID: task.ID, Silent: true}, nil)
		claimed, err := s.ClaimRun(ctx, RunTask)
		if err != nil || claimed == nil || claimed.ID != run.ID {
			t.Fatalf("initial run = %+v, %v", claimed, err)
		}
		if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "页面确认月费为 17 元。"}, ""); err != nil {
			t.Fatal(err)
		}
		callbackFixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("settle_task", settleTaskArgs{TaskID: task.ID, ExpectedRevision: task.Revision, Status: TaskCompleted, Summary: "页面确认月费为 17 元。"}, "settle"),
			responseText(`{"text":"已收到，后台会继续处理。","reply_kind":"receipt","attachments":[]}`),
		}}
		r.Think = newMainAgent(t, callbackFixture, s).Think
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("terminal callback turn = %v, %v", worked, err)
		}
		callbackDelivery, err := s.ClaimDelivery(ctx)
		if err != nil || callbackDelivery == nil || !strings.Contains(callbackDelivery.Text, "整理：") || !strings.Contains(callbackDelivery.Text, "17 元") || strings.Contains(callbackDelivery.Text, "会继续处理") {
			t.Fatalf("terminal callback was not presented from committed result: %+v, %v", callbackDelivery, err)
		}
		if presenterCalls != 2 || len(presenterEvents) != 2 || presenterEvents[0] != EventUser || presenterEvents[1] != EventTask {
			t.Fatalf("presenter calls/events = %d/%v, want one direct content and one terminal callback", presenterCalls, presenterEvents)
		}
	})
}

func TestRuntimeReplyDeferralReadErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "defer-error.db"), time.Minute)
	event := openUserEvent(t, s, 1, 1, "登记一个定时计划。")
	readErr := errors.New("fixture CanDeferReply failure")
	r := testRuntime(s)
	r.Store = deferReplyErrorRepository{Repository: s, err: readErr}
	presenterCalls := 0
	r.PresentReply = func(context.Context, Event, string) (string, error) {
		presenterCalls++
		return "unexpected", nil
	}
	err := r.publish(ctx, event, TurnOutput{Text: "已登记。", replyKind: replyReceipt})
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
	s := testStore(t, filepath.Join(t.TempDir(), "literal.db"), time.Minute)
	event := openUserEvent(t, s, 1, 1, "请原样输出这个字符串：\n````\n**literal**\n````\n及 foo_bar(x)。")
	r := testRuntime(s)
	r.PresentReply = func(context.Context, Event, string) (string, error) {
		return "原样：**literal** 与 foo_bar(x)。其他 **正常加粗**。", nil
	}
	if err := r.publish(ctx, event, TurnOutput{Text: "**literal** 与 foo_bar(x)"}); err != nil {
		t.Fatal(err)
	}
	delivery, err := s.ClaimDelivery(ctx)
	const want = "原样：**literal** 与 foo_bar(x)。其他 正常加粗。"
	if err != nil || delivery == nil || delivery.Text != want {
		t.Fatalf("literal delivery = %+v, %v; want %q", delivery, err, want)
	}
	var history string
	if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_history WHERE event_id=? AND role='assistant'`, event.ID).Scan(&history); err != nil || history != want {
		t.Fatalf("literal history = %q, %v", history, err)
	}
}
