package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for runtime state")
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for runtime event")
		var zero T
		return zero
	}
}

func TestRuntimeRequiresReplyPresenter(t *testing.T) {
	runtime := testRuntime(testkit.Store(t, ":memory:", time.Minute))
	runtime.PresentReply = nil
	if err := runtime.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "reply presenter") {
		t.Fatalf("Run without reply presenter = %v", err)
	}
}

func TestRuntimeFourTasksLeaveConversationAndMemoryResponsive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	path := filepath.Join(t.TempDir(), "runtime.db")
	s := testkit.Store(t, path, time.Minute)
	r := testRuntime(s)
	started, sent := make(chan int64, 10), make(chan string, 20)
	release := make(chan struct{})
	var active, peak, receipts atomic.Int64
	r.Think = func(ctx context.Context, in personal.TurnInput) (personal.TurnOutput, error) {
		switch in.Event.Text {
		case "start five":
			for i := 1; i <= 5; i++ {
				_, _, err := s.CreateTask(ctx, in.Event.ID, fmt.Sprint(i), fmt.Sprintf("task %d", i), personal.TaskInput{Goal: fmt.Sprintf("goal %d", i)})
				if err != nil {
					return personal.TurnOutput{}, err
				}
			}
			return personal.TurnOutput{Text: "five queued"}, nil
		case "我长期偏好简短回复":
			return personal.TurnOutput{Text: "conversation responsive"}, nil
		case "cancel one":
			task, err := s.Task(ctx, 1)
			if err != nil {
				return personal.TurnOutput{}, err
			}
			_, err = s.CancelTask(ctx, in.Event.ID, task.ID, task.Revision)
			return personal.TurnOutput{Text: "one cancelled", TaskID: task.ID}, err
		}
		if in.Event.Kind == personal.EventTask {
			task, err := s.Task(ctx, in.Event.TaskID)
			if err != nil {
				return personal.TurnOutput{}, err
			}
			_, err = s.SettleTask(ctx, in.Event.ID, task.ID, task.Revision, personal.TaskCompleted, "complete")
			return personal.TurnOutput{Text: fmt.Sprintf("completed %d", task.ID), TaskID: task.ID}, err
		}
		return personal.TurnOutput{Text: "unexpected event"}, nil
	}
	r.Execute = func(ctx context.Context, run personal.Run, progress func(string) error) (personal.StageResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old; old = peak.Load() {
			if peak.CompareAndSwap(old, current) {
				break
			}
		}
		if err := progress("reading"); err != nil {
			return personal.StageResult{}, err
		}
		started <- run.TaskID
		select {
		case <-release:
		case <-ctx.Done():
		}
		// Even a late successful return must not revive a cancelled task.
		return personal.StageResult{Summary: "stage complete"}, nil
	}
	r.ReviewMemory = func(_ context.Context, in personal.MemoryReviewInput) (personal.MemoryReview, error) {
		if in.Event.Text != "我长期偏好简短回复" {
			return personal.MemoryReview{}, nil
		}
		return personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "reply_style", Value: "简短回复", Evidence: in.Event.Text}}}, nil
	}
	r.Send = func(_ context.Context, content channel.Content) (channel.Receipt, error) {
		sent <- content.Text
		return channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: fmt.Sprint(receipts.Add(1))}, nil
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := receive(t, done); err != nil {
			t.Error(err)
		}
	})
	add := func(id int64, text string) {
		t.Helper()
		testkit.AcceptInput(t, s, strconv.FormatInt(id, 10), testkit.SourceInput(strconv.FormatInt(id, 10), text))
	}
	add(1, "start five")
	seen := map[int64]bool{}
	for i := 0; i < 4; i++ {
		seen[receive(t, started)] = true
	}
	if len(seen) != 4 || seen[5] {
		t.Fatalf("initial tasks: %v", seen)
	}
	task, err := s.Task(ctx, 5)
	if err != nil || task.Status != personal.TaskQueued {
		t.Fatalf("fifth task: %+v, %v", task, err)
	}
	add(2, "我长期偏好简短回复")
	eventually(t, func() bool {
		m, err := s.Memory(ctx, "reply_style")
		return err == nil && m != nil && m.Value == "简短回复"
	})
	if got := receive(t, sent); got != "five queued" {
		t.Fatal(got)
	}
	if got := receive(t, sent); got != "conversation responsive" {
		t.Fatal(got)
	}
	add(3, "cancel one")
	if got := receive(t, sent); got != "one cancelled" {
		t.Fatal(got)
	}
	if got := receive(t, started); got != 5 {
		t.Fatalf("next task: %d", got)
	}
	close(release)
	for i := 0; i < 4; i++ {
		if got := receive(t, sent); !strings.Contains(got, "已经处理完了") || !strings.HasSuffix(got, "\n\ncomplete") || strings.Contains(got, "「task 1」") {
			t.Fatal(got)
		}
	}
	if peak.Load() != 4 {
		t.Fatalf("peak task concurrency: %d", peak.Load())
	}
	task, err = s.Task(ctx, 1)
	if err != nil || task.Status != personal.TaskCancelled {
		t.Fatalf("cancelled task revived: %+v, %v", task, err)
	}
	events, err := s.RecentEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "memory" {
			t.Fatalf("ordinary memory notified: %+v", event)
		}
	}
	// A second connection demonstrates durable recall without invoking another model.
	reopened := testkit.Store(t, path, time.Minute)
	m, err := reopened.Memory(ctx, "reply_style")
	if err != nil || m == nil || m.Value != "简短回复" {
		t.Fatalf("durable memory: %+v, %v", m, err)
	}
}

