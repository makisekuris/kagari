package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRuntimeDeferredReadingRepliesOnlyWithResult(t *testing.T) {
	for _, request := range []string{"https://example.com/article", "请分析这个方案的部署限制：https://example.com/article"} {
		t.Run(request, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, filepath.Join(t.TempDir(), "replies.db"), time.Minute)
			r := testRuntime(s)
			r.Think = func(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
				if input.Event.Kind == personal.EventUser {
					task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "页面初步阅读", personal.TaskInput{Goal: "Read the supplied page and identify the important limitation."})
					return personal.TurnOutput{TaskID: task.ID, Silent: true}, err
				}
				task, err := s.Task(ctx, input.Event.TaskID)
				if err != nil {
					return personal.TurnOutput{}, err
				}
				_, err = s.SettleTask(ctx, input.Event.ID, task.ID, task.Revision, personal.TaskSettlement{Status: personal.TaskCompleted, Summary: "requires a dedicated server", Delivery: personal.DeliveryDraft{Text: "requires a dedicated server"}})
				return personal.TurnOutput{TaskID: task.ID, Text: "这个方案需要独立服务器，不适合直接部署到静态托管平台。"}, err
			}
			r.Execute = func(context.Context, personal.Run, func(string) error) (personal.StageResult, error) {
				return personal.StageResult{Summary: "requires a dedicated server"}, nil
			}
			testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", request))
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("initial turn = %v, %v", worked, err)
			}
			if message, err := s.ClaimDelivery(ctx); err != nil || message != nil {
				t.Fatalf("deferred reading sent an acknowledgement: %+v, %v", message, err)
			}
			if worked, err := r.task(ctx); err != nil || !worked {
				t.Fatalf("reading stage = %v, %v", worked, err)
			}
			if message, err := s.ClaimDelivery(ctx); err != nil || message != nil {
				t.Fatalf("unfinished reading sent a status message: %+v, %v", message, err)
			}
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("result turn = %v, %v", worked, err)
			}
			message, err := s.ClaimDelivery(ctx)
			if err != nil || message == nil || !strings.Contains(message.Text, "requires a dedicated server") {
				t.Fatalf("result delivery = %+v, %v", message, err)
			}
			if err := s.UpdateDelivery(ctx, message.ID, "sent", channel.Receipt{MessageID: "result"}, "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			if extra, err := s.ClaimDelivery(ctx); err != nil || extra != nil {
				t.Fatalf("extra delivery = %+v, %v", extra, err)
			}
		})
	}
}

func TestRuntimeRejectsSilenceWithoutWork(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "no-work.db"), time.Minute)
	r := testRuntime(s)
	r.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
		return personal.TurnOutput{Silent: true}, nil
	}
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/article"))
	if _, err := r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil || !strings.Contains(message.Text, "未能完成") {
		t.Fatalf("unhandled request was lost: %+v, %v", message, err)
	}
}

