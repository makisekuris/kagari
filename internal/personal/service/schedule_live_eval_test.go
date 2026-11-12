package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"kagari/internal/domain"
	"kagari/internal/logging"
	"kagari/internal/personal"
	"kagari/internal/personal/assistant"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

func TestLivePersonalScheduleRegistration(t *testing.T) {
	main := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	store := testkit.Store(t, filepath.Join(t.TempDir(), "schedules.db"), 10*time.Minute)
	main.Store = store
	main.Timezone = "UTC"

	ordinary := testkit.OpenUserEvent(t, store, 501, 601,
		"请直接登记一个普通定时目标：每天 09:00 UTC，为虚构项目白桦生成一道 Go 练习题。每次独立生成，不持续检查新信息，也不需要固定来源或补查历史。")
	ordinaryOutput, ordinaryLogs := livePersonalScheduleThink(t, ctx, main, livePersonalInput(t, ctx, store, ordinary))
	testkit.FinishEvent(t, store, ordinary, ordinaryOutput, nil)
	assertScheduleOutputHasNoQuestionOrMemory(t, ctx, store, ordinary, ordinaryOutput, ordinaryLogs)
	if ordinaryOutput.Silent {
		t.Fatal("registration receipt must be visible")
	}
	ordinarySchedule, err := store.Schedule(ctx, ordinaryOutput.ScheduleID)
	if err != nil || ordinarySchedule == nil {
		t.Fatalf("registered ordinary schedule lookup = (%+v, %v)", ordinarySchedule, err)
	}

	continuous := testkit.OpenUserEvent(t, store, 502, 602,
		"请直接登记一个连续信息检查计划：每周一 10:30 UTC 检查虚构项目白桦的新增状态更新，来源是 https://status.example.invalid/baihua。检查从上次成功检查后开始，必须确认该来源完整读取；如果来源失败就报告不完整。")
	continuousOutput, continuousLogs := livePersonalScheduleThink(t, ctx, main, livePersonalInput(t, ctx, store, continuous))
	testkit.FinishEvent(t, store, continuous, continuousOutput, nil)
	assertScheduleOutputHasNoQuestionOrMemory(t, ctx, store, continuous, continuousOutput, continuousLogs)
	if continuousOutput.Silent {
		t.Fatal("registration receipt must be visible")
	}
	tasks, err := store.Tasks(ctx, 20)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("semantic FAIL: schedule registration created unrelated Tasks, including cancelled ones: tasks=%+v error=%v ordinary_output=%+v continuous_output=%+v", tasks, err, ordinaryOutput, continuousOutput)
	}
	continuousSchedule, err := store.Schedule(ctx, continuousOutput.ScheduleID)
	if err != nil || continuousSchedule == nil {
		t.Fatalf("registered continuous schedule lookup = (%+v, %v)", continuousSchedule, err)
	}

	schedules, err := store.Schedules(ctx, false, 0, 20)
	if err != nil || len(schedules) != 2 {
		t.Fatalf("semantic FAIL: expected two fictional schedules, got %d (%s)", len(schedules), err)
	}
	if ordinarySchedule == nil || ordinarySchedule.WindowMode != personal.WindowNone || ordinarySchedule.Timezone != "UTC" || ordinarySchedule.Cron != "0 9 * * *" || ordinarySchedule.NextRunAt == nil {
		t.Fatalf("semantic FAIL: ordinary goal was not stored as a normal schedule: %+v", ordinarySchedule)
	}
	if continuousSchedule == nil || continuousSchedule.Cron != "30 10 * * 1" || continuousSchedule.WindowMode != personal.WindowSinceSuccess || continuousSchedule.Timezone != "UTC" || continuousSchedule.BaselineAt == nil || len(continuousSchedule.RequiredSources) != 1 || continuousSchedule.RequiredSources[0].URL != "https://status.example.invalid/baihua" {
		t.Fatalf("semantic FAIL: continuous check did not retain its coverage source: %+v", continuousSchedule)
	}
	t.Log("mechanism PASS: two real SQLite plans persisted; semantic PASS: ordinary generation and since-success source check kept distinct modes")
}

