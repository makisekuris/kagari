package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"kagari/internal/logging"
	"kagari/internal/personal"
	"kagari/internal/personal/assistant"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

// Run only this suite: KAGARI_LIVE_EVAL=1 KAGARI_EVAL_CONFIG_DIR=/path/to/config go test ./internal/personal/service -run '^TestLivePersonal' -count=1 -v.
// Core scenarios use eleven model turns; tool iterations and one formatting repair are bounded separately.
// All owner data is fictional.
func TestLivePersonalMemoryLifecycle(t *testing.T) {
	a := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "memory.db"), 10*time.Minute)
	a.Store = s
	var original personal.Memory

	if !t.Run("automatic_stable_preference", func(t *testing.T) {
		event := testkit.OpenUserEvent(t, s, 1, 1, "我平时一贯喜欢喝无糖绿茶。刚刚结束了一段散步。")
		testkit.FinishEvent(t, s, event, personal.TurnOutput{Text: "收到。"}, nil)
		stableReview := livePersonalMemoryStep(t, ctx, s, a)
		memories, err := s.Memories(ctx, "", 20)
		if err != nil || len(stableReview.Changes) != 1 || len(memories) != 1 || !strings.Contains(memories[0].Value, "无糖绿茶") {
			t.Fatalf("semantic FAIL: ordinary stable preference was not persisted as one memory (changes=%+v question=%q memories=%+v error=%s)", stableReview.Changes, stableReview.Question, memories, logging.ErrorReason(err))
		}
		original = memories[0]
		t.Log("mechanism PASS: CompleteEvent → background reviewer → real SQLite; semantic PASS: automatic stable preference")
	}) {
		return
	}

	t.Run("temporary_change_does_not_overwrite", func(t *testing.T) {
		event := testkit.OpenUserEvent(t, s, 2, 2, "今天只是这一次改喝加糖红茶，明天恢复平时习惯；这不是长期偏好的改变。")
		// Isolate the background extractor: the owner event is completed without another conversational model call.
		testkit.FinishEvent(t, s, event, personal.TurnOutput{Text: "收到。"}, nil)
		livePersonalMemoryStep(t, ctx, s, a)
		got, err := s.Memory(ctx, original.Key)
		active, listErr := s.Memories(ctx, "", 20)
		if err != nil || listErr != nil || got == nil || got.Version != original.Version || got.Value != original.Value || len(active) != 1 {
			t.Fatal("semantic FAIL: temporary change replaced or added a durable preference")
		}
		t.Log("mechanism PASS: automatic review completed; semantic PASS: temporary preference did not alter durable memory")
	})

	t.Run("correction_reuses_key", func(t *testing.T) {
		event := testkit.OpenUserEvent(t, s, 3, 3, "纠正一下我之前说的长期饮料偏好：我平时一贯喜欢的是无糖乌龙茶，不是无糖绿茶。")
		testkit.FinishEvent(t, s, event, personal.TurnOutput{Text: "收到更正。"}, nil)
		livePersonalMemoryStep(t, ctx, s, a)
		got, err := s.Memory(ctx, original.Key)
		active, listErr := s.Memories(ctx, "", 20)
		if err != nil || listErr != nil || got == nil || got.Version != original.Version+1 || !strings.Contains(got.Value, "无糖乌龙茶") || len(active) != 1 {
			t.Fatal("semantic FAIL: explicit correction did not update the original key exactly once")
		}
		t.Log("mechanism PASS: versioned SQLite update; semantic PASS: correction reused original memory key")
	})
}

