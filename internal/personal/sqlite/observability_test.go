package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"kagari/internal/channel"
	"kagari/internal/logging"
	"kagari/internal/personal"
)

func observedStoreContext() (context.Context, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.InfoLevel)
	return logging.WithLogger(context.Background(), zap.New(core)), logs
}

func assertStoreOutcome(t *testing.T, logs *observer.ObservedLogs, index int, operation, outcome string) map[string]any {
	t.Helper()
	entries := logs.All()
	if index >= len(entries) {
		t.Fatalf("log entry %d missing; have %d", index, len(entries))
	}
	entry := entries[index]
	fields := normalizedFields(t, entry)
	if entry.Message != "store.outcome" || fields["operation"] != operation || fields["outcome"] != outcome {
		t.Fatalf("store outcome log = (%s,%v), want operation=%s outcome=%s", entry.Message, fields, operation, outcome)
	}
	return fields
}

func normalizedFields(t *testing.T, entry observer.LoggedEntry) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(entry.ContextMap())
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestStoreTaskOutcomeLogsReplayNoopAndStaleStage(t *testing.T) {
	ctx, logs := observedStoreContext()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1901, "Create and cancel a report task")
	input := personal.TaskInput{Goal: "Prepare a report", Context: "Use the saved context"}
	task, run, err := s.CreateTask(ctx, event.ID, "create", "Report", input)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateTask(ctx, event.ID, "create", "Report", input); err != nil {
		t.Fatal(err)
	}
	assertStoreOutcome(t, logs, 0, "task.create", "created")
	if fields := assertStoreOutcome(t, logs, 1, "task.create", "replayed"); fields["payload"] == nil {
		t.Fatal("task replay omitted its returned payload")
	}

	finishEvent(t, s, event, personal.TurnOutput{}, nil)
	claimed, err := s.ClaimRun(context.Background(), personal.RunTask)
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim task run = (%+v,%v)", claimed, err)
	}
	// Force a revision race while leaving the claimed run active, as if a newer request won first.
	if _, err := s.db.Exec(`UPDATE personal_tasks SET revision=revision+1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, run.ID, personal.StageResult{Summary: "stale result"}, ""); err != nil {
		t.Fatal(err)
	}
	fields := assertStoreOutcome(t, logs, 2, "task.stage.finish", "discarded_stale")
	if fields["run_status"] != string(personal.RunCancelled) || fields["task_status"] != string(personal.TaskRunning) {
		t.Fatalf("stale stage reported wrong saved status: %v", fields)
	}

	cancelEvent := openUserEvent(t, s, 2, 1902, "Cancel the report task")
	if _, err := s.CancelTask(ctx, cancelEvent.ID, task.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelTask(ctx, cancelEvent.ID, task.ID, 2); err != nil {
		t.Fatal(err)
	}
	assertStoreOutcome(t, logs, 3, "task.cancel", "cancelled")
	fields = assertStoreOutcome(t, logs, 4, "task.cancel", "unchanged")
	if payload, ok := fields["payload"].(map[string]any); !ok || payload["task"] == nil {
		t.Fatalf("cancel no-op omitted current task payload: %v", fields["payload"])
	}
}

func TestStoreMemoryOutcomeLogsReplayConflictAndReviewCounts(t *testing.T) {
	ctx, logs := observedStoreContext()
	s := testStore(t, ":memory:", time.Minute)
	seed := openUserEvent(t, s, 1, 1903, "I prefer tea")
	change := personal.MemoryChange{Operation: "put", Key: "drink", Value: "tea", Evidence: seed.Text}
	if _, err := s.WriteMemory(ctx, seed.ID, "put", change); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteMemory(ctx, seed.ID, "put", change); err != nil {
		t.Fatal(err)
	}
	assertStoreOutcome(t, logs, 0, "memory.write", "applied")
	assertStoreOutcome(t, logs, 1, "memory.write", "replayed")
	if _, err := s.WriteMemory(ctx, seed.ID, "stale", change); !errors.Is(err, errMemoryVersionConflict) {
		t.Fatalf("stale write error = %v", err)
	}
	if logs.Len() != 2 {
		t.Fatalf("conflicting write was logged as applied: %v", logs.All())
	}

	finishEvent(t, s, seed, personal.TurnOutput{Silent: true}, nil)
	firstReview, err := s.ClaimRun(context.Background(), personal.RunMemoryReview)
	if err != nil || firstReview == nil {
		t.Fatalf("claim seed review = (%+v,%v)", firstReview, err)
	}
	if err := s.FinishMemoryReview(ctx, firstReview.ID, personal.MemoryReview{}, ""); err != nil {
		t.Fatal(err)
	}

	newer := openUserEvent(t, s, 2, 1904, "I still prefer tea")
	finishEvent(t, s, newer, personal.TurnOutput{Silent: true}, nil)
	review, err := s.ClaimRun(context.Background(), personal.RunMemoryReview)
	if err != nil || review == nil {
		t.Fatalf("claim newer review = (%+v,%v)", review, err)
	}
	reviewChange := personal.MemoryChange{Operation: "put", Key: "drink", Value: "coffee", Evidence: newer.Text}
	if err := s.FinishMemoryReview(ctx, review.ID, personal.MemoryReview{Changes: []personal.MemoryChange{reviewChange}}, ""); err != nil {
		t.Fatal(err)
	}
	fields := assertStoreOutcome(t, logs, 3, "memory.review.finish", "completed")
	if fields["proposed_count"] != json.Number("1") || fields["applied_count"] != json.Number("0") || fields["skipped_count"] != json.Number("1") || fields["conflict_count"] != json.Number("1") {
		t.Fatalf("conflicting review counts = %v", fields)
	}
	if fields["run_status"] != string(personal.RunCompleted) {
		t.Fatalf("review outcome misreported persisted run state: %v", fields)
	}

	failedSource := openUserEvent(t, s, 3, 1906, "Remember a preference")
	finishEvent(t, s, failedSource, personal.TurnOutput{Silent: true}, nil)
	failedRun, err := s.ClaimRun(context.Background(), personal.RunMemoryReview)
	if err != nil || failedRun == nil {
		t.Fatalf("claim failed review = (%+v,%v)", failedRun, err)
	}
	invalid := personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "BadKey", Value: "brief", Evidence: failedSource.Text}}}
	if err := s.FinishMemoryReview(ctx, failedRun.ID, invalid, ""); err != nil {
		t.Fatalf("persist failed review: %v", err)
	}
	fields = assertStoreOutcome(t, logs, 4, "memory.review.finish", "failed")
	if fields["run_status"] != string(personal.RunFailed) || fields["applied_count"] != json.Number("0") || fields["skipped_count"] != json.Number("1") {
		t.Fatalf("failed review outcome = %v", fields)
	}
}

func TestStoreCommitFailureDoesNotLogSuccess(t *testing.T) {
	ctx, logs := observedStoreContext()
	errCommit := errors.New("commit failed")
	err := commitStoreOutcome(ctx, failingCommit{err: errCommit}, "task.create", "created", personal.Task{}, zap.Int64("task_id", 1))
	if !errors.Is(err, errCommit) || logs.Len() != 0 {
		t.Fatalf("commit failure result = %v, logs = %v", err, logs.All())
	}
}

type failingCommit struct{ err error }

func (f failingCommit) Commit() error { return f.err }

func TestStoreDeliveryAndRecoveryLogCommittedState(t *testing.T) {
	ctx, logs := observedStoreContext()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1905, "Send a result")
	if err := s.CompleteEvent(ctx, event.ID, personal.TurnOutput{Text: "result"}, []string{"result"}); err != nil {
		t.Fatal(err)
	}
	fields := assertStoreOutcome(t, logs, 0, "event.complete", "completed")
	if fields["payload"] != nil || fields["reply_part_count"] != json.Number("1") {
		t.Fatalf("event completion log duplicated the reply body or omitted metadata: %v", fields)
	}
	delivery, err := s.ClaimDelivery(context.Background())
	if err != nil || delivery == nil {
		t.Fatalf("claim delivery = (%+v,%v)", delivery, err)
	}
	if err := s.UpdateDelivery(ctx, delivery.ID, "uncertain", channel.Receipt{}, "remote result unknown", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDelivery(ctx, delivery.ID, "uncertain", channel.Receipt{}, "ignored retry detail", time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertStoreOutcome(t, logs, 2, "delivery.update", "saved")
	fields = assertStoreOutcome(t, logs, 3, "delivery.update", "unchanged")
	if fields["delivery_id"] == nil || fields["outbox_id"] != nil || fields["status"] != "uncertain" || fields["delivery_status"] != nil {
		t.Fatalf("delivery fields do not match runtime/CLI names: %v", fields)
	}
	if payload, ok := fields["payload"].(map[string]any); !ok || payload["detail"] != "remote result unknown" {
		t.Fatalf("idempotent delivery log did not report persisted state: %v", fields["payload"])
	}
	queuedEntries := 0
	for _, entry := range logs.All() {
		if entry.Message == "conversation.reply_queued" {
			queuedEntries++
			queued := normalizedFields(t, entry)
			if queued["parts"] != json.Number("1") || queued["payload"] != "result" || queued["event_id"] == nil || queued["task_id"] == nil || queued["run_id"] == nil {
				t.Fatalf("queued reply log = %v", queued)
			}
		}
	}
	if queuedEntries != 1 {
		t.Fatalf("queued reply log count = %d, want 1", queuedEntries)
	}

	if _, err := s.db.Exec(`UPDATE personal_events SET status='running' WHERE id=?`, event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE personal_outbox SET status='sending' WHERE id=?`, delivery.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	fields = assertStoreOutcome(t, logs, 4, "store.recover", "recovered")
	if fields["requeued_events"] != json.Number("1") || fields["uncertain_sends"] != json.Number("1") {
		t.Fatalf("recovery counts = %v", fields)
	}
	var eventStatus, deliveryStatus string
	if err := s.db.QueryRow(`SELECT status FROM personal_events WHERE id=?`, event.ID).Scan(&eventStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT status FROM personal_outbox WHERE id=?`, delivery.ID).Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if eventStatus != string(personal.EventQueued) || deliveryStatus != "uncertain" {
		t.Fatalf("recovered states = event %q, delivery %q", eventStatus, deliveryStatus)
	}
}

func TestStaleTaskCallbackDoesNotLogQueuedReply(t *testing.T) {
	ctx, logs := observedStoreContext()
	s := testStore(t, ":memory:", time.Minute)
	userEvent := openUserEvent(t, s, 1, 1907, "Run the task")
	task, run, err := s.CreateTask(context.Background(), userEvent.ID, "create", "Task", personal.TaskInput{Goal: "Do work"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, userEvent, personal.TurnOutput{}, nil)
	claimed, err := s.ClaimRun(context.Background(), personal.RunTask)
	if err != nil || claimed == nil {
		t.Fatalf("claim task run = (%+v,%v)", claimed, err)
	}
	if err := s.FinishStage(context.Background(), run.ID, personal.StageResult{Summary: "stage saved"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(context.Background())
	if err != nil || callback == nil || callback.Kind != personal.EventTask {
		t.Fatalf("claim task callback = (%+v,%v)", callback, err)
	}
	cancelEvent := openUserEvent(t, s, 2, 1908, "Cancel before callback completes")
	if _, err := s.CancelTask(context.Background(), cancelEvent.ID, task.ID, task.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEvent(ctx, callback.ID, personal.TurnOutput{Text: "stale result", TaskID: task.ID}, []string{"stale result"}); err != nil {
		t.Fatal(err)
	}
	fields := assertStoreOutcome(t, logs, 0, "event.complete", "discarded_stale_callback")
	if fields["task_id"] != json.Number("0") || fields["reply_part_count"] != json.Number("0") {
		t.Fatalf("stale callback logged a task or queued part that was not committed: %v", fields)
	}
	for _, entry := range logs.All() {
		if entry.Message == "conversation.reply_queued" {
			t.Fatalf("stale callback emitted queued-reply log: %v", entry.ContextMap())
		}
	}
	// Discarding a reply clears its task_id, but run_id must keep it discoverable
	// in both directions when diagnosing why that callback never reached Telegram.
	fromEvent, err := s.Diagnostic(ctx, callback.ID, 0)
	if err != nil || len(fromEvent.Tasks) != 1 || fromEvent.Tasks[0].ID != task.ID {
		t.Fatalf("discarded callback lost task association: %+v, %v", fromEvent, err)
	}
	fromTask, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range fromTask.Events {
		if event.ID == callback.ID {
			return
		}
	}
	t.Fatal("task trace omitted the discarded callback")
}
