package sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func startDeliveryPolicyTask(t *testing.T, s *Store, sequence int64, title string) (personal.Event, personal.Task, personal.Run, personal.Event) {
	t.Helper()
	ctx := context.Background()
	user := openUserEvent(t, s, sequence, sequence, "Create "+title)
	task, _, err := s.CreateTask(ctx, user.ID, "create-"+title, title, personal.TaskInput{Goal: "Investigate " + title})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, user, personal.TurnOutput{Silent: true}, nil)
	run, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil || run.TaskID != task.ID {
		t.Fatalf("ClaimRun()=(%+v,%v), want task %d", run, err, task.ID)
	}
	callback := finishDeliveryPolicyRun(t, s, *run)
	return user, task, *run, callback
}

func finishDeliveryPolicyRun(t *testing.T, s *Store, run personal.Run) personal.Event {
	t.Helper()
	ctx := context.Background()
	if err := s.FinishStage(ctx, run.ID, personal.StageResult{Summary: "Saved stage findings"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask || callback.TaskID != run.TaskID {
		t.Fatalf("ClaimEvent()=(%+v,%v), want task callback for %d", callback, err, run.TaskID)
	}
	return *callback
}

func settleDeliveryPolicyResult(t *testing.T, s *Store, task personal.Task, run personal.Run, callback personal.Event, status personal.TaskStatus, text string) int64 {
	t.Helper()
	ctx := context.Background()
	settlement := personal.TaskSettlement{
		Status:  status,
		Summary: "Committed findings",
		Delivery: personal.DeliveryDraft{
			Text:        text,
			Attachments: []string{},
		},
	}
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, settlement); err != nil {
		t.Fatal(err)
	}
	intents, err := s.EventDeliveries(ctx, callback.ID)
	if err != nil || len(intents) != 1 || intents[0].Purpose != personal.DeliveryResult {
		t.Fatalf("EventDeliveries()=(%+v,%v), want one result", intents, err)
	}
	finishEvent(t, s, callback, personal.TurnOutput{Silent: true}, nil)
	return intents[0].ID
}

func continueDeliveryPolicyTask(t *testing.T, s *Store, sequence int64, task personal.Task) (personal.Event, personal.Run) {
	t.Helper()
	ctx := context.Background()
	user := openUserEvent(t, s, sequence, sequence, "Continue "+task.Title)
	key := "continue-" + task.Title + "-" + strconv.FormatInt(sequence, 10)
	_, run, err := s.ContinueTask(ctx, user.ID, key, task.ID, task.Revision, "Continue the existing goal", false)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, user, personal.TurnOutput{Silent: true}, nil)
	claimed, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("ClaimRun()=(%+v,%v), want run %d", claimed, err, run.ID)
	}
	return user, *claimed
}

func policyIntent(t *testing.T, snapshot DiagnosticSnapshot, deliveryID int64) personal.DeliveryIntent {
	t.Helper()
	for _, intent := range snapshot.DeliveryIntents {
		if intent.ID == deliveryID {
			return intent
		}
	}
	t.Fatalf("delivery intent %d missing from diagnostic snapshot", deliveryID)
	return personal.DeliveryIntent{}
}

func policyOutboxPart(t *testing.T, snapshot DiagnosticSnapshot, deliveryID int64, deliveryPart int) DeliveryRecord {
	t.Helper()
	for _, item := range snapshot.Deliveries {
		if item.DeliveryID == deliveryID && item.DeliveryPart == deliveryPart {
			return item
		}
	}
	t.Fatalf("delivery %d part %d missing from diagnostic snapshot", deliveryID, deliveryPart)
	return DeliveryRecord{}
}