func TestLivePersonalScheduleAmbiguousCancellationAsksQuestion(t *testing.T) {
	main := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	store := testkit.Store(t, filepath.Join(t.TempDir(), "schedule-ambiguous-cancel.db"), 10*time.Minute)
	main.Store = store
	main.Timezone = "UTC"

	seedSchedule := func(updateID, messageID int64, title, goal, cron string) *personal.Schedule {
		t.Helper()
		seed := testkit.OpenUserEvent(t, store, updateID, messageID, "为取消歧义测试预置虚构计划")
		schedule, _, err := store.CreateSchedule(ctx, seed.ID, "ambiguous-cancel-seed-"+strconv.FormatInt(updateID, 10), personal.ScheduleInput{
			Title: title, Goal: goal, Cron: cron, Timezone: "UTC", WindowMode: personal.WindowNone,
		}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		testkit.FinishEvent(t, store, seed, personal.TurnOutput{ScheduleID: schedule.ID, Text: "隔离测试计划已准备。"}, nil)
		if schedule.Status != personal.ScheduleActive || schedule.Revision != 1 {
			t.Fatalf("seed schedule is not active at revision 1: %+v", schedule)
		}
		return &schedule
	}
	plans := []*personal.Schedule{
		seedSchedule(531, 631, "白桦每日 Go 练习题", "为虚构项目白桦生成一道新的 Go 练习题。", "0 9 * * *"),
		seedSchedule(532, 632, "白桦每周状态检查", "检查虚构项目白桦的新增状态更新。", "30 10 * * 1"),
	}

	event := testkit.OpenUserEvent(t, store, 533, 633, "我要取消其中一个计划，先确认我要取消哪一个，不要自行选择。")
	output, entries := livePersonalScheduleThink(t, ctx, main, livePersonalInput(t, ctx, store, event))
	testkit.FinishEvent(t, store, event, output, nil)
	pendingQuestions := replaySetUserQuestions(t, entries)
	if len(pendingQuestions) == 0 || output.Silent || strings.TrimSpace(output.Text) == "" {
		t.Fatalf("semantic FAIL: ambiguous cancellation needs a visible clarification; pending_questions=%q output=%+v", pendingQuestions, output)
	}
	asksWhichPlan := false
	for _, question := range pendingQuestions {
		if !strings.Contains(output.Text, strings.TrimSpace(question)) {
			t.Fatalf("semantic FAIL: pending clarification is not visible in reply: question=%q output=%+v", question, output)
		}
		asksWhichPlan = asksWhichPlan || (strings.Contains(question, "取消") && (strings.Contains(question, "哪") || (strings.Contains(question, "练习") && strings.Contains(question, "状态"))))
	}
	if !asksWhichPlan {
		t.Fatalf("semantic FAIL: pending clarification does not ask which plan to cancel: questions=%q", pendingQuestions)
	}

	for _, before := range plans {
		after, err := store.Schedule(ctx, before.ID)
		if err != nil || after == nil || after.Revision != before.Revision || after.Status != personal.ScheduleActive || after.Cron != before.Cron || after.Title != before.Title || after.Goal != before.Goal {
			t.Fatalf("semantic FAIL: asking which plan to cancel changed the plan: before=%+v after=%+v error=%v", before, after, err)
		}
	}
	allPlans, err := store.Schedules(ctx, true, 0, 20)
	if err != nil || len(allPlans) != len(plans) {
		t.Fatalf("mechanism FAIL: ambiguous cancellation changed plan count: plans=%+v error=%v", allPlans, err)
	}
	tasks, err := store.Tasks(ctx, 20)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("semantic FAIL: clarification turn created an unrelated Task: tasks=%+v error=%v", tasks, err)
	}
	memories, err := store.Memories(ctx, "", 20)
	if err != nil || len(memories) != 0 {
		t.Fatalf("semantic FAIL: clarification turn created an unrelated memory: memories=%+v error=%v", memories, err)
	}
	history, err := store.MemoryHistory(ctx, "", 50)
	if err != nil || len(history) != 0 {
		t.Fatalf("semantic FAIL: clarification turn changed memory history: history=%+v error=%v", history, err)
	}
	t.Logf("semantic PASS: ambiguous cancellation left both plans active and unchanged; pending questions=%q", pendingQuestions)
}

