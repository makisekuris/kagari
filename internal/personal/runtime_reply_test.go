package personal

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/domain"
)

func TestRuntimeDeferredReadingAndWaitingDelivery(t *testing.T) {
	for _, waiting := range []string{"none", "pending", "sent"} {
		t.Run(waiting, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, filepath.Join(t.TempDir(), "replies.db"), time.Minute)
			r := testRuntime(s)
			r.Think = func(ctx context.Context, input TurnInput) (TurnOutput, error) {
				if input.Event.Kind == EventUser {
					task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "页面初步阅读", TaskInput{Goal: "Read the supplied page and identify the important limitation."})
					return TurnOutput{TaskID: task.ID, Silent: true}, err
				}
				task, err := s.Task(ctx, input.Event.TaskID)
				if err != nil {
					return TurnOutput{}, err
				}
				_, err = s.SettleTask(ctx, input.Event.ID, task.ID, task.Revision, TaskCompleted, "requires a dedicated server")
				return TurnOutput{TaskID: task.ID, Text: "这个方案需要独立服务器，不适合直接部署到静态托管平台。"}, err
			}
			r.Execute = func(context.Context, Run, func(string) error) (StageResult, error) {
				return StageResult{Summary: "requires a dedicated server"}, nil
			}
			acceptInput(t, s, "1", sourceInput("1", "https://example.com/article"))
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("initial turn = %v, %v", worked, err)
			}
			if message, err := s.ClaimDelivery(ctx); err != nil || message != nil {
				t.Fatalf("deferred reading sent an acknowledgement: %+v, %v", message, err)
			}
			if worked, err := r.waitingReply(ctx); err != nil || worked {
				t.Fatalf("waiting hint was early: %v, %v", worked, err)
			}
			if waiting != "none" {
				if worked, err := s.QueueWaitingReply(ctx, time.Now(), time.Nanosecond); err != nil || !worked {
					t.Fatalf("queue waiting hint = %v, %v", worked, err)
				}
			}
			if waiting == "sent" {
				message, err := s.ClaimDelivery(ctx)
				if err != nil || message == nil || strings.Contains(message.Text, "#") {
					t.Fatalf("waiting hint = %+v, %v", message, err)
				}
				if err := s.UpdateDelivery(ctx, message.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "wait"}, "", time.Time{}); err != nil {
					t.Fatal(err)
				}
			}
			if worked, err := r.task(ctx); err != nil || !worked {
				t.Fatalf("reading stage = %v, %v", worked, err)
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
			if worked, err := s.QueueWaitingReply(ctx, time.Now().Add(time.Minute), replyWaitDelay); err != nil || worked {
				t.Fatalf("completed reading created a late hint: %v, %v", worked, err)
			}
			if extra, err := s.ClaimDelivery(ctx); err != nil || extra != nil {
				t.Fatalf("extra delivery = %+v, %v", extra, err)
			}
		})
	}
}

func TestRuntimeRejectsSilenceWithoutWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "no-work.db"), time.Minute)
	r := testRuntime(s)
	r.Think = func(context.Context, TurnInput) (TurnOutput, error) { return TurnOutput{Silent: true}, nil }
	acceptInput(t, s, "1", sourceInput("1", "https://example.com/article"))
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
	s := testStore(t, filepath.Join(t.TempDir(), "closing.db"), time.Minute)
	r := testRuntime(s)
	stages := 0
	r.Execute = func(context.Context, Run, func(string) error) (StageResult, error) {
		stages++
		return StageResult{Summary: fmt.Sprintf("stage %d: restriction confirmed", stages)}, nil
	}
	r.Think = func(ctx context.Context, input TurnInput) (TurnOutput, error) {
		if input.Event.Kind == EventUser {
			task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "兼容性检查", TaskInput{Goal: "Check both references."})
			return TurnOutput{TaskID: task.ID, Silent: true}, err
		}
		task, err := s.Task(ctx, input.Event.TaskID)
		if err != nil {
			return TurnOutput{}, err
		}
		if stages == 1 {
			_, _, err = s.ContinueTask(ctx, input.Event.ID, "next", task.ID, task.Revision, "Check the second reference.", false)
			return TurnOutput{TaskID: task.ID, Text: "第一个页面说明它不支持离线使用，我还在核对第二个页面。"}, err
		}
		_, err = s.SettleTask(ctx, input.Event.ID, task.ID, task.Revision, TaskCompleted, "第二个页面也核对完了，结论不变。")
		return TurnOutput{TaskID: task.ID, Silent: true}, err
	}
	acceptInput(t, s, "1", sourceInput("1", "https://example.com/first https://example.com/second"))
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
	s := testStore(t, filepath.Join(t.TempDir(), "empty-closing.db"), time.Minute)
	r := testRuntime(s)
	r.Execute = func(context.Context, Run, func(string) error) (StageResult, error) {
		return StageResult{Summary: "新资料已经核对，结论不变。"}, nil
	}
	r.Think = func(ctx context.Context, input TurnInput) (TurnOutput, error) {
		if input.Event.Kind == EventUser {
			task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "资料核对", TaskInput{Goal: "核对资料"})
			return TurnOutput{TaskID: task.ID, Silent: true}, err
		}
		task, err := s.SettleTask(ctx, input.Event.ID, input.Event.TaskID, 1, TaskCompleted, "新资料已经核对，结论不变。")
		return TurnOutput{TaskID: task.ID}, err
	}
	acceptInput(t, s, "1", sourceInput("1", "https://example.com/reference"))
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
	s := testStore(t, ":memory:", time.Minute)
	r := testRuntime(s)
	r.Execute = func(context.Context, Run, func(string) error) (StageResult, error) {
		return StageResult{Summary: "页面确认月费17元。"}, nil
	}
	r.Think = func(ctx context.Context, input TurnInput) (TurnOutput, error) {
		if input.Event.Kind == EventUser {
			task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "资料核对", TaskInput{Goal: "核对价格"})
			return TurnOutput{TaskID: task.ID, Silent: true}, err
		}
		return TurnOutput{Text: "我会继续处理，有结果再回复。"}, nil
	}
	acceptInput(t, s, "1", sourceInput("1", "https://example.com/reference"))
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
		status  TaskStatus
		state   string
		summary string
		want    string
	}{
		{status: TaskCompleted, state: "已经处理完", summary: "页面确认月费17元。", want: "17元"},
		{status: TaskFailed, state: "这次未能完成", summary: "页面访问失败，未能确认价格。", want: "未能确认价格"},
		{status: TaskWaitingInput, state: "需要你补充信息", summary: "需要账户地区才能确认价格。", want: "账户地区"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, filepath.Join(t.TempDir(), "terminal.db"), time.Minute)
			r := testRuntime(s)
			var attachmentID string
			if tc.status == TaskCompleted {
				file, err := s.SaveAttachment(ctx, "receipt.txt", "text/plain", strings.NewReader("月费17元"))
				if err != nil {
					t.Fatal(err)
				}
				attachmentID = file.ID
			}
			r.Execute = func(ctx context.Context, run Run, _ func(string) error) (StageResult, error) {
				ref, err := s.SaveSource(ctx, run.ID, "price-source", domain.Source{URL: "https://example.com/price", Status: "ok", Content: "月费17元。", FetchedAt: time.Now()})
				return StageResult{Summary: "阶段资料已读取。", Sources: []domain.SourceRef{ref}}, err
			}
			r.Think = func(ctx context.Context, input TurnInput) (TurnOutput, error) {
				if input.Event.Kind == EventUser {
					task, _, err := s.CreateTask(ctx, input.Event.ID, "read", "价格核对", TaskInput{Goal: "Check the displayed price."})
					return TurnOutput{TaskID: task.ID, Silent: true}, err
				}
				task, err := s.Task(ctx, input.Event.TaskID)
				if err != nil {
					return TurnOutput{}, err
				}
				if _, err := s.SettleTask(ctx, input.Event.ID, task.ID, task.Revision, tc.status, tc.summary); err != nil {
					return TurnOutput{}, err
				}
				output := TurnOutput{TaskID: task.ID, Text: "我会继续处理，有结果再回复。"}
				if attachmentID != "" {
					output.Attachments = []string{attachmentID}
				}
				return output, nil
			}
			acceptInput(t, s, "1", sourceInput("1", "https://example.com/price"))
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
			if tc.status == TaskCompleted {
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

func TestTaskClosingSourcesOmitDuplicatesAndFailedReads(t *testing.T) {
	text := taskClosingText(&Task{Title: "核对", Status: TaskCompleted, Summary: "已核对 https://example.com/existing。"}, []domain.SourceRef{
		{URL: "https://example.com/existing", Status: "ok"},
		{URL: "https://example.com/additional", Status: "ok"},
		{URL: "https://example.com/additional", Status: "ok"},
		{URL: "https://example.com/unread", Status: "failed"},
	})
	for _, url := range []string{"https://example.com/existing", "https://example.com/additional"} {
		if strings.Count(text, url) != 1 {
			t.Fatalf("source missing or duplicated: %s", text)
		}
	}
	if strings.Contains(text, "https://example.com/unread") {
		t.Fatalf("failed read presented as evidence: %s", text)
	}
}

func TestRuntimeTerminalScheduledTaskKeepsWindowAndSuggestion(t *testing.T) {
	ctx := context.Background()
	s, schedule, memory := scheduleSuggestionFixture(t, WindowSinceSuccess)
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
	_, run := dispatchTestRun(t, s, *schedule.NextRunAt)
	if _, err := s.BeginScheduledRun(ctx, run.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Feed checked."}, ""); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	r.Think = func(ctx context.Context, input TurnInput) (TurnOutput, error) {
		task, err := s.Task(ctx, input.Event.TaskID)
		if err != nil {
			return TurnOutput{}, err
		}
		if _, _, err := s.ProposeScheduleContextUpdate(ctx, input.Event.ID, "proposal", task.ID, scheduleSuggestionProposal(schedule, memory), scheduleTestNow()); err != nil {
			return TurnOutput{}, err
		}
		if _, err := s.SettleTask(ctx, input.Event.ID, task.ID, task.Revision, TaskFailed, "来源检查失败，无法确认完整区间。"); err != nil {
			return TurnOutput{}, err
		}
		return TurnOutput{TaskID: task.ID, ScheduleID: input.Event.ScheduleID, Text: "我会继续处理，有结果再回复。"}, nil
	}
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("scheduled result turn = %v, %v", worked, err)
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil {
		t.Fatalf("scheduled result delivery = %+v, %v", message, err)
	}
	for _, expected := range []string{
		fmt.Sprintf("计划 #%d · Task #%d", schedule.ID, run.TaskID),
		schedule.BaselineAt.Format(time.RFC3339),
		"完整检查：false",
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
