package personal

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kagari/internal/channel"
)

func TestScheduledRunCancelledWhileWaitingForTaskLockNeverStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s := testStore(t, ":memory:", time.Minute)
	p := dispatchTestSchedule(t, s, WindowNone)
	_, claimed := dispatchTestRun(t, s, *p.NextRunAt)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	var executed atomic.Bool
	r.Execute = func(context.Context, Run, func(string) error) (StageResult, error) {
		executed.Store(true)
		return StageResult{Summary: "unexpected"}, nil
	}
	release, err := r.acquireTask(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.task(ctx); done <- err }()
	eventually(t, func() bool { run, err := s.Run(ctx, claimed.ID); return err == nil && run.Status == RunRunning })
	e := openUserEvent(t, s, 2, 2, "Cancel the plan")
	if _, _, err := s.CancelSchedule(ctx, e.ID, "cancel", p.ID, p.Revision, time.Now()); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if executed.Load() {
		t.Fatal("worker entered execution after pre-start cancellation")
	}
	info, err := s.ScheduleForTask(ctx, claimed.TaskID)
	if err != nil || info.Occurrence.StartedAt != nil {
		t.Fatalf("cancelled work got a start marker: %+v %v", info, err)
	}
}

func TestScheduledAttachmentFallbackKeepsAttributionAndWindow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	p := dispatchTestSchedule(t, s, WindowSinceSuccess)
	_, run := dispatchTestRun(t, s, *p.NextRunAt)
	started := p.NextRunAt.Add(time.Minute)
	if _, err := s.BeginScheduledRun(ctx, run.ID, started); err != nil {
		t.Fatal(err)
	}
	callback := dispatchTestFinish(t, s, run)
	file, err := s.SaveAttachment(ctx, "result.txt", "text/plain", strings.NewReader("partial result"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAttachment(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleTask(ctx, callback.ID, run.TaskID, run.Revision, TaskFailed, "Source failed"); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	if err := r.publish(ctx, callback, TurnOutput{TaskID: run.TaskID, Attachments: []string{file.ID}}); err != nil {
		t.Fatal(err)
	}
	delivery, err := s.ClaimDelivery(ctx)
	if err != nil || delivery == nil {
		t.Fatalf("fallback delivery=%+v %v", delivery, err)
	}
	for _, want := range []string{fmt.Sprintf("计划 #%d · Task #%d", p.ID, run.TaskID), p.BaselineAt.Format(time.RFC3339), started.Format(time.RFC3339), "完整检查：false", "附件已删除或不可用"} {
		if !strings.Contains(delivery.Text, want) {
			t.Fatalf("fallback lost %q: %q", want, delivery.Text)
		}
	}
}

func TestScheduleSuggestionReplyUsesTheExactDeliveredPart(t *testing.T) {
	ctx := context.Background()
	s, plan, memory := scheduleSuggestionFixture(t, WindowNone)
	callback := scheduleSuggestionCallback(t, s, plan.ID)
	suggestion, _, err := s.ProposeScheduleContextUpdate(ctx, callback.ID, "proposal", callback.TaskID, scheduleSuggestionProposal(plan, memory), scheduleTestNow())
	if err != nil {
		t.Fatal(err)
	}
	finishScheduleSuggestionCallback(t, s, callback, false)
	var resultMessage, suggestionMessage string
	var suggestionOutboxID int64
	for {
		delivery, err := s.ClaimDelivery(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if delivery == nil {
			break
		}
		messageID := fmt.Sprintf("sent-%d", delivery.ID)
		if err := s.UpdateDelivery(ctx, delivery.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: messageID}, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if delivery.EventID == callback.ID {
			if strings.Contains(delivery.Text, fmt.Sprintf("建议 #%d", suggestion.ID)) {
				suggestionMessage, suggestionOutboxID = messageID, delivery.ID
			} else {
				resultMessage = messageID
			}
		}
	}
	if resultMessage == "" || suggestionMessage == "" {
		t.Fatal("expected separate result and suggestion deliveries")
	}
	for i, messageID := range []string{resultMessage, suggestionMessage} {
		input := sourceInput(fmt.Sprintf("reply-%d", i), "同步")
		input.ReplyToID = messageID
		acceptInput(t, s, fmt.Sprintf("reply-cursor-%d", i), input)
		event, err := s.ClaimEvent(ctx)
		if err != nil || event == nil {
			t.Fatalf("reply event=%+v %v", event, err)
		}
		if event.TaskID != callback.TaskID || event.ScheduleID != plan.ID {
			t.Fatalf("reply attribution=%+v", event)
		}
		turn, err := testRuntime(s).context(ctx, *event)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && len(turn.ReplySuggestions) != 0 {
			t.Fatal("reply to the result guessed a suggestion from the same event")
		}
		if i == 1 && (event.ReplyOutboxID != suggestionOutboxID || len(turn.ReplySuggestions) != 1 || turn.ReplySuggestions[0].ID != suggestion.ID) {
			t.Fatalf("exact suggestion association lost: event=%+v suggestions=%+v", event, turn.ReplySuggestions)
		}
		finishEvent(t, s, *event, TurnOutput{Text: "Reply inspected."}, nil)
	}
}

func TestRevisedScheduledTaskLabelsItsWindowAsHistorical(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	p := dispatchTestSchedule(t, s, WindowSinceSuccess)
	_, run := dispatchTestRun(t, s, *p.NextRunAt)
	if _, err := s.BeginScheduledRun(ctx, run.ID, p.NextRunAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	callback := dispatchTestFinish(t, s, run)
	check := &CheckAssessment{Complete: true, RequiredSources: []SourceAssessment{{ID: "feed", Complete: true, Evidence: "No updates in the full interval"}}}
	if _, err := s.SettleTask(ctx, callback.ID, run.TaskID, run.Revision, TaskCompleted, "No new updates", check); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, callback, TurnOutput{TaskID: run.TaskID}, nil)
	edit := openUserEvent(t, s, 2, 2, "Change this task's goal")
	if _, _, err := s.ReviseTask(ctx, edit.ID, "revise", run.TaskID, run.Revision, TaskInput{Goal: "Draft an exercise instead"}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, edit, TurnOutput{TaskID: run.TaskID, Silent: true}, nil)
	revised, err := s.ClaimRun(ctx, RunTask)
	if err != nil || revised == nil {
		t.Fatalf("claim revision=%+v %v", revised, err)
	}
	if _, err := s.BeginScheduledRun(ctx, revised.ID, p.NextRunAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	callback = dispatchTestFinish(t, s, *revised)
	if _, err := s.SettleTask(ctx, callback.ID, revised.TaskID, revised.Revision, TaskCompleted, "Exercise drafted"); err != nil {
		t.Fatal(err)
	}
	if err := testRuntime(s).publish(ctx, callback, TurnOutput{TaskID: revised.TaskID, Text: "Exercise drafted"}); err != nil {
		t.Fatal(err)
	}
	var text string
	if err := s.db.QueryRow(`SELECT text FROM personal_outbox WHERE event_id=? ORDER BY part LIMIT 1`, callback.ID).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "原检查区间") || !strings.Contains(text, "不计入计划检查进度") || strings.Contains(text, "完整检查：true") {
		t.Fatalf("revised task reported historical credit as its own: %q", text)
	}
}

func TestScheduledWindowSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedule.db")
	s := testStore(t, path, time.Minute)
	p := dispatchTestSchedule(t, s, WindowSinceSuccess)
	_, claimed := dispatchTestRun(t, s, *p.NextRunAt)
	start := p.NextRunAt.Add(2 * time.Hour)
	first, err := s.BeginScheduledRun(ctx, claimed.ID, start)
	if err != nil {
		t.Fatal(err)
	}
	before, err := decodeTaskInput(first.Input)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := testStore(t, path, time.Minute)
	if err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := reopened.ClaimRun(ctx, RunTask)
	if err != nil || run == nil {
		t.Fatalf("claim=%+v %v", run, err)
	}
	after, err := reopened.BeginScheduledRun(ctx, run.ID, start.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	input, err := decodeTaskInput(after.Input)
	if err != nil {
		t.Fatal(err)
	}
	if *input.Trigger.StartedAt != *before.Trigger.StartedAt || *input.Trigger.Window != *before.Trigger.Window {
		t.Fatalf("reopen moved window: %+v -> %+v", before.Trigger, input.Trigger)
	}
}

func TestRecoverCommittedScheduleDoesNotRepeatModelOrRegistration(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	e := openUserEvent(t, s, 1, 1, "Register a plan")
	p, _, err := s.CreateSchedule(ctx, e.ID, "committed", ScheduleInput{Title: "plan", Goal: "exercise", Cron: "0 * * * *", Timezone: "UTC"}, dispatchTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r := testRuntime(s)
	r.Think = func(context.Context, TurnInput) (TurnOutput, error) {
		t.Error("interrupted user model replayed")
		return TurnOutput{}, nil
	}
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("recover=%t %v", worked, err)
	}
	plans, err := s.Schedules(ctx, true, 0, 20)
	if err != nil || len(plans) != 1 || plans[0].ID != p.ID {
		t.Fatalf("registration repeated=%+v %v", plans, err)
	}
	if o, err := s.DispatchDueSchedule(ctx, *p.NextRunAt); err != nil || o == nil || o.Outcome != "enqueued" {
		t.Fatalf("recovered authorization not released=%+v %v", o, err)
	}
}