func TestRuntimeClosesAfterAnImportantIntermediateReply(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "closing.db"), time.Minute)
	r := testRuntime(s)
	stages := 0
	r.Execute = func(context.Context, personal.Run, func(string) error) (personal.StageResult, error) {
		stages++
		return personal.StageResult{Summary: fmt.Sprintf("stage %d: restriction confirmed", stages)}, nil
	}
	r.Think = func(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
		if input.Event.Kind == personal.EventUser {
			task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "兼容性检查", personal.TaskInput{Goal: "Check both references."})
			return personal.TurnOutput{TaskID: task.ID, Silent: true}, err
		}
		task, err := s.Task(ctx, input.Event.TaskID)
		if err != nil {
			return personal.TurnOutput{}, err
		}
		if stages == 1 {
			_, _, err = s.ContinueTask(ctx, input.Event.ID, "next", task.ID, task.Revision, "Check the second reference.", false)
			return personal.TurnOutput{TaskID: task.ID, Text: "第一个页面说明它不支持离线使用，我还在核对第二个页面。"}, err
		}
		_, err = s.SettleTask(ctx, input.Event.ID, task.ID, task.Revision, personal.TaskSettlement{Status: personal.TaskCompleted, Summary: "第二个页面也核对完了，结论不变。", Delivery: personal.DeliveryDraft{Text: "第二个页面也核对完了，结论不变。"}})
		return personal.TurnOutput{TaskID: task.ID, Silent: true}, err
	}
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/first https://example.com/second"))
	for i := 0; i < 5; i++ {
		var err error
		if i%2 == 0 {
			_, err = r.turn(ctx)
		} else {
			_, err = r.task(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, expected := range []string{"还在核对", "结论不变"} {
		message, err := s.ClaimDelivery(ctx)
		if err != nil || message == nil || !strings.Contains(message.Text, expected) || strings.Contains(message.Text, "#") {
			t.Fatalf("expected %q, got %+v, %v", expected, message, err)
		}
		if err := s.UpdateDelivery(ctx, message.ID, "sent", channel.Receipt{MessageID: expected}, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeClosesWhenModelSettlesWithoutReplyText(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "empty-closing.db"), time.Minute)
	r := testRuntime(s)
	r.Execute = func(context.Context, personal.Run, func(string) error) (personal.StageResult, error) {
		return personal.StageResult{Summary: "新资料已经核对，结论不变。"}, nil
	}
	r.Think = func(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
		if input.Event.Kind == personal.EventUser {
			task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "资料核对", personal.TaskInput{Goal: "核对资料"})
			return personal.TurnOutput{TaskID: task.ID, Silent: true}, err
		}
		task, err := s.SettleTask(ctx, input.Event.ID, input.Event.TaskID, 1, personal.TaskSettlement{Status: personal.TaskCompleted, Summary: "新资料已经核对，结论不变。", Delivery: personal.DeliveryDraft{Text: "资料已经处理完，新资料已核对，结论不变。"}})
		return personal.TurnOutput{TaskID: task.ID}, err
	}
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/reference"))
	for _, step := range []func(context.Context) (bool, error){r.turn, r.task, r.turn} {
		if worked, err := step(ctx); err != nil || !worked {
			t.Fatalf("reading step = %v, %v", worked, err)
		}
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil || !strings.Contains(message.Text, "处理完") || !strings.Contains(message.Text, "结论不变") {
		t.Fatalf("missing closing status: %+v, %v", message, err)
	}
}

func TestRuntimeReportsIncompletePlanningWithSavedResult(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	r := testRuntime(s)
	r.Execute = func(context.Context, personal.Run, func(string) error) (personal.StageResult, error) {
		return personal.StageResult{Summary: "页面确认月费17元。"}, nil
	}
	r.Think = func(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
		if input.Event.Kind == personal.EventUser {
			task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "资料核对", personal.TaskInput{Goal: "核对价格"})
			return personal.TurnOutput{TaskID: task.ID, Silent: true}, err
		}
		return personal.TurnOutput{Text: "我会继续处理，有结果再回复。"}, nil
	}
	testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/reference"))
	for _, step := range []func(context.Context) (bool, error){r.turn, r.task, r.turn} {
		if worked, err := step(ctx); err != nil || !worked {
			t.Fatalf("reading step = %v, %v", worked, err)
		}
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil {
		t.Fatalf("missing planning failure: %+v %v", message, err)
	}
	if !strings.Contains(message.Text, "后续安排未能完成") || !strings.Contains(message.Text, "17元") || strings.Contains(message.Text, "补充信息") || strings.Contains(message.Text, "我会继续") {
		t.Fatalf("planning failure misrepresented: %s", message.Text)
	}
}

func TestRuntimeTerminalTaskResultOverridesModelCommitment(t *testing.T) {
	for _, tc := range []struct {
		status  personal.TaskStatus
		state   string
		summary string
		want    string
	}{
		{status: personal.TaskCompleted, state: "已经处理完", summary: "页面确认月费17元。", want: "17元"},
		{status: personal.TaskFailed, state: "这次未能完成", summary: "页面访问失败，未能确认价格。", want: "未能确认价格"},
		{status: personal.TaskWaitingInput, state: "需要你补充信息", summary: "需要账户地区才能确认价格。", want: "账户地区"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, filepath.Join(t.TempDir(), "terminal.db"), time.Minute)
			r := testRuntime(s)
			var attachmentID string
			var taskID int64
			var presentedDraft string
			presenterCalls := 0
			r.PresentReply = func(_ context.Context, event personal.Event, draft string) (string, error) {
				if event.Kind != personal.EventTask {
					t.Fatalf("presenter received event kind %q", event.Kind)
				}
				presenterCalls++
				presentedDraft = draft
				return "整理后：\n" + draft, nil
			}
			if tc.status == personal.TaskCompleted {
				file, err := s.SaveAttachment(ctx, "receipt.txt", "text/plain", strings.NewReader("月费17元"))
				if err != nil {
					t.Fatal(err)
				}
				attachmentID = file.ID
			}
			r.Execute = func(ctx context.Context, run personal.Run, _ func(string) error) (personal.StageResult, error) {
				ref, err := s.SaveSource(ctx, run.ID, "price-source", domain.Source{URL: "https://example.com/price", Status: "ok", Content: "月费17元。", FetchedAt: time.Now()})
				return personal.StageResult{Summary: "阶段资料已读取。", Sources: []domain.SourceRef{ref}}, err
			}
			r.Think = func(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
				if input.Event.Kind == personal.EventUser {
					task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "价格核对", personal.TaskInput{Goal: "Check the displayed price."})
					taskID = task.ID
					return personal.TurnOutput{TaskID: task.ID, Silent: true}, err
				}
				task, err := s.Task(ctx, input.Event.TaskID)
				if err != nil {
					return personal.TurnOutput{}, err
				}
				delivery := personal.DeliveryDraft{Text: tc.state + "。\n\n" + tc.summary + "\nhttps://example.com/price"}
				if attachmentID != "" {
					delivery.Attachments = []string{attachmentID}
				}
				if _, err := s.SettleTask(ctx, input.Event.ID, task.ID, task.Revision, personal.TaskSettlement{Status: tc.status, Summary: tc.summary, Delivery: delivery}); err != nil {
					return personal.TurnOutput{}, err
				}
				output := personal.TurnOutput{TaskID: task.ID, Text: "我会继续处理，有结果再回复。"}
				if attachmentID != "" {
					output.Attachments = []string{attachmentID}
				}
				return output, nil
			}
			testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "https://example.com/price"))
			for _, step := range []func(context.Context) (bool, error){r.turn, r.task, r.turn} {
				if worked, err := step(ctx); err != nil || !worked {
					t.Fatalf("runtime step = %v, %v", worked, err)
				}
			}
			message, err := s.ClaimDelivery(ctx)
			if err != nil || message == nil {
				t.Fatalf("terminal delivery = %+v, %v", message, err)
			}
			for _, expected := range []string{tc.state, tc.want, "https://example.com/price"} {
				if !strings.Contains(message.Text, expected) {
					t.Fatalf("terminal delivery omitted %q: %+v", expected, message)
				}
			}
			if strings.Contains(message.Text, "有结果再回复") {
				t.Fatalf("model commitment replaced the saved result: %q", message.Text)
			}
			if presenterCalls != 1 || !strings.Contains(presentedDraft, tc.state) || !strings.Contains(presentedDraft, tc.want) || !strings.Contains(presentedDraft, "https://example.com/price") || strings.Contains(presentedDraft, "有结果再回复") {
				t.Fatalf("presenter did not receive the committed terminal result first: calls=%d draft=%q", presenterCalls, presentedDraft)
			}
			history, err := s.History(ctx, message.EventID+1, 20)
			if err != nil {
				t.Fatal(err)
			}
			var assistant *personal.Message
			for i := range history {
				if history[i].EventID == message.EventID && history[i].Role == "assistant" {
					assistant = &history[i]
				}
			}
			if assistant == nil || assistant.Text != message.Text || assistant.TaskID != taskID {
				t.Fatalf("presented delivery and history diverged: delivery=%+v history=%+v", message, assistant)
			}
			if tc.status == personal.TaskCompleted {
				if err := s.UpdateDelivery(ctx, message.ID, "sent", channel.Receipt{MessageID: "terminal-text"}, "", time.Time{}); err != nil {
					t.Fatal(err)
				}
				attachment, err := s.ClaimDelivery(ctx)
				if err != nil || attachment == nil || attachment.AttachmentID != attachmentID {
					t.Fatalf("terminal result lost its attachment: %+v, %v", attachment, err)
				}
			}
		})
	}
}