func TestDeliveryPolicyBackoffBlocksSameTaskButAllowsIndependentTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, taskA, runA, callbackA := startDeliveryPolicyTask(t, s, 1, "Task A")
	firstID := settleDeliveryPolicyResult(t, s, taskA, runA, callbackA, personal.TaskWaitingInput, "Task A first result")
	first, err := s.ClaimDelivery(ctx)
	if err != nil || first == nil || first.DeliveryID != firstID {
		t.Fatalf("first result claim=(%+v,%v), want delivery %d", first, err, firstID)
	}
	if err := s.UpdateDelivery(ctx, first.ID, "waiting", channel.Receipt{}, "rate limited", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, runA2 := continueDeliveryPolicyTask(t, s, 2, taskA)
	callbackA2 := finishDeliveryPolicyRun(t, s, runA2)
	secondID := settleDeliveryPolicyResult(t, s, taskA, runA2, callbackA2, personal.TaskWaitingInput, "Task A second result")

	_, taskB, runB, callbackB := startDeliveryPolicyTask(t, s, 3, "Task B")
	otherID := settleDeliveryPolicyResult(t, s, taskB, runB, callbackB, personal.TaskCompleted, "Task B result")
	other, err := s.ClaimDelivery(ctx)
	if err != nil || other == nil || other.DeliveryID != otherID {
		t.Fatalf("independent result claim=(%+v,%v), want delivery %d", other, err, otherID)
	}
	receipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "task-b-result"}
	if err := s.UpdateDelivery(ctx, other.ID, "sent", receipt, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.ClaimDelivery(ctx)
	if err != nil || blocked != nil {
		t.Fatalf("same-task result bypassed the backoff: (%+v,%v)", blocked, err)
	}
	snapshot, err := s.Diagnostic(ctx, 0, taskA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstPart := policyOutboxPart(t, snapshot, firstID, 1); firstPart.Status != "pending" || firstPart.Detail != "rate limited" {
		t.Fatalf("first result after backoff=(%+v), want pending with retry detail", firstPart)
	}
	if secondPart := policyOutboxPart(t, snapshot, secondID, 1); secondPart.Status != "pending" {
		t.Fatalf("later same-task result=%+v, want pending behind the earlier result", secondPart)
	}
}

func TestDeliveryPolicySameRevisionRunKeepsResultAndSuppressesOldStatus(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	user, task, runA, callbackA := startDeliveryPolicyTask(t, s, 1, "Stable Task")
	legacyWaitFixture(t, s, user.ID)
	waitA := waitIntentForEvent(t, s, user.ID)
	resultA := settleDeliveryPolicyResult(t, s, task, runA, callbackA, personal.TaskWaitingInput, "Committed from run A")
	continueEvent, runB := continueDeliveryPolicyTask(t, s, 2, task)
	legacyWaitFixture(t, s, continueEvent.ID)
	waitB := waitIntentForEvent(t, s, continueEvent.ID)
	callbackB := finishDeliveryPolicyRun(t, s, runB)
	if _, _, err := s.ContinueTask(ctx, callbackB.ID, "continue-from-callback", task.ID, task.Revision, "Continue at the same revision", false); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEvent(ctx, callbackB.ID, personal.TurnOutput{Silent: true}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, snapshot, resultA); intent.State != personal.DeliveryIntentRendered {
		t.Fatalf("committed result after same-revision Run=%q, want rendered", intent.State)
	}
	for _, waitID := range []int64{waitA, waitB} {
		if intent := policyIntent(t, snapshot, waitID); intent.State != personal.DeliveryIntentSuppressed {
			t.Fatalf("old status delivery %d state=%q, want suppressed", waitID, intent.State)
		}
		if item := policyOutboxPart(t, snapshot, waitID, 1); item.Status != "suppressed" {
			t.Fatalf("old status delivery %d outbox status=%q, want suppressed", waitID, item.Status)
		}
	}
	claimed, err := s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.DeliveryID != resultA {
		t.Fatalf("committed same-revision result claim=(%+v,%v), want delivery %d", claimed, err, resultA)
	}
}

func TestDeliveryPolicyWaitRetryIsSuppressedAfterSameRevisionRunChange(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	user, task, _, callbackA := startDeliveryPolicyTask(t, s, 1, "Wait Retry")
	legacyWaitFixture(t, s, user.ID)
	waitID := waitIntentForEvent(t, s, user.ID)
	forcePolicyDeliveryDue(t, s, user.ID, waitID)
	claimed, err := s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.DeliveryID != waitID {
		t.Fatalf("wait claim=(%+v,%v), want delivery %d", claimed, err, waitID)
	}
	if _, _, err := s.ContinueTask(ctx, callbackA.ID, "continue-same-revision", task.ID, task.Revision, "Start a newer run", false); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEvent(ctx, callbackA.ID, personal.TurnOutput{Silent: true}, nil); err != nil {
		t.Fatal(err)
	}
	beforeRetry, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item := policyOutboxPart(t, beforeRetry, waitID, 1); item.Status != "sending" {
		t.Fatalf("new Run changed already-started wait status: %+v", item)
	}
	if err := s.UpdateDelivery(ctx, claimed.ID, "waiting", channel.Receipt{}, "rate limited", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	afterRetry, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, afterRetry, waitID); intent.State != personal.DeliveryIntentSuppressed {
		t.Fatalf("retry wait intent=%+v, want suppressed", intent)
	}
	if item := policyOutboxPart(t, afterRetry, waitID, 1); item.Status != "suppressed" {
		t.Fatalf("retry wait outbox=%+v, want suppressed", item)
	}
}

func waitIntentForEvent(t *testing.T, s *Store, eventID int64) int64 {
	t.Helper()
	snapshot, err := s.Diagnostic(context.Background(), eventID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range snapshot.DeliveryIntents {
		if intent.EventID == eventID && intent.Purpose == personal.DeliveryStatus {
			return intent.ID
		}
	}
	t.Fatalf("event %d has no waiting status intent", eventID)
	return 0
}

func forcePolicyDeliveryDue(t *testing.T, s *Store, eventID, deliveryID int64) {
	t.Helper()
	snapshot, err := s.Diagnostic(context.Background(), eventID, 0)
	if err != nil {
		t.Fatal(err)
	}
	item := policyOutboxPart(t, snapshot, deliveryID, 1)
	if _, err := s.db.ExecContext(context.Background(), `UPDATE personal_outbox SET next_attempt_at=? WHERE id=?`, timestamp(time.Now().Add(-time.Second)), item.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryPolicyRevisionChangeSuppressesOldResult(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := startDeliveryPolicyTask(t, s, 1, "Revisable Task")
	resultID := settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskWaitingInput, "Result for revision one")
	edit := openUserEvent(t, s, 2, 2, "Change the task goal")
	updated, _, err := s.ReviseTask(ctx, edit.ID, "revise-goal", task.ID, task.Revision,
		personal.TaskInput{Goal: "Investigate the revised target", Context: task.Context})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != task.Revision+1 {
		t.Fatalf("revision=%d, want %d", updated.Revision, task.Revision+1)
	}
	finishEvent(t, s, edit, personal.TurnOutput{Silent: true}, nil)
	snapshot, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, snapshot, resultID); intent.State != personal.DeliveryIntentSuppressed {
		t.Fatalf("old result state=%q, want suppressed after target revision change", intent.State)
	}
	if item := policyOutboxPart(t, snapshot, resultID, 1); item.Status != "suppressed" || item.Detail == "" {
		t.Fatalf("old result outbox=%+v, want suppressed with a reason", item)
	}
	if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
		t.Fatalf("old revision result remained claimable: (%+v,%v)", next, err)
	}
}

func TestDeliveryPolicyCancelStopsCalculationButKeepsCommittedResult(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := startDeliveryPolicyTask(t, s, 1, "Cancellable Task")
	resultID := settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskWaitingInput, "Saved result before cancellation")
	cancel := openUserEvent(t, s, 2, 2, "Cancel the remaining calculation")
	if canceled, err := s.CancelTask(ctx, cancel.ID, task.ID, task.Revision); err != nil || canceled.Status != personal.TaskCancelled {
		t.Fatalf("CancelTask()=(%+v,%v)", canceled, err)
	}
	finishEvent(t, s, cancel, personal.TurnOutput{Silent: true}, nil)
	claimed, err := s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.DeliveryID != resultID {
		t.Fatalf("saved result after calculation cancellation=(%+v,%v), want delivery %d", claimed, err, resultID)
	}
	snapshot, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, snapshot, resultID); intent.State != personal.DeliveryIntentRendered {
		t.Fatalf("cancel changed saved delivery state to %q", intent.State)
	}
}

