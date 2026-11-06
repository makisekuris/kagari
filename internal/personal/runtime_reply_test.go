package personal

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
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
			if err != nil || message == nil || !strings.Contains(message.Text, "独立服务器") {
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