func TestTaskClosingSourcesDeduplicateAndLabelFailedReads(t *testing.T) {
	text := taskClosingText(&personal.Task{Title: "核对", Status: personal.TaskCompleted, Summary: "已核对 https://example.com/existing。"}, []domain.SourceRef{
		{URL: "https://example.com/existing", Status: "ok"},
		{URL: "https://example.com/additional", Status: "ok"},
		{URL: "https://example.com/additional", Status: "ok"},
		{URL: "https://example.com/unread", Status: "failed"},
	})
	for _, url := range []string{"https://example.com/existing", "https://example.com/additional", "https://example.com/unread"} {
		if strings.Count(text, url) != 1 {
			t.Fatalf("source missing or duplicated: %s", text)
		}
	}
	if !strings.Contains(text, "未能完整读取的来源：\nhttps://example.com/unread（读取失败）") {
		t.Fatalf("failed read was not clearly distinguished from evidence: %s", text)
	}
}

func TestTaskClosingTextPreservesLongSuccessfulSummary(t *testing.T) {
	summary := strings.Repeat("已核对每项要求。", 700) + "最后一项结论已保留。"
	text := taskClosingText(&personal.Task{Title: "完整核对", Status: personal.TaskCompleted, Summary: summary}, nil)
	if !strings.Contains(text, summary) {
		t.Fatalf("task closing truncated the saved result: got %d runes, want full %d-rune summary", len([]rune(text)), len([]rune(summary)))
	}
}

