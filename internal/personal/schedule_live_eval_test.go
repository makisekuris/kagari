package personal

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
)

func TestLivePersonalScheduleRegistration(t *testing.T) {
	main := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	store := testStore(t, filepath.Join(t.TempDir(), "schedules.db"), 10*time.Minute)
	main.Store = store
	main.Timezone = "UTC"

	ordinary := openUserEvent(t, store, 501, 601,
		"请直接登记一个普通定时目标：每天 09:00 UTC，为虚构项目白桦生成一道 Go 练习题。每次独立生成，不持续检查新信息，也不需要固定来源或补查历史。")
	ordinaryOutput := livePersonalThink(t, ctx, main, livePersonalInput(t, ctx, store, ordinary))
	ordinarySchedule, err := store.Schedule(ctx, ordinaryOutput.ScheduleID)
	if err != nil || ordinarySchedule == nil {
		t.Fatalf("registered ordinary schedule lookup = (%+v, %v)", ordinarySchedule, err)
	}
	if ordinaryOutput.Silent {
		t.Fatal("registration receipt must be visible")
	}
	finishEvent(t, store, ordinary, ordinaryOutput, nil)

	continuous := openUserEvent(t, store, 502, 602,
		"请直接登记一个连续信息检查计划：每周一 10:30 UTC 检查虚构项目白桦的新增状态更新，来源是 https://status.example.invalid/baihua。检查从上次成功检查后开始，必须确认该来源完整读取；如果来源失败就报告不完整。")
	continuousOutput := livePersonalThink(t, ctx, main, livePersonalInput(t, ctx, store, continuous))
	continuousSchedule, err := store.Schedule(ctx, continuousOutput.ScheduleID)
	if err != nil || continuousSchedule == nil {
		t.Fatalf("registered continuous schedule lookup = (%+v, %v)", continuousSchedule, err)
	}
	if continuousOutput.Silent {
		t.Fatal("registration receipt must be visible")
	}
	finishEvent(t, store, continuous, continuousOutput, nil)

	schedules, err := store.Schedules(ctx, false, 0, 20)
	if err != nil || len(schedules) != 2 {
		t.Fatalf("semantic FAIL: expected two fictional schedules, got %d (%s)", len(schedules), err)
	}
	if ordinarySchedule == nil || ordinarySchedule.WindowMode != WindowNone || ordinarySchedule.Timezone != "UTC" || ordinarySchedule.Cron != "0 9 * * *" || ordinarySchedule.NextRunAt == nil {
		t.Fatalf("semantic FAIL: ordinary goal was not stored as a normal schedule: %+v", ordinarySchedule)
	}
	if continuousSchedule == nil || continuousSchedule.Cron != "30 10 * * 1" || continuousSchedule.WindowMode != WindowSinceSuccess || continuousSchedule.Timezone != "UTC" || continuousSchedule.BaselineAt == nil || len(continuousSchedule.RequiredSources) != 1 || continuousSchedule.RequiredSources[0].URL != "https://status.example.invalid/baihua" {
		t.Fatalf("semantic FAIL: continuous check did not retain its coverage source: %+v", continuousSchedule)
	}
	t.Log("mechanism PASS: two real SQLite plans persisted; semantic PASS: ordinary generation and since-success source check kept distinct modes")
}