func TestLivePersonalScheduleLifecycle(t *testing.T) {
	main := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	store := testkit.Store(t, filepath.Join(t.TempDir(), "schedule-lifecycle.db"), 10*time.Minute)
	main.Store = store
	main.Timezone = "UTC"

	seed := testkit.OpenUserEvent(t, store, 521, 621, "创建隔离用的虚构计划")
	createdAt := time.Now().UTC()
	schedule, _, err := store.CreateSchedule(ctx, seed.ID, "live-lifecycle-seed", personal.ScheduleInput{
		Title: "白桦每日练习",
		Goal:  "为虚构项目白桦生成一道 Go 练习题。",
		Cron:  "0 9 * * *", Timezone: "UTC", WindowMode: personal.WindowNone,
	}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, seed, personal.TurnOutput{ScheduleID: schedule.ID, Text: "隔离测试计划已准备。"}, nil)
	if schedule.Revision != 1 || schedule.Status != personal.ScheduleActive || schedule.NextRunAt == nil || !schedule.NextRunAt.After(createdAt) {
		t.Fatalf("mechanism FAIL: seeded plan has invalid initial state: %+v", schedule)
	}

	userTurn := func(updateID int64, text string) personal.Event {
		event := testkit.OpenUserEvent(t, store, updateID, updateID+100, text)
		output, logs := livePersonalScheduleThink(t, ctx, main, livePersonalInput(t, ctx, store, event))
		testkit.FinishEvent(t, store, event, output, nil)
		assertScheduleOutputHasNoQuestionOrMemory(t, ctx, store, event, output, logs)
		if output.Silent || output.ScheduleID != schedule.ID {
			t.Fatalf("semantic FAIL: direct schedule operation did not produce a visible reply for plan %d: %+v", schedule.ID, output)
		}
		return event
	}
	checkPlan := func(event personal.Event, revision int64, status personal.ScheduleStatus, cron string, wantNext bool) *personal.Schedule {
		t.Helper()
		got, err := store.Schedule(ctx, schedule.ID)
		if err != nil || got == nil {
			t.Fatalf("mechanism FAIL: schedule lookup = (%+v, %v)", got, err)
		}
		if got.Revision != revision || got.Status != status || got.Cron != cron || got.Timezone != "UTC" || got.AuthorizedEventID != event.ID {
			t.Fatalf("semantic FAIL: persisted schedule state = %+v, want revision=%d status=%s cron=%q authorized_event=%d", got, revision, status, cron, event.ID)
		}
		if wantNext {
			if got.NextRunAt == nil || !got.NextRunAt.After(got.UpdatedAt) {
				t.Fatalf("semantic FAIL: active schedule has no future next run: %+v", got)
			}
			want, err := personal.NextScheduleTime(got.Cron, got.Timezone, got.UpdatedAt)
			if err != nil || !got.NextRunAt.Equal(want) {
				t.Fatalf("semantic FAIL: next run = %v, want next cron activation %v (%v)", got.NextRunAt, want, err)
			}
		} else if got.NextRunAt != nil {
			t.Fatalf("semantic FAIL: inactive schedule retained next run %s", got.NextRunAt)
		}
		return got
	}

	changedCron := "15 10 * * *"
	change := userTurn(522, "请将白桦每日练习计划（计划 #"+strconv.FormatInt(schedule.ID, 10)+"）改为每天 UTC 10:15 执行，只调整执行时间，目标、时区和预算保持不变。")
	updated := checkPlan(change, schedule.Revision+1, personal.ScheduleActive, changedCron, true)
	if updated.Goal != schedule.Goal || updated.Title != schedule.Title || updated.TaskBudgetMS != schedule.TaskBudgetMS {
		t.Fatalf("semantic FAIL: changing execution time also changed plan content: %+v", updated)
	}

	pause := userTurn(523, "请暂停白桦每日练习计划（计划 #"+strconv.FormatInt(schedule.ID, 10)+"），不要删除。")
	paused := checkPlan(pause, updated.Revision+1, personal.SchedulePaused, changedCron, false)
	if paused.PauseReason != "manual" {
		t.Fatalf("semantic FAIL: manual pause reason = %q, want manual", paused.PauseReason)
	}

	resume := userTurn(524, "请恢复白桦每日练习计划（计划 #"+strconv.FormatInt(schedule.ID, 10)+"），从之后的正常周期执行。")
	resumed := checkPlan(resume, paused.Revision+1, personal.ScheduleActive, changedCron, true)
	if resumed.PauseReason != "" {
		t.Fatalf("semantic FAIL: resumed plan retained pause reason %q", resumed.PauseReason)
	}

	cancelEvent := userTurn(525, "请永久取消白桦每日练习计划（计划 #"+strconv.FormatInt(schedule.ID, 10)+"）。")
	cancelled := checkPlan(cancelEvent, resumed.Revision+1, personal.ScheduleCancelled, changedCron, false)
	if cancelled.PauseReason != "" {
		t.Fatalf("semantic FAIL: cancelled plan retained pause reason %q", cancelled.PauseReason)
	}
	activePlans, err := store.Schedules(ctx, false, 0, 20)
	if err != nil || len(activePlans) != 0 {
		t.Fatalf("mechanism FAIL: cancelled plan remained in active listing: plans=%+v error=%v", activePlans, err)
	}
	allPlans, err := store.Schedules(ctx, true, 0, 20)
	if err != nil || len(allPlans) != 1 || allPlans[0].ID != schedule.ID || allPlans[0].Status != personal.ScheduleCancelled {
		t.Fatalf("mechanism FAIL: cancelled plan history was not retained: plans=%+v error=%v", allPlans, err)
	}
	tasks, err := store.Tasks(ctx, 20)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("mechanism FAIL: lifecycle test unexpectedly created scheduled Tasks: tasks=%+v error=%v", tasks, err)
	}
	t.Log("mechanism PASS: four direct-user turns committed distinct schedule revisions to isolated SQLite; semantic PASS: cron update, manual pause/resume, and cancellation persisted without dispatching a Task")
}