func TestTaskFallbackBoundsFailedAndWaitingSummaries(t *testing.T) {
	const sourceText = "英文来源正文不得进入回退消息。"
	legacySummary := "Stage did not complete. upstream timed out. Partial source evidence returned:\n" +
		"- Account guide (status: restricted); note: login required; returned text: " + strings.Repeat(sourceText, 500) + "\n" +
		"- Pricing guide (status: failed); note: connection refused; returned text: " + strings.Repeat(sourceText, 500)
	sources := []domain.SourceRef{
		{URL: "https://example.com/account", Status: "restricted"},
		{URL: "https://example.com/pricing", Status: "failed"},
	}

	for _, tc := range []struct {
		name   string
		status personal.TaskStatus
	}{
		{name: "failed task", status: personal.TaskFailed},
		{name: "legacy completed task", status: personal.TaskCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := taskClosingText(&personal.Task{Title: "账户核对", Status: tc.status, Summary: legacySummary}, sources)
			if strings.Contains(text, sourceText) || strings.Contains(text, "returned text:") {
				t.Fatalf("fallback repeated source excerpts: %s", text)
			}
			for _, want := range []string{"upstream timed out", "login required", "connection refused", "https://example.com/account", "https://example.com/pricing"} {
				if !strings.Contains(text, want) {
					t.Errorf("fallback omitted actionable detail %q: %s", want, text)
				}
			}
			if len(text) > maxTaskFallbackSummaryBytes+500 {
				t.Fatalf("legacy fallback remains unbounded: %d bytes", len(text))
			}
		})
	}

	waitingSummary := strings.Repeat("已核对当前资料并保存阶段结果。", 300) + "\n\n需要你提供账户地区才能继续核对。"
	waitingText := taskClosingText(&personal.Task{Title: "价格核对", Status: personal.TaskWaitingInput, Summary: waitingSummary}, nil)
	if len(waitingText) > maxTaskFallbackSummaryBytes+300 {
		t.Fatalf("waiting-input fallback remains unbounded: %d bytes", len(waitingText))
	}
	if !strings.Contains(waitingText, "需要你提供账户地区才能继续核对") {
		t.Fatalf("bounded waiting fallback lost the required question: %s", waitingText)
	}

	budgetText := taskBudgetText(&personal.Task{Title: "账户核对", Status: personal.TaskWaitingBudget, Summary: legacySummary}, sources)
	if strings.Contains(budgetText, sourceText) || strings.Contains(budgetText, "returned text:") || !strings.Contains(budgetText, "https://example.com/account") {
		t.Fatalf("waiting-budget fallback leaked legacy body or lost source: %s", budgetText)
	}
}