func TestDeliveryPolicyStopDuringSendingSuppressesReturnedRetry(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := startDeliveryPolicyTask(t, s, 1, "Stopped Delivery")
	deliveryID := settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskWaitingInput, "Result being sent")
	claimed, err := s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.DeliveryID != deliveryID {
		t.Fatalf("ClaimDelivery()=(%+v,%v)", claimed, err)
	}
	stopEvent := openUserEvent(t, s, 2, 2, "Do not send more results for this task")
	control, err := s.ControlDelivery(ctx, stopEvent.ID, "stop-task", personal.DeliveryControlRequest{
		Action: personal.DeliveryStop, TaskID: task.ID,
	})
	if err != nil || control.DeliveryID <= 0 || control.Request.Action != personal.DeliveryStop {
		t.Fatalf("ControlDelivery()=(%+v,%v), want stop confirmation", control, err)
	}
	controlSnapshot, err := s.Diagnostic(ctx, stopEvent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, controlSnapshot, control.DeliveryID); intent.Purpose != personal.DeliveryControl || intent.State != personal.DeliveryIntentPending {
		t.Fatalf("stop confirmation intent=%+v, want pending control intent", intent)
	}
	finishEvent(t, s, stopEvent, personal.TurnOutput{Silent: true}, nil)
	beforeRetry, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item := policyOutboxPart(t, beforeRetry, deliveryID, 1); item.Status != "sending" {
		t.Fatalf("Stop changed already-started item: %+v", item)
	}
	if err := s.UpdateDelivery(ctx, claimed.ID, "waiting", channel.Receipt{}, "rate limited", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	afterRetry, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, afterRetry, deliveryID); intent.State != personal.DeliveryIntentSuppressed || !strings.Contains(intent.Detail, "task_delivery_stopped") {
		t.Fatalf("returned retry intent=%+v, want suppressed by stop gate", intent)
	}
	if item := policyOutboxPart(t, afterRetry, deliveryID, 1); item.Status != "suppressed" {
		t.Fatalf("returned retry outbox=%+v, want suppressed", item)
	}
}