func TestLivePersonalMemoryScope(t *testing.T) {
	a := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	runReview := func(t *testing.T, updateID, messageID int64, text string) (personal.MemoryReview, []personal.Memory, []personal.Memory) {
		t.Helper()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "memory-scope.db"), 10*time.Minute)
		a.Store = s
		event := testkit.OpenUserEvent(t, s, updateID, messageID, text)
		testkit.FinishEvent(t, s, event, personal.TurnOutput{Text: "收到。"}, nil)
		review := livePersonalMemoryStep(t, ctx, s, a)
		active, err := s.Memories(ctx, "", 20)
		if err != nil {
			t.Fatal(err)
		}
		history, err := s.MemoryHistory(ctx, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		return review, active, history
	}

	t.Run("task_local_schedule_conditions_are_not_preferences", func(t *testing.T) {
		const event = "请登记临时测试计划「E2E 每日练习」：每天 09:00 UTC 生成一道 Go 练习题，每次独立生成，不持续检查新信息，不需要固定来源，不要立即执行。"
		review, active, history := runReview(t, 13, 13, event)
		if len(review.Changes) != 0 || len(active) != 0 || len(history) != 0 {
			t.Fatalf("semantic FAIL: plan-local execution conditions became durable preferences: review=%+v active_memories=%+v memory_history=%+v", review, active, history)
		}
		t.Log("mechanism PASS: real memory review completed into isolated SQLite; semantic PASS: no plan-local conditions were persisted as preferences")
	})

	t.Run("explicit_cross_plan_preference_is_saved", func(t *testing.T) {
		const event = "以后我的所有定时计划都默认使用 UTC 时区，这是我的长期偏好。"
		review, active, history := runReview(t, 14, 14, event)
		if len(review.Changes) != 1 || len(active) != 1 || len(history) != 1 || !strings.Contains(strings.ToLower(active[0].Value), "utc") {
			t.Fatalf("semantic FAIL: explicit long-term UTC preference was not saved as one stable memory: review=%+v active_memories=%+v memory_history=%+v", review, active, history)
		}
		t.Logf("mechanism PASS: explicit preference written to isolated SQLite; semantic PASS: UTC preference retained under key %q", active[0].Key)
	})

	t.Run("one_off_timer_settings_are_not_memories", func(t *testing.T) {
		const event = "番茄钟这次设置为30分钟专注、10分钟休息，结束时给我发消息提醒。"
		review, active, history := runReview(t, 15, 15, event)
		if len(review.Changes) != 0 || len(active) != 0 || len(history) != 0 {
			t.Fatalf("semantic FAIL: one-off timer settings became a durable preference: review=%+v active_memories=%+v memory_history=%+v", review, active, history)
		}
		t.Log("mechanism PASS: real memory review completed into isolated SQLite; semantic PASS: one-off timer settings did not become a memory")
	})

	t.Run("explicit_long_term_timer_default_is_saved", func(t *testing.T) {
		const event = "以后我的番茄钟都默认专注30分钟、休息10分钟，这是我的长期偏好。"
		review, active, history := runReview(t, 16, 16, event)
		if len(review.Changes) != 1 || len(active) != 1 || len(history) != 1 || !strings.Contains(active[0].Value, "30") || !strings.Contains(active[0].Value, "10") {
			t.Fatalf("semantic FAIL: explicit long-term timer default was not stored: review=%+v active_memories=%+v memory_history=%+v", review, active, history)
		}
		t.Logf("mechanism PASS: explicit default written to isolated SQLite; semantic PASS: long-term timer preference retained under key %q", active[0].Key)
	})

	t.Run("plan_and_separate_durable_preference_store_only_preference", func(t *testing.T) {
		const event = "请登记临时计划「睡前法语词卡」：每周二、四 20:30 Asia/Shanghai 生成5张A2词汇卡，重复触发时沿用整份定义。无论是否安排这项计划，我长期更喜欢法语练习附上简短中文释义。"
		review, active, history := runReview(t, 17, 17, event)
		if len(review.Changes) != 1 || len(active) != 1 || len(history) != 1 {
			t.Fatalf("semantic FAIL: mixed plan and preference did not produce exactly one durable memory: review=%+v active_memories=%+v memory_history=%+v", review, active, history)
		}
		value := active[0].Value
		if !strings.Contains(value, "中文释义") || strings.Contains(value, "睡前法语词卡") || strings.Contains(value, "20:30") || strings.Contains(value, "Asia/Shanghai") || strings.Contains(value, "A2") {
			t.Fatalf("semantic FAIL: memory did not isolate the cross-context preference from the plan definition: memory=%+v", active[0])
		}
		t.Logf("mechanism PASS: mixed-scope review written to isolated SQLite; semantic PASS: only the independent preference was retained under key %q", active[0].Key)
	})
}