func livePersonalScheduleThink(t *testing.T, ctx context.Context, main *assistant.MainAgent, input personal.TurnInput) (personal.TurnOutput, []observer.LoggedEntry) {
	t.Helper()
	core, observed := observer.New(zapcore.InfoLevel)
	logger := logging.FromContext(ctx).WithOptions(zap.WrapCore(func(existing zapcore.Core) zapcore.Core {
		return zapcore.NewTee(existing, core)
	}))
	ctx = logging.WithLogger(ctx, logger)
	output := livePersonalThink(t, ctx, main, input)
	entries := observed.FilterMessage("tool.finished").All()
	for _, entry := range entries {
		fields := entry.ContextMap()
		payload, _ := json.Marshal(fields["payload"])
		t.Logf("schedule tool trace name=%v outcome=%v payload=%s", fields["tool_name"], fields["outcome"], payload)
	}
	return output, entries
}

func assertScheduleOutputHasNoQuestionOrMemory(t *testing.T, ctx context.Context, store *sqlite.Store, event personal.Event, output personal.TurnOutput, entries []observer.LoggedEntry) {
	t.Helper()
	memories, err := store.Memories(ctx, "", 20)
	if err != nil {
		t.Fatalf("checking active memories after schedule event %d: %v; output=%+v", event.ID, err, output)
	}
	history, err := store.MemoryHistory(ctx, "", 50)
	if err != nil {
		t.Fatalf("checking memory history after schedule event %d: %v; output=%+v; active memories=%+v", event.ID, err, output, memories)
	}
	if len(memories) != 0 || len(history) != 0 {
		t.Fatalf("semantic FAIL: schedule event %d produced an unrelated memory; output=%+v active_memories=%+v memory_history=%+v", event.ID, output, memories, history)
	}
	pendingQuestions := replaySetUserQuestions(t, entries)
	lines := strings.Split(strings.TrimSpace(output.Text), "\n")
	placeholder := false
	if len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		placeholder = last == "无" || last == "无需补充"
	}
	t.Logf("schedule event %d tool_calls=%d pending_questions=%q trailing_placeholder=%t active_memories=%d memory_history=%d",
		event.ID, len(entries), pendingQuestions, placeholder, len(memories), len(history))
	if len(pendingQuestions) > 0 {
		t.Fatalf("semantic FAIL: schedule event %d retained unnecessary pending questions: questions=%q output=%+v active_memories=%+v memory_history=%+v", event.ID, pendingQuestions, output, memories, history)
	}
	if placeholder || strings.ContainsAny(output.Text, "?？") {
		t.Fatalf("semantic FAIL: schedule event %d ended with a question or placeholder without a pending-question record; output=%+v active_memories=%+v memory_history=%+v", event.ID, output, memories, history)
	}
}