func TestLegacyWaitRefreshesStopGateAndKeepsGroupReferences(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := startDeliveryPolicyTask(t, s, 1, "Stopped Wait Notice")
	settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskWaitingInput, "Result before stop")
	stopEvent := openUserEvent(t, s, 2, 2, "Stop automatic task delivery")
	control, err := s.ControlDelivery(ctx, stopEvent.ID, "stop-before-wait", personal.DeliveryControlRequest{
		Action: personal.DeliveryStop, TaskID: task.ID,
	})
	if err != nil || control.DeliveryID <= 0 {
		t.Fatalf("ControlDelivery()=(%+v,%v)", control, err)
	}
	finishEvent(t, s, stopEvent, personal.TurnOutput{Silent: true}, nil)
	continueEvent, _ := continueDeliveryPolicyTask(t, s, 3, task)
	legacyWaitFixture(t, s, continueEvent.ID)
	waitID := waitIntentForEvent(t, s, continueEvent.ID)
	snapshot, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, snapshot, waitID); intent.State != personal.DeliveryIntentSuppressed || !strings.Contains(intent.Detail, "task_delivery_stopped") {
		t.Fatalf("new wait intent=%+v, want suppressed by stop gate", intent)
	}
	if item := policyOutboxPart(t, snapshot, waitID, 1); item.Status != "suppressed" {
		t.Fatalf("new wait outbox=%+v, want suppressed", item)
	}
	var notified int
	var outboxID, historyID sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT wait_notified,wait_outbox_id,wait_history_id FROM personal_reply_groups WHERE id=?`, continueEvent.ID).
		Scan(&notified, &outboxID, &historyID); err != nil {
		t.Fatal(err)
	}
	if notified != 1 || !outboxID.Valid || !historyID.Valid {
		t.Fatalf("suppressed wait lost group references: notified=%d outbox=%v history=%v", notified, outboxID, historyID)
	}
}

func TestDeliveryPolicyStopAndRevisionKeepAcceptedReceipt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := startDeliveryPolicyTask(t, s, 1, "Accepted Delivery")
	deliveryID := settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskWaitingInput, "Already accepted result")
	claimed, err := s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.DeliveryID != deliveryID {
		t.Fatalf("ClaimDelivery()=(%+v,%v)", claimed, err)
	}
	receipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "accepted-result"}
	if err := s.UpdateDelivery(ctx, claimed.ID, "sent", receipt, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	stopEvent := openUserEvent(t, s, 2, 2, "Stop future automatic delivery")
	control, err := s.ControlDelivery(ctx, stopEvent.ID, "stop-after-send", personal.DeliveryControlRequest{
		Action: personal.DeliveryStop, TaskID: task.ID,
	})
	if err != nil || control.DeliveryID <= 0 {
		t.Fatalf("ControlDelivery()=(%+v,%v)", control, err)
	}
	finishEvent(t, s, stopEvent, personal.TurnOutput{Silent: true}, nil)
	reviseEvent := openUserEvent(t, s, 3, 3, "Revise the task target")
	updated, _, err := s.ReviseTask(ctx, reviseEvent.ID, "revise-after-send", task.ID, task.Revision,
		personal.TaskInput{Goal: "Changed target", Context: task.Context})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, reviseEvent, personal.TurnOutput{Silent: true}, nil)
	snapshot, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != task.Revision+1 {
		t.Fatalf("revision=%d, want %d", updated.Revision, task.Revision+1)
	}
	if intent := policyIntent(t, snapshot, deliveryID); intent.State != personal.DeliveryIntentRendered {
		t.Fatalf("fully sent intent changed after stop/revision: %+v", intent)
	}
	if item := policyOutboxPart(t, snapshot, deliveryID, 1); item.Status != "sent" || item.Receipt == nil || *item.Receipt != receipt {
		t.Fatalf("accepted result changed after stop/revision: %+v", item)
	}
}

func TestDeliveryPolicyFailedOrUncertainPartSuppressesRemainderAndReleasesNext(t *testing.T) {
	for _, finalStatus := range []string{"failed", "uncertain"} {
		t.Run(finalStatus, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			_, task, runA, callbackA := startDeliveryPolicyTask(t, s, 1, "Multipart Task")
			firstText := strings.Repeat("A", 4500)
			firstID := settleDeliveryPolicyResult(t, s, task, runA, callbackA, personal.TaskWaitingInput, firstText)
			_, runB := continueDeliveryPolicyTask(t, s, 2, task)
			callbackB := finishDeliveryPolicyRun(t, s, runB)
			secondID := settleDeliveryPolicyResult(t, s, task, runB, callbackB, personal.TaskCompleted, "Later result")

			first, err := s.ClaimDelivery(ctx)
			if err != nil || first == nil || first.DeliveryID != firstID || first.DeliveryPart != 1 {
				t.Fatalf("first part claim=(%+v,%v)", first, err)
			}
			prefixReceipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "accepted-prefix"}
			if err := s.UpdateDelivery(ctx, first.ID, "sent", prefixReceipt, "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			second, err := s.ClaimDelivery(ctx)
			if err != nil || second == nil || second.DeliveryID != firstID || second.DeliveryPart != 2 {
				t.Fatalf("second part claim=(%+v,%v), want part two of %d", second, err, firstID)
			}
			detail := "permanent send failure"
			if finalStatus == "uncertain" {
				detail = "remote outcome unknown"
			}
			if err := s.UpdateDelivery(ctx, second.ID, finalStatus, channel.Receipt{}, detail, time.Time{}); err != nil {
				t.Fatal(err)
			}
			next, err := s.ClaimDelivery(ctx)
			if err != nil || next == nil || next.DeliveryID != secondID || next.DeliveryPart != 1 {
				t.Fatalf("later task result after %s part=(%+v,%v), want delivery %d", finalStatus, next, err, secondID)
			}
			snapshot, err := s.Diagnostic(ctx, 0, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if item := policyOutboxPart(t, snapshot, firstID, 1); item.Status != "sent" || item.Receipt == nil || item.Receipt.MessageID != prefixReceipt.MessageID {
				t.Fatalf("accepted prefix receipt changed: %+v", item)
			}
			if item := policyOutboxPart(t, snapshot, firstID, 2); item.Status != finalStatus || item.Detail != detail {
				t.Fatalf("terminal part=(%+v), want %s with detail", item, finalStatus)
			}
			if item := policyOutboxPart(t, snapshot, firstID, 3); item.Status != "suppressed" || item.Detail == "" {
				t.Fatalf("unattempted remainder=(%+v), want suppressed with a reason", item)
			}
			if item := policyOutboxPart(t, snapshot, secondID, 1); item.Status != "sending" {
				t.Fatalf("released next delivery part=%+v, want sending", item)
			}
		})
	}
}

func TestDeliveryPolicyRecoveryKeepsSendingItemUncertain(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := startDeliveryPolicyTask(t, s, 1, "Interrupted Delivery")
	deliveryID := settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskCompleted, "May have reached the user")
	claimed, err := s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.DeliveryID != deliveryID {
		t.Fatalf("ClaimDelivery()=(%+v,%v)", claimed, err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item := policyOutboxPart(t, snapshot, deliveryID, 1); item.Status != "uncertain" || item.Detail == "" {
		t.Fatalf("recovered item=(%+v), want uncertain with outcome detail", item)
	}
	if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
		t.Fatalf("recovery retried unknown delivery: (%+v,%v)", next, err)
	}
}

func TestDeliveryPolicyMergedTaskScopesBlockOnlySharedTasks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	mergedEvent := openUserEvent(t, s, 1, 1, "Create two related tasks")
	taskA, _, err := s.CreateTask(ctx, mergedEvent.ID, "create-a", "Task A", personal.TaskInput{Goal: "Investigate A"})
	if err != nil {
		t.Fatal(err)
	}
	taskB, _, err := s.CreateTask(ctx, mergedEvent.ID, "create-b", "Task B", personal.TaskInput{Goal: "Investigate B"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, mergedEvent, personal.TurnOutput{Text: "Both tasks are underway."}, []string{"Both tasks are underway."})
	mergedSnapshot, err := s.Diagnostic(ctx, mergedEvent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var merged personal.DeliveryIntent
	for _, intent := range mergedSnapshot.DeliveryIntents {
		if intent.EventID == mergedEvent.ID && intent.Purpose == personal.DeliveryResult {
			merged = intent
			break
		}
	}
	if merged.ID == 0 || len(merged.Scopes) != 2 || merged.Scopes[0].TaskID != taskA.ID || merged.Scopes[1].TaskID != taskB.ID {
		t.Fatalf("merged delivery scopes=%+v, want Tasks %d and %d", merged.Scopes, taskA.ID, taskB.ID)
	}
	started, err := s.ClaimDelivery(ctx)
	if err != nil || started == nil || started.DeliveryID != merged.ID {
		t.Fatalf("merged delivery claim=(%+v,%v), want %d", started, err, merged.ID)
	}
	if err := s.UpdateDelivery(ctx, started.ID, "waiting", channel.Receipt{}, "delivery backoff", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	runA, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || runA == nil || runA.TaskID != taskA.ID {
		t.Fatalf("Task A run=(%+v,%v)", runA, err)
	}
	callbackA := finishDeliveryPolicyRun(t, s, *runA)
	resultA := settleDeliveryPolicyResult(t, s, taskA, *runA, callbackA, personal.TaskWaitingInput, "A result")
	runB, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || runB == nil || runB.TaskID != taskB.ID {
		t.Fatalf("Task B run=(%+v,%v)", runB, err)
	}
	callbackB := finishDeliveryPolicyRun(t, s, *runB)
	resultB := settleDeliveryPolicyResult(t, s, taskB, *runB, callbackB, personal.TaskWaitingInput, "B result")

	userC := openUserEvent(t, s, 2, 2, "Create independent task C")
	taskC, _, err := s.CreateTask(ctx, userC.ID, "create-c", "Task C", personal.TaskInput{Goal: "Investigate C"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, userC, personal.TurnOutput{Silent: true}, nil)
	runC, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || runC == nil || runC.TaskID != taskC.ID {
		t.Fatalf("Task C run=(%+v,%v)", runC, err)
	}
	callbackC := finishDeliveryPolicyRun(t, s, *runC)
	resultC := settleDeliveryPolicyResult(t, s, taskC, *runC, callbackC, personal.TaskCompleted, "Independent C result")
	next, err := s.ClaimDelivery(ctx)
	if err != nil || next == nil || next.DeliveryID != resultC {
		t.Fatalf("independent delivery was blocked by merged scopes: (%+v,%v), want %d", next, err, resultC)
	}
	if item := policyOutboxPart(t, mustDiagnostic(t, s, taskA.ID), resultA, 1); item.Status != "pending" {
		t.Fatalf("Task A result behind shared Task scope=%+v, want pending", item)
	}
	if item := policyOutboxPart(t, mustDiagnostic(t, s, taskB.ID), resultB, 1); item.Status != "pending" {
		t.Fatalf("Task B result behind shared Task scope=%+v, want pending", item)
	}
}

func mustDiagnostic(t *testing.T, s *Store, taskID int64) DiagnosticSnapshot {
	t.Helper()
	snapshot, err := s.Diagnostic(context.Background(), 0, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