func TestLivePersonalForgetAndHistory(t *testing.T) {
	a := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "forget.db"), 10*time.Minute)
	a.Store = s
	seed := testkit.OpenUserEvent(t, s, 1, 1, "我的测试笔记本叫白桦。")
	testkit.FinishEvent(t, s, seed, personal.TurnOutput{}, nil)
	stableRun, err := s.ClaimRun(ctx, personal.RunMemoryReview)
	if err != nil || stableRun == nil {
		t.Fatal("cannot claim fictional seed review")
	}
	stableReview := personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "test_laptop_name", Value: "白桦", Evidence: seed.Text}}}
	if err := s.FinishMemoryReview(ctx, stableRun.ID, stableReview, ""); err != nil {
		t.Fatal(err)
	}
	original, err := s.Memory(ctx, "test_laptop_name")
	if err != nil || original == nil || original.Deleted {
		t.Fatal("cannot persist fictional seed memory")
	}
	if !t.Run("forget_confirmation_does_not_repeat_removed_value", func(t *testing.T) {
		event := testkit.OpenUserEvent(t, s, 2, 2, "请忘掉我的测试笔记本名称。")
		output := livePersonalThink(t, ctx, a, livePersonalInput(t, ctx, s, event))
		if strings.TrimSpace(output.Text) == "" || strings.Contains(output.Text, "白桦") {
			t.Fatalf("semantic FAIL: forget confirmation was empty or disclosed the removed value: %q", output.Text)
		}
		forgotten, err := s.Memory(ctx, original.Key)
		if err != nil || forgotten == nil || !forgotten.Deleted || forgotten.Value != "" || forgotten.Version != original.Version+1 {
			t.Fatal("mechanism FAIL: explicit forgetting was not persisted by the tool call")
		}
		testkit.FinishEvent(t, s, event, output, nil)
		livePersonalMemoryStep(t, ctx, s, a)
		got, err := s.Memory(ctx, original.Key)
		if err != nil || got == nil || !got.Deleted || got.Value != "" || got.Version != original.Version+1 {
			t.Fatal("semantic FAIL: explicit forgetting did not persist an empty tombstone")
		}
		if err := s.FinishMemoryReview(ctx, stableRun.ID, stableReview, ""); err != nil {
			t.Fatal(err)
		}
		got, err = s.Memory(ctx, original.Key)
		if err != nil || got == nil || !got.Deleted || got.Value != "" {
			t.Fatal("mechanism FAIL: replaying a completed old review resurrected forgotten data")
		}
		t.Log("mechanism PASS: forget tool → synchronous write → tombstone; semantic PASS: confirmation and old review replay did not restore or disclose the value")
	}) {
		return
	}

	t.Run("current_value_query_does_not_disclose_removed_value", func(t *testing.T) {
		event := testkit.OpenUserEvent(t, s, 3, 3, "按现在有效的长期记忆，我的测试笔记本叫什么？")
		output := livePersonalThink(t, ctx, a, livePersonalInput(t, ctx, s, event))
		testkit.FinishEvent(t, s, event, output, nil)
		if !output.SkipReplyPresentation || output.Text != "当前没有查到匹配的有效记忆。" {
			t.Fatalf("expected the current-memory lookup result after forgetting, got %q (skip presentation=%v)", output.Text, output.SkipReplyPresentation)
		}
		active, err := s.Memories(ctx, "", 20)
		if err != nil || len(active) != 0 {
			t.Fatal("mechanism FAIL: forgotten name remains in active recall")
		}
		t.Log("mechanism PASS: old conversation remained available and memory search stayed empty; semantic PASS: current-value answer did not disclose the removed value")
	})
}