func TestLivePersonalSchedulePartialFailureAndStaleSuggestion(t *testing.T) {
	main := livePersonalAgent(t)
	main.MaxIterations = 8
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	store := testStore(t, filepath.Join(t.TempDir(), "schedule-callback.db"), 10*time.Minute)
	main.Store = store
	main.Timezone = "UTC"

	createEvent := openUserEvent(t, store, 511, 611, "固定虚构计划背景和白桦项目状态来源")
	schedule, _, err := store.CreateSchedule(ctx, createEvent.ID, "live-seed-schedule", ScheduleInput{
		Title:   "白桦状态检查",
		Goal:    "检查白桦项目状态来源的新增更新。若必需来源不完整且没有历史读取能力，结束本次为失败，不要宣称检查完整。",
		Context: "虚构项目白桦当前使用 Go。发布页没有历史存档；本次无法读取时不得盲目重试，计划任务继续按这份固定背景执行。",
		Cron:    "0 * * * *", Timezone: "UTC", WindowMode: WindowSinceSuccess,
		RequiredSources: []SourceRequirement{
			{ID: "status", Label: "白桦状态页", URL: "https://status.example.invalid/baihua"},
			{ID: "release", Label: "白桦发布页", URL: "https://releases.example.invalid/baihua"},
		},
	}, dispatchTestNow)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, store, createEvent, TurnOutput{ScheduleID: schedule.ID}, nil)

	memoryEvent := openUserEvent(t, store, 512, 612, "虚构项目白桦已经迁移到 Rust；请把这条事实作为当前记忆。")
	memory, err := store.WriteMemory(ctx, memoryEvent.ID, "live-seed-memory", MemoryChange{
		Operation: "put", Key: "fiction.baihua.language", Value: "白桦项目当前使用 Rust。", Evidence: memoryEvent.Text,
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, store, memoryEvent, TurnOutput{Text: "记忆已保存。"}, []string{"记忆已保存。"})

	occurrence, run := dispatchTestRun(t, store, *schedule.NextRunAt)
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
	if err := store.FinishStage(ctx, run.ID, StageResult{
		Summary: "白桦状态页已完整检查；白桦发布页访问失败，尚不能确认整个检查区间。",
		Sources: []domain.SourceRef{statusRef, releaseRef},
	}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := store.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask || callback.TaskID != run.TaskID {
		t.Fatalf("ClaimEvent() = (%+v, %v), want scheduled callback", callback, err)
	}
	main.Store = &scheduleSuggestionTraceStore{Store: store, t: t}
	output := livePersonalThink(t, ctx, main, livePersonalInput(t, ctx, store, *callback))
	if output.Silent {
		t.Fatal("semantic FAIL: failed check and background candidate must be included in a user notification")
	}
	finishEvent(t, store, *callback, output, nil)
	updated, err := store.Schedule(ctx, schedule.ID)
	task, taskErr := store.Task(ctx, run.TaskID)
	if err != nil || taskErr != nil || updated == nil || updated.CoveredThrough != nil || task == nil || task.Status != TaskFailed {
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

	correction := openUserEvent(t, store, 513, 613, "更正：白桦项目现在使用 Kotlin，不再使用 Rust。")
	if _, err := store.WriteMemory(ctx, correction.ID, "live-correct-memory", MemoryChange{
		Operation: "put", Key: memory.Key, Value: "白桦项目当前使用 Kotlin。", ExpectedVersion: memory.Version, Evidence: correction.Text,
	}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, store, correction, TurnOutput{Text: "记忆已更正。"}, []string{"记忆已更正。"})

	confirm := openUserEvent(t, store, 514, 614, "请应用计划 #"+strconv.FormatInt(schedule.ID, 10)+" 的建议 #"+strconv.FormatInt(suggestion.ID, 10)+"（版本 "+strconv.FormatInt(suggestion.Revision, 10)+"），我确认同步。")
	output = livePersonalThink(t, ctx, main, livePersonalInput(t, ctx, store, confirm))
	finishEvent(t, store, confirm, output, nil)
	main.Store = store
	updated, err = store.Schedule(ctx, schedule.ID)
	if err != nil || updated == nil || !strings.Contains(updated.Context, "当前使用 Go") {
		t.Fatalf("semantic FAIL: stale confirmation changed the fixed plan context: schedule=%+v err=%v", updated, err)
	}
	var resolutionRequests int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_requests WHERE event_id=? AND operation='resolve_schedule_context_update'`, confirm.ID).Scan(&resolutionRequests); err != nil || resolutionRequests == 0 {
		t.Fatalf("mechanism FAIL: confirmation did not reach the resolution tool (%d, %v)", resolutionRequests, err)
	}
	suggestions, err = store.ScheduleSuggestions(ctx, schedule.ID)
	if err != nil || len(suggestions) != 1 || suggestions[0].Status != "pending" || suggestions[0].ID == suggestion.ID || !strings.Contains(suggestions[0].NewContext, "Kotlin") {
		t.Fatalf("stale suggestion should remain pending for a refreshed proposal: suggestions=%+v err=%v", suggestions, err)
	}
	var shown string
	if err := store.db.QueryRowContext(ctx, `SELECT text FROM personal_outbox WHERE event_id=? AND suggestion_id=?`, confirm.ID, suggestions[0].ID).Scan(&shown); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range suggestions[0].Replacements {
		if !strings.Contains(shown, "原背景："+replacement.Old) || !strings.Contains(shown, "建议改为："+replacement.New) {
			t.Fatalf("refreshed confirmation did not show the stored replacement: %q", shown)
		}
	}
	t.Log("mechanism PASS: seeded plan, failed-source occurrence, and pending suggestion survived SQLite; semantic PASS: partial check was not credited and stale approval left fixed context unchanged")
}

type scheduleSuggestionTraceStore struct {
	*Store
	t *testing.T
}

func (store *scheduleSuggestionTraceStore) ProposeScheduleContextUpdate(ctx context.Context, eventID int64, key string, taskID int64, proposal ScheduleContextProposal, now time.Time) (ScheduleSuggestion, bool, error) {
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

func (store *scheduleSuggestionTraceStore) ResolveScheduleContextUpdate(ctx context.Context, eventID int64, key string, suggestionID, expectedRevision int64, decision string, now time.Time) (ScheduleSuggestionResolution, bool, error) {
	store.t.Logf("real-model resolve suggestion=%d expected_revision=%d decision=%s", suggestionID, expectedRevision, decision)
	resolution, replayed, err := store.Store.ResolveScheduleContextUpdate(ctx, eventID, key, suggestionID, expectedRevision, decision, now)
	if err != nil {
		store.t.Logf("Store.ResolveScheduleContextUpdate error=%q", err.Error())
		return resolution, replayed, err
	}
	store.t.Logf("Store.ResolveScheduleContextUpdate status=%s needs_proposal=%t replayed=%t", resolution.Status, resolution.NeedsProposal, replayed)
	return resolution, replayed, nil
}
