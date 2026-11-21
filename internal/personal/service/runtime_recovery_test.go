package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRuntimeRecoveredUserTurnPreservesActionsWithoutReplayingTools(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "research this")
	task, first := testkit.CreateTask(t, s, event, "first", "research", "research this", "")
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	r.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
		t.Error("interrupted turn was replanned")
		return personal.TurnOutput{}, nil
	}
	if _, err := r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Tasks(ctx, 10)
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatalf("recovery duplicated work: %+v, %v", tasks, err)
	}
	run, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil || run.ID != first.ID {
		t.Fatalf("recovery did not release original task: %+v, %v", run, err)
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil || !strings.Contains(message.Text, "中断") {
		t.Fatalf("recovery did not explain interruption: %+v, %v", message, err)
	}
}

func TestRuntimeRecoveredUserTurnReportsCommittedMemoryAndTimingOutcomes(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	seed := testkit.OpenUserEvent(t, s, 1, 1, "Remember that I prefer tea.")
	if _, err := s.WriteMemory(ctx, seed.ID, "seed-memory", personal.MemoryChange{
		Operation: "put", Key: "drink.preference", Value: "tea", Evidence: seed.Text,
	}); err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, s, seed, personal.TurnOutput{Silent: true}, nil)

	event := testkit.OpenUserEvent(t, s, 2, 2, "把口味更正为咖啡，再忘掉这条口味记忆；记住偏好说明；同时每天 9 点检查天气，并在 10 分钟后提醒我喝水。")
	corrected, err := s.WriteMemory(ctx, event.ID, "correct-memory", personal.MemoryChange{
		Operation: "put", Key: "drink.preference", Value: "coffee", ExpectedVersion: 1, Evidence: "更正为咖啡",
	})
	if err != nil || corrected.Version != 2 {
		t.Fatalf("correct memory = %+v, %v", corrected, err)
	}
	forgotten, err := s.WriteMemory(ctx, event.ID, "forget-memory", personal.MemoryChange{
		Operation: "forget", Key: "drink.preference", ExpectedVersion: corrected.Version, Evidence: "忘掉这条口味记忆",
	})
	if err != nil || !forgotten.Deleted || forgotten.Value != "" || forgotten.Version != 3 {
		t.Fatalf("forget memory = %+v, %v", forgotten, err)
	}
	longValue := strings.Repeat("长", 4000)
	if _, err := s.WriteMemory(ctx, event.ID, "put-long-memory", personal.MemoryChange{
		Operation: "put", Key: "research.preference", Value: longValue, Evidence: "记住偏好说明",
	}); err != nil {
		t.Fatal(err)
	}
	reminder, _, err := s.CreateReminder(ctx, event.ID, "remind-water", personal.ReminderInput{
		Title: "喝水提醒", Steps: []personal.ReminderStep{{AfterMinutes: 10, Text: "喝水"}},
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	schedule, _, err := s.CreateSchedule(ctx, event.ID, "check-weather", personal.ScheduleInput{
		Title: "天气检查", Goal: "检查天气并报告", Cron: "0 9 * * *", Timezone: "Asia/Shanghai", WindowMode: personal.WindowNone,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	r.Timezone = "Asia/Shanghai"
	r.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
		t.Error("interrupted turn was replanned")
		return personal.TurnOutput{}, nil
	}
	if _, err := r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil {
		t.Fatalf("recovery delivery = %+v, %v", message, err)
	}
	for _, want := range []string{
		"长期记忆「drink.preference」已遗忘（版本 3）",
		"长期记忆「research.preference」已更新（版本 1）",
		"计时提醒 #" + fmt.Sprint(reminder.ID) + "「喝水提醒」已启用",
		"计划 #" + fmt.Sprint(schedule.ID) + "「天气检查」已启用",
		"以上已提交操作已保存",
	} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("recovery omitted %q: %s", want, message.Text)
		}
	}
	for _, forgottenValue := range []string{"tea", "coffee", "咖啡"} {
		if strings.Contains(message.Text, forgottenValue) {
			t.Errorf("recovery disclosed forgotten memory value %q: %s", forgottenValue, message.Text)
		}
	}
	if strings.Contains(message.Text, longValue) || len([]rune(message.Text)) > 600 {
		t.Fatalf("recovery echoed the full 4000-character memory value or became verbose: %d runes", len([]rune(message.Text)))
	}
	if got, err := s.EventMemoryReceipts(ctx, event.ID); err != nil || len(got) != 1 || !got[0].Deleted || got[0].Value != "" || got[0].Version != 3 {
		if err != nil || len(got) != 2 || got[0].Deleted != true || got[0].Value != "" || got[0].Version != 3 || got[1].Value != longValue || got[1].Version != 1 {
			t.Fatalf("latest memory receipts = %+v, %v", got, err)
		}
	}
	history, err := s.History(ctx, event.ID+1, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range history {
		if message.EventID == event.ID && message.Role == "assistant" && message.ScheduleID != 0 {
			t.Fatalf("mixed memory and timing recovery was associated with one schedule: %+v", message)
		}
	}
}

func TestRuntimeRecoveredCallbackReleasesOnlyItsCommittedContinuation(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "research")
	task, first := testkit.CreateTask(t, s, event, "create", "research", "research", "")
	testkit.FinishEvent(t, s, event, personal.TurnOutput{}, nil)
	if _, err := s.ClaimRun(ctx, personal.RunTask); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, first.ID, personal.StageResult{Summary: "first result"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, next, err := s.ContinueTask(ctx, callback.ID, "continue", task.ID, task.Revision, "finish", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	r.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
		t.Error("callback replanned")
		return personal.TurnOutput{}, nil
	}
	if _, err = r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimed == nil || claimed.ID != next.ID {
		t.Fatalf("continuation lost: %+v, %v", claimed, err)
	}
	if message, err := s.ClaimDelivery(ctx); err != nil || message != nil {
		t.Fatalf("ordinary recovered continuation notified: %+v, %v", message, err)
	}
}

func TestLongStageEvidenceRemainsQueryableAndCanContinue(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "research")
	task, first := testkit.CreateTask(t, s, event, "create", "research", "research", "")
	testkit.FinishEvent(t, s, event, personal.TurnOutput{}, nil)
	if _, err := s.ClaimRun(ctx, personal.RunTask); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("evidence", 6000)
	ref, err := s.SaveSource(ctx, first.ID, "recovery-source", domain.Source{
		URL: "https://example.com/source", Status: "ok", Content: content, FetchedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := personal.StageResult{Summary: strings.Repeat("summary ", 4000), Sources: []domain.SourceRef{ref}}
	if err := s.FinishStage(ctx, first.ID, result, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, next, err := s.ContinueTask(ctx, callback.ID, "next", task.ID, task.Revision, "check the next source", false)
	if err != nil {
		t.Fatal(err)
	}
	var input personal.TaskInput
	if err = json.Unmarshal([]byte(next.Input), &input); err != nil {
		t.Fatal(err)
	}
	if len([]rune(input.Previous)) > personal.MaxTaskChars || !json.Valid([]byte(input.Previous)) || !strings.Contains(input.Previous, "https://example.com/source") {
		t.Fatal("invalid bounded checkpoint")
	}
	stored, err := s.Run(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	var full personal.StageResult
	if err = json.Unmarshal([]byte(stored.Result), &full); err != nil || len(full.Sources) != 1 || full.Sources[0].ID != ref.ID {
		t.Fatal("full stage evidence was lost")
	}
	expanded, err := s.ExpandSource(ctx, full.Sources[0].ID)
	if err != nil || expanded.Content != content {
		t.Fatalf("expanded source body = %d runes, err=%v", len([]rune(expanded.Content)), err)
	}
}