func replaySetUserQuestions(t *testing.T, entries []observer.LoggedEntry) []string {
	t.Helper()
	var pending []string
	for _, entry := range entries {
		if entry.Message != "tool.finished" {
			continue
		}
		fields := entry.ContextMap()
		if fields["tool_name"] != "set_user_questions" || fields["outcome"] != "success" {
			continue
		}
		payload, err := json.Marshal(fields["payload"])
		if err != nil {
			t.Fatalf("encode set_user_questions trace: %v", err)
		}
		var trace struct {
			Input struct {
				ArgumentsInJSON string `json:"ArgumentsInJSON"`
			} `json:"input"`
		}
		if err := json.Unmarshal(payload, &trace); err != nil {
			t.Fatalf("decode set_user_questions trace payload %s: %v", payload, err)
		}
		var args struct {
			Questions []string `json:"questions"`
		}
		if err := json.Unmarshal([]byte(trace.Input.ArgumentsInJSON), &args); err != nil {
			t.Fatalf("decode set_user_questions arguments %q: %v", trace.Input.ArgumentsInJSON, err)
		}
		if args.Questions != nil {
			pending = append([]string(nil), args.Questions...)
		}
	}
	return pending
}

func TestLivePersonalSchedulePartialFailureAndStaleSuggestion(t *testing.T) {
	main := livePersonalAgent(t)
	main.MaxIterations = 8
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	store := testkit.Store(t, filepath.Join(t.TempDir(), "schedule-callback.db"), 10*time.Minute)
	main.Store = store
	main.Timezone = "UTC"

	createEvent := testkit.OpenUserEvent(t, store, 511, 611, "固定虚构计划背景和白桦项目状态来源")
	schedule, _, err := store.CreateSchedule(ctx, createEvent.ID, "live-seed-schedule", personal.ScheduleInput{
		Title:   "白桦状态检查",
		Goal:    "检查白桦项目状态来源的新增更新。若必需来源不完整且没有历史读取能力，结束本次为失败，不要宣称检查完整。",
		Context: "虚构项目白桦当前使用 Go。发布页没有历史存档；本次无法读取时不得盲目重试，计划任务继续按这份固定背景执行。",
		Cron:    "0 * * * *", Timezone: "UTC", WindowMode: personal.WindowSinceSuccess,
		RequiredSources: []personal.SourceRequirement{
			{ID: "status", Label: "白桦状态页", URL: "https://status.example.invalid/baihua"},
			{ID: "release", Label: "白桦发布页", URL: "https://releases.example.invalid/baihua"},
		},
	}, time.Date(2026, 11, 4, 7, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, createEvent, personal.TurnOutput{ScheduleID: schedule.ID}, nil)

	memoryEvent := testkit.OpenUserEvent(t, store, 512, 612, "虚构项目白桦已经迁移到 Rust；请把这条事实作为当前记忆。")
	memory, err := store.WriteMemory(ctx, memoryEvent.ID, "live-seed-memory", personal.MemoryChange{
		Operation: "put", Key: "fiction.baihua.language", Value: "白桦项目当前使用 Rust。", Evidence: memoryEvent.Text,
	})
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, memoryEvent, personal.TurnOutput{Text: "记忆已保存。"}, []string{"记忆已保存。"})

	occurrence, run := testkit.DispatchRun(t, store, *schedule.NextRunAt)
	started := occurrence.ScheduledAt.Add(5 * time.Minute)
	if _, err := store.BeginScheduledRun(ctx, run.ID, started); err != nil {
		t.Fatal(err)
	}
	statusRef, err := store.SaveSource(ctx, run.ID, "status-source", domain.Source{
		URL: "https://status.example.invalid/baihua", Status: "ok", Content: "No new status entry.", FetchedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	releaseRef, err := store.SaveSource(ctx, run.ID, "release-source", domain.Source{
		URL: "https://releases.example.invalid/baihua", Status: "failed", Reason: "fictional source unavailable", FetchedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishStage(ctx, run.ID, personal.StageResult{
		Summary: "白桦状态页已完整检查；白桦发布页访问失败，尚不能确认整个检查区间。",
		Sources: []domain.SourceRef{statusRef, releaseRef},
	}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := store.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask || callback.TaskID != run.TaskID {
		t.Fatalf("ClaimEvent() = (%+v, %v), want scheduled callback", callback, err)
	}
	main.Store = &scheduleSuggestionTraceStore{Store: store, t: t}
	output := livePersonalThink(t, ctx, main, livePersonalInput(t, ctx, store, *callback))
	if output.Silent {
		t.Fatal("semantic FAIL: failed check and background candidate must be included in a user notification")
	}
	testkit.FinishEvent(t, store, *callback, output, nil)
	updated, err := store.Schedule(ctx, schedule.ID)
	task, taskErr := store.Task(ctx, run.TaskID)
	if err != nil || taskErr != nil || updated == nil || updated.CoveredThrough != nil || task == nil || task.Status != personal.TaskFailed {
		t.Fatalf("semantic FAIL: partial source failure credited or completed the check (schedule=%+v task=%+v errors=%v/%v)", updated, task, err, taskErr)
	}
	suggestions, err := store.ScheduleSuggestions(ctx, schedule.ID)
	if err != nil || len(suggestions) != 1 {
		t.Fatalf("semantic FAIL: callback did not record one background candidate (suggestions=%+v err=%v)", suggestions, err)
	}
	suggestion := suggestions[0]
	if suggestion.Status != "pending" || suggestion.TaskID != run.TaskID || suggestion.NotifiedEventID != callback.ID || len(suggestion.Replacements) == 0 || !strings.Contains(suggestion.OldContext, "Go") || !strings.Contains(suggestion.NewContext, "Rust") {
		t.Fatalf("semantic FAIL: candidate did not preserve the fixed/current background difference: %+v", suggestion)
	}

	correction := testkit.OpenUserEvent(t, store, 513, 613, "更正：白桦项目现在使用 Kotlin，不再使用 Rust。")
	if _, err := store.WriteMemory(ctx, correction.ID, "live-correct-memory", personal.MemoryChange{
		Operation: "put", Key: memory.Key, Value: "白桦项目当前使用 Kotlin。", ExpectedVersion: memory.Version, Evidence: correction.Text,
	}); err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, correction, personal.TurnOutput{Text: "记忆已更正。"}, []string{"记忆已更正。"})

	confirm := testkit.OpenUserEvent(t, store, 514, 614, "请应用计划 #"+strconv.FormatInt(schedule.ID, 10)+" 的建议 #"+strconv.FormatInt(suggestion.ID, 10)+"（版本 "+strconv.FormatInt(suggestion.Revision, 10)+"），我确认同步。")
	output = livePersonalThink(t, ctx, main, livePersonalInput(t, ctx, store, confirm))
	testkit.FinishEvent(t, store, confirm, output, nil)
	main.Store = store
	updated, err = store.Schedule(ctx, schedule.ID)
	if err != nil || updated == nil || !strings.Contains(updated.Context, "当前使用 Go") {
		t.Fatalf("semantic FAIL: stale confirmation changed the fixed plan context: schedule=%+v err=%v", updated, err)
	}
	suggestions, err = store.ScheduleSuggestions(ctx, schedule.ID)
	if err != nil || len(suggestions) != 1 || suggestions[0].Status != "pending" || suggestions[0].ID == suggestion.ID || !strings.Contains(suggestions[0].NewContext, "Kotlin") {
		t.Fatalf("stale suggestion should remain pending for a refreshed proposal: suggestions=%+v err=%v", suggestions, err)
	}
	snapshot, err := store.Diagnostic(ctx, confirm.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var notification strings.Builder
	for _, delivery := range snapshot.Deliveries {
		if delivery.EventID == confirm.ID {
			notification.WriteString(delivery.Text)
		}
	}
	shown := notification.String()
	for _, replacement := range suggestions[0].Replacements {
		if !strings.Contains(shown, "原背景："+replacement.Old) || !strings.Contains(shown, "建议改为："+replacement.New) {
			t.Fatalf("refreshed confirmation did not show the stored replacement: %q", shown)
		}
	}
	t.Log("mechanism PASS: seeded plan, failed-source occurrence, and pending suggestion survived SQLite; semantic PASS: partial check was not credited and stale approval left fixed context unchanged")
}

type scheduleSuggestionTraceStore struct {
	*sqlite.Store
	t *testing.T
}

func (store *scheduleSuggestionTraceStore) ProposeScheduleContextUpdate(ctx context.Context, eventID int64, key string, taskID int64, proposal personal.ScheduleContextProposal, now time.Time) (personal.ScheduleSuggestion, bool, error) {
	store.t.Logf("real-model proposal revision=%d", proposal.ScheduleRevision)
	for _, ref := range proposal.MemoryRefs {
		store.t.Logf("real-model proposal memory key=%s version=%d", ref.Key, ref.Version)
	}
	for _, replacement := range proposal.Replacements {
		store.t.Logf("real-model proposal replacement old=%q new=%q key=%s", replacement.Old, replacement.New, replacement.MemoryKey)
	}
	suggestion, replayed, err := store.Store.ProposeScheduleContextUpdate(ctx, eventID, key, taskID, proposal, now)
	if err != nil {
		store.t.Logf("Store.ProposeScheduleContextUpdate error=%q", err.Error())
		return suggestion, replayed, err
	}
	store.t.Logf("Store.ProposeScheduleContextUpdate status=%s replayed=%t", suggestion.Status, replayed)
	return suggestion, replayed, nil
}

func (store *scheduleSuggestionTraceStore) ResolveScheduleContextUpdate(ctx context.Context, eventID int64, key string, suggestionID, expectedRevision int64, decision string, now time.Time) (personal.ScheduleSuggestionResolution, bool, error) {
	store.t.Logf("real-model resolve suggestion=%d expected_revision=%d decision=%s", suggestionID, expectedRevision, decision)
	resolution, replayed, err := store.Store.ResolveScheduleContextUpdate(ctx, eventID, key, suggestionID, expectedRevision, decision, now)
	if err != nil {
		store.t.Logf("Store.ResolveScheduleContextUpdate error=%q", err.Error())
		return resolution, replayed, err
	}
	store.t.Logf("Store.ResolveScheduleContextUpdate status=%s needs_proposal=%t replayed=%t", resolution.Status, resolution.NeedsProposal, replayed)
	return resolution, replayed, nil
}