func TestRuntimeTerminalScheduledTaskKeepsWindowAndSuggestion(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := testkit.ScheduleSuggestion(t, personal.WindowSinceSuccess)
	for {
		previous, err := s.ClaimDelivery(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if previous == nil {
			break
		}
		if err := s.UpdateDelivery(ctx, previous.ID, "sent", channel.Receipt{MessageID: fmt.Sprintf("setup-%d", previous.ID)}, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	_, run := testkit.DispatchRun(t, s, *schedule.NextRunAt)
	if _, err := s.BeginScheduledRun(ctx, run.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, run.ID, personal.StageResult{Summary: "Feed checked."}, ""); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	r.Think = func(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
		task, err := s.Task(ctx, input.Event.TaskID)
		if err != nil {
			return personal.TurnOutput{}, err
		}
		if _, _, err := s.ProposeScheduleContextUpdate(ctx, input.Event.ID, "proposal", task.ID, testkit.SuggestionProposal(schedule, memory), testkit.ScheduleNow()); err != nil {
			return personal.TurnOutput{}, err
		}
		if _, err := s.SettleTask(ctx, input.Event.ID, task.ID, task.Revision, personal.TaskSettlement{Status: personal.TaskFailed, Summary: "来源检查失败，无法确认完整区间。", Delivery: personal.DeliveryDraft{Text: "这次未能完成检查。来源检查失败，无法确认完整区间。"}}); err != nil {
			return personal.TurnOutput{}, err
		}
		return personal.TurnOutput{TaskID: task.ID, ScheduleID: input.Event.ScheduleID, Text: "我会继续处理，有结果再回复。"}, nil
	}
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("scheduled result turn = %v, %v", worked, err)
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil {
		t.Fatalf("scheduled result delivery = %+v, %v", message, err)
	}
	for _, expected := range []string{
		fmt.Sprintf("计划 #%d · 任务 #%d", schedule.ID, run.TaskID),
		schedule.BaselineAt.Format(time.RFC3339),
		"检查结果：不完整",
		"这次未能完成",
		"无法确认完整区间",
	} {
		if !strings.Contains(message.Text, expected) {
			t.Fatalf("scheduled delivery omitted %q: %q", expected, message.Text)
		}
	}
	if strings.Contains(message.Text, "有结果再回复") {
		t.Fatalf("model commitment replaced the saved scheduled result: %q", message.Text)
	}
	if err := s.UpdateDelivery(ctx, message.ID, "sent", channel.Receipt{MessageID: "scheduled-result"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	suggestion, err := s.ClaimDelivery(ctx)
	if err != nil || suggestion == nil || !strings.Contains(suggestion.Text, "建议 #") {
		t.Fatalf("scheduled follow-up suggestion = %+v, %v", suggestion, err)
	}
}