func TestLivePersonalRecallAfterRestart(t *testing.T) {
	a := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	path := filepath.Join(t.TempDir(), "recall.db")
	s := testkit.Store(t, path, 10*time.Minute)
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	code := hex.EncodeToString(random)
	event := testkit.OpenUserEvent(t, s, 1, 1, "请记住我的虚构编辑器配色代号是 "+code+"。")
	if _, err := s.WriteMemory(ctx, event.ID, "seed-editor", personal.MemoryChange{Operation: "put", Key: "editor.color_code", Value: "虚构编辑器配色代号 " + code, Evidence: event.Text}); err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, s, event, personal.TurnOutput{}, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testkit.Store(t, path, 10*time.Minute)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	spy := &livePersonalRecallStore{Store: s}
	a.Store = spy
	event = testkit.OpenUserEvent(t, s, 2, 2, "我的编辑器配色代号是什么？请查询长期记忆后原样回答。")
	// Omit both memory preload and history: the random answer is available only through real repository tools.
	output := livePersonalThink(t, ctx, a, personal.TurnInput{Event: event})
	testkit.FinishEvent(t, s, event, output, nil)
	if spy.reads == 0 {
		t.Fatal("mechanism FAIL: no memory retrieval tool reached the restarted Store")
	}
	if !strings.Contains(output.Text, code) {
		t.Fatal("semantic FAIL: retrieved answer did not contain the persisted random value")
	}
	t.Logf("mechanism PASS: restarted SQLite and %d memory tool reads; semantic PASS: correct persisted value recalled", spy.reads)
}

func TestLivePersonalTaskCallbacks(t *testing.T) {
	a := livePersonalAgent(t)
	for _, outcome := range []string{"continue_original_goal", "settle_completed_goal"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s := testkit.Store(t, filepath.Join(t.TempDir(), "tasks.db"), 10*time.Minute)
			a.Store = s
			seed := testkit.OpenUserEvent(t, s, 1, 1, "比较虚构产品甲和乙的月费与离线能力，两项都需要核实，最后给我一个对照结论。")
			task, run := testkit.CreateTask(t, s, seed, "seed-task", "虚构产品对比", seed.Text, "仅处理本测试提供的虚构产品材料。")
			testkit.FinishEvent(t, s, seed, personal.TurnOutput{TaskID: task.ID}, nil)
			claimed, err := s.ClaimRun(ctx, personal.RunTask)
			if err != nil || claimed == nil || claimed.ID != run.ID {
				t.Fatal("mechanism FAIL: task seed was not claimable")
			}
			result := personal.StageResult{Summary: "已核实月费：甲每月10元，乙每月20元。离线能力尚未核实，原目标尚未完成。下一步需继续核实甲、乙的离线能力，再给出包含月费与离线能力的对照结论。"}
			if outcome == "settle_completed_goal" {
				result = personal.StageResult{Summary: "核实已全部完成：甲月费10元，支持离线；乙月费20元，不支持离线。完整对照结论：甲更便宜且有离线能力；乙月费更高且无离线能力。原目标要求的两项已全部有明确依据，无后续事项。"}
			}
			if err := s.FinishStage(ctx, run.ID, result, ""); err != nil {
				t.Fatal(err)
			}
			event := livePersonalTaskEvent(t, ctx, s, task.ID)
			output := livePersonalThink(t, ctx, a, livePersonalInput(t, ctx, s, event))
			testkit.FinishEvent(t, s, event, output, nil)
			got, err := s.Task(ctx, task.ID)
			runs, listErr := s.Runs(ctx, task.ID, 10)
			if err != nil || listErr != nil || got == nil || got.Goal != task.Goal || got.Revision != task.Revision || got.BudgetMS != task.BudgetMS {
				t.Fatal("mechanism FAIL: callback lost or changed the original task, goal, revision, or budget")
			}
			if outcome == "continue_original_goal" {
				if got.Status != personal.TaskQueued || len(runs) != 2 {
					t.Fatalf("semantic FAIL: callback did not continue its own original task (status=%s runs=%d)", got.Status, len(runs))
				}
				t.Log("mechanism PASS: callback queued another run; semantic PASS: original task, goal, revision, and budget preserved")
			} else {
				if got.Status != personal.TaskCompleted || len(runs) != 1 || output.Silent {
					t.Fatalf("semantic FAIL: complete callback evidence was not settled and notified (status=%s runs=%d silent=%v)", got.Status, len(runs), output.Silent)
				}
				t.Log("mechanism PASS: settle_task persisted completed and notified; semantic PASS: completion stayed within original goal")
			}
		})
	}
}