func TestRuntimeContinuesSameTaskAndAccountsRecoveredRun(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "continuation.db"), time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "research in two stages")
	task, first := testkit.CreateTask(t, s, event, "create", "research", "research in two stages", "")
	testkit.FinishEvent(t, s, event, personal.TurnOutput{}, nil)
	claimed, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimed == nil {
		t.Fatal(err)
	}
	if _, err = s.AccountRun(ctx, claimed.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	stages := 0
	r.Execute = func(ctx context.Context, run personal.Run, _ func(string) error) (personal.StageResult, error) {
		stages++
		var input personal.TaskInput
		if err := json.Unmarshal([]byte(run.Input), &input); err != nil {
			return personal.StageResult{}, err
		}
		if stages == 2 && (!strings.Contains(input.Previous, "first evidence") || input.Goal != task.Goal || run.TaskID != task.ID) {
			return personal.StageResult{}, fmt.Errorf("lost prior stage: %+v", input)
		}
		if !wait(ctx, 15*time.Millisecond) {
			return personal.StageResult{}, ctx.Err()
		}
		summary := "first evidence"
		if stages == 2 {
			summary = "final evidence"
		}
		return personal.StageResult{Summary: summary}, nil
	}
	r.Think = func(ctx context.Context, in personal.TurnInput) (personal.TurnOutput, error) {
		if stages == 1 {
			_, _, err := s.ContinueTask(ctx, in.Event.ID, "next", task.ID, task.Revision, "finish using first evidence", false)
			// This time must count even after the next Run has been enqueued.
			if !wait(ctx, 15*time.Millisecond) {
				return personal.TurnOutput{}, ctx.Err()
			}
			return personal.TurnOutput{Text: "continuing", TaskID: task.ID, Silent: true}, err
		}
		_, err := s.SettleTask(ctx, in.Event.ID, task.ID, task.Revision, personal.TaskCompleted, "final evidence")
		return personal.TurnOutput{Text: "complete", TaskID: task.ID}, err
	}
	if _, err := r.task(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Run(ctx, first.ID)
	if err != nil || got.ElapsedMS <= 1000 {
		t.Fatalf("recovered run lost new time: %+v, %v", got, err)
	}
	before, _ := s.Task(ctx, task.ID)
	if _, err := r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Task(ctx, task.ID)
	if after.UsedMS-before.UsedMS < 10 {
		t.Fatalf("continuation planning not charged: before=%d after=%d", before.UsedMS, after.UsedMS)
	}
	if message, err := s.ClaimDelivery(ctx); err != nil || message != nil {
		t.Fatalf("ordinary continuation notified: %+v, %v", message, err)
	}
	if _, err := r.task(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	after, err = s.Task(ctx, task.ID)
	if err != nil || after.Status != personal.TaskCompleted || after.Summary != "final evidence" || after.BudgetMS != 60000 {
		t.Fatalf("final task: %+v, %v", after, err)
	}
	if message, err := s.ClaimDelivery(ctx); err != nil || message == nil || !strings.Contains(message.Text, "已经处理完了") || !strings.Contains(message.Text, "final evidence") {
		t.Fatalf("completion: %+v, %v", message, err)
	}
}

func TestRuntimeBudgetStopsExecutionAndNotifiesWithoutTaskPlanning(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "budget.db"), 25*time.Millisecond)
	event := testkit.OpenUserEvent(t, s, 1, 1, "long task")
	task, _ := testkit.CreateTask(t, s, event, "create", "long", "long task", "")
	testkit.FinishEvent(t, s, event, personal.TurnOutput{}, nil)
	r := testRuntime(s)
	r.Execute = func(ctx context.Context, _ personal.Run, _ func(string) error) (personal.StageResult, error) {
		<-ctx.Done()
		return personal.StageResult{Summary: "partial findings"}, ctx.Err()
	}
	r.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
		t.Error("exhausted task called thinker")
		return personal.TurnOutput{}, nil
	}
	if _, err := r.task(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, task.ID)
	if err != nil || got.Status != personal.TaskWaitingBudget || got.UsedMS < got.BudgetMS {
		t.Fatalf("budget state: %+v, %v", got, err)
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil || !strings.Contains(message.Text, "partial findings") {
		t.Fatalf("budget notice: %+v, %v", message, err)
	}
}