func livePersonalAgent(t *testing.T) *assistant.MainAgent {
	t.Helper()
	engine := testkit.LiveEngine(t)
	return &assistant.MainAgent{Model: engine.Model, Profile: engine.Profile, MaxIterations: engine.Config.Agent.MaxIterations, Timeout: 2 * time.Minute}
}

func livePersonalThink(t *testing.T, ctx context.Context, a *assistant.MainAgent, input personal.TurnInput) personal.TurnOutput {
	t.Helper()
	started := time.Now()
	core, observed := observer.New(zapcore.InfoLevel)
	logger := logging.FromContext(ctx).WithOptions(zap.WrapCore(func(existing zapcore.Core) zapcore.Core {
		return zapcore.NewTee(existing, core)
	}))
	ctx = logging.WithLogger(ctx, logger)
	output, err := a.Think(ctx, input)
	t.Logf("real-model Think duration=%s success=%v max_iterations=%d event_kind=%s event_id=%d task_id=%d memories=%d", time.Since(started).Round(time.Millisecond), err == nil, a.MaxIterations, input.Event.Kind, input.Event.ID, input.Event.TaskID, len(input.Memories))
	if err == nil {
		t.Logf("fictional evaluation response=%q", output.Text)
	}
	if err != nil {
		for _, entry := range observed.FilterMessage("tool.finished").All() {
			fields := entry.ContextMap()
			payload, _ := json.Marshal(fields["payload"])
			t.Logf("fictional Think tool=%v outcome=%v input/output=%s", fields["tool_name"], fields["outcome"], payload)
		}
		reason := logging.ErrorReason(err)
		if strings.Contains(err.Error(), "personal response") || strings.Contains(err.Error(), "model returned blank") {
			reason = err.Error()
		}
		t.Fatalf("real-model Think failed: %s (configuration withheld)", reason)
	}
	return output
}

func livePersonalInput(t *testing.T, ctx context.Context, s *sqlite.Store, event personal.Event) personal.TurnInput {
	t.Helper()
	input, err := (&Runtime{Store: s}).context(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func livePersonalMemoryStep(t *testing.T, ctx context.Context, store *sqlite.Store, agent *assistant.MainAgent) personal.MemoryReview {
	t.Helper()
	started := time.Now()
	var review personal.MemoryReview
	var reviewErr error
	runtime := &Runtime{Store: store, ReviewMemory: func(ctx context.Context, input personal.MemoryReviewInput) (personal.MemoryReview, error) {
		review, reviewErr = agent.ReviewMemory(ctx, input)
		return review, reviewErr
	}}
	worked, err := runtime.memory(ctx)
	if err != nil || !worked {
		t.Fatalf("memory worker did not finish: worked=%v error=%s", worked, logging.ErrorReason(err))
	}
	// The worker persists reviewer errors and stays silent. An empty review is
	// evidence about memory extraction only after the model actually succeeded.
	if reviewErr != nil {
		t.Fatalf("real-model memory review failed: %s (configuration withheld)", logging.ErrorReason(reviewErr))
	}
	t.Logf("memory worker duration=%s", time.Since(started).Round(time.Millisecond))
	return review
}

func livePersonalTaskEvent(t *testing.T, ctx context.Context, s *sqlite.Store, taskID int64) personal.Event {
	t.Helper()
	event, err := s.ClaimEvent(ctx)
	if err != nil || event == nil || event.Kind != personal.EventTask || event.TaskID != taskID {
		t.Fatal("mechanism FAIL: expected persisted callback for current task")
	}
	return *event
}

type livePersonalRecallStore struct {
	*sqlite.Store
	reads int
}

func (s *livePersonalRecallStore) Memory(ctx context.Context, key string) (*personal.Memory, error) {
	s.reads++
	return s.Store.Memory(ctx, key)
}

func (s *livePersonalRecallStore) Memories(ctx context.Context, query string, limit int) ([]personal.Memory, error) {
	s.reads++
	return s.Store.Memories(ctx, query, limit)
}
