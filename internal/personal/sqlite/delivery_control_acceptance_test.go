package sqlite

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func assertDeliveryControlReplay(t *testing.T, first, replay personal.DeliveryControlResult) {
	t.Helper()
	if first.ID != replay.ID || first.DeliveryID != replay.DeliveryID || first.Request != replay.Request || !first.CreatedAt.Equal(replay.CreatedAt) {
		t.Fatalf("control replay=(%+v), original=(%+v)", replay, first)
	}
}

func TestControlDeliveryStopBeforeResultGatesFutureDelivery(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := deliveryTestCallback(t, s)
	stopEvent := openUserEvent(t, s, 2, 2, "Stop future task notifications")
	stop, err := s.ControlDelivery(ctx, stopEvent.ID, "stop-before-result", personal.DeliveryControlRequest{
		Action: personal.DeliveryStop,
		TaskID: task.ID,
	})
	if err != nil || stop.DeliveryID <= 0 {
		t.Fatalf("ControlDelivery(stop)=(%+v,%v)", stop, err)
	}
	page, err := s.QueryDeliveries(ctx, personal.ReadScope{EventID: stopEvent.ID}, personal.DeliveryQuery{TaskID: task.ID})
	if err != nil || !reflect.DeepEqual(page.StoppedTaskIDs, []int64{task.ID}) {
		t.Fatalf("stop gate before result=(%+v,%v)", page, err)
	}
	var controlOutbox int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE delivery_id=?`, stop.DeliveryID).Scan(&controlOutbox); err != nil || controlOutbox != 0 {
		t.Fatalf("unrendered stop confirmation has %d outbox rows: %v", controlOutbox, err)
	}

	resultID := settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskCompleted, "Result generated after stop")
	snapshot, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, snapshot, resultID); intent.State != personal.DeliveryIntentSuppressed || !strings.Contains(intent.Detail, "task_delivery_stopped") {
		t.Fatalf("future result after stop=(%+v), want suppressed", intent)
	}
	var resultOutbox int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE delivery_id=?`, resultID).Scan(&resultOutbox); err != nil || resultOutbox != 0 {
		t.Fatalf("future result created %d outbox rows after stop: %v", resultOutbox, err)
	}
}

func TestControlDeliveryResumeKeepsOldSuppressionAndAllowsFutureResult(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := startDeliveryPolicyTask(t, s, 1, "Resumable task")
	oldID := settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskWaitingInput, "Old committed result")

	stopEvent := openUserEvent(t, s, 2, 2, "Stop")
	if _, err := s.ControlDelivery(ctx, stopEvent.ID, "stop", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, stopEvent, personal.TurnOutput{Silent: true}, nil)

	resumeEvent := openUserEvent(t, s, 3, 3, "Resume")
	if _, err := s.ControlDelivery(ctx, resumeEvent.ID, "resume", personal.DeliveryControlRequest{Action: personal.DeliveryResume, TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, resumeEvent, personal.TurnOutput{Silent: true}, nil)
	snapshot, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, snapshot, oldID); intent.State != personal.DeliveryIntentSuppressed {
		t.Fatalf("resume restored old result=%q", intent.State)
	}

	_, nextRun := continueDeliveryPolicyTask(t, s, 4, task)
	nextCallback := finishDeliveryPolicyRun(t, s, nextRun)
	futureID := settleDeliveryPolicyResult(t, s, task, nextRun, nextCallback, personal.TaskCompleted, "Future result after resume")
	snapshot, err = s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, snapshot, oldID); intent.State != personal.DeliveryIntentSuppressed {
		t.Fatalf("old result after resume=%q, want suppressed", intent.State)
	}
	if intent := policyIntent(t, snapshot, futureID); intent.State != personal.DeliveryIntentRendered {
		t.Fatalf("future result after resume=%q, want rendered", intent.State)
	}
}

func TestControlDeliveryReplayIsStableAndPayloadBound(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := startDeliveryPolicyTask(t, s, 1, "Replay task")

	stopEvent := openUserEvent(t, s, 2, 2, "Stop")
	stopRequest := personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID}
	firstStop, err := s.ControlDelivery(ctx, stopEvent.ID, "same-stop", stopRequest)
	if err != nil {
		t.Fatal(err)
	}
	replayedStop, err := s.ControlDelivery(ctx, stopEvent.ID, "same-stop", stopRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertDeliveryControlReplay(t, firstStop, replayedStop)
	if _, err := s.ControlDelivery(ctx, stopEvent.ID, "same-stop", personal.DeliveryControlRequest{Action: personal.DeliveryResume, TaskID: task.ID}); err == nil {
		t.Fatal("same stop key accepted a different payload")
	}
	stopIntents, err := s.EventDeliveries(ctx, stopEvent.ID)
	if err != nil || len(stopIntents) != 1 || stopIntents[0].ID != firstStop.DeliveryID {
		t.Fatalf("stop replay created duplicate effects: intents=(%+v,%v)", stopIntents, err)
	}

	resumeEvent := openUserEvent(t, s, 3, 3, "Resume")
	resumeRequest := personal.DeliveryControlRequest{Action: personal.DeliveryResume, TaskID: task.ID}
	firstResume, err := s.ControlDelivery(ctx, resumeEvent.ID, "same-resume", resumeRequest)
	if err != nil {
		t.Fatal(err)
	}
	replayedResume, err := s.ControlDelivery(ctx, resumeEvent.ID, "same-resume", resumeRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertDeliveryControlReplay(t, firstResume, replayedResume)
	if _, err := s.ControlDelivery(ctx, resumeEvent.ID, "same-resume", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID}); err == nil {
		t.Fatal("same resume key accepted a different payload")
	}
	resumeIntents, err := s.EventDeliveries(ctx, resumeEvent.ID)
	if err != nil || len(resumeIntents) != 1 || resumeIntents[0].ID != firstResume.DeliveryID {
		t.Fatalf("resume replay created duplicate effects: intents=(%+v,%v)", resumeIntents, err)
	}

	resultID := settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskWaitingInput, "Saved before resend")
	resendEvent := openUserEvent(t, s, 4, 4, "Resend old result")
	resendRequest := personal.DeliveryControlRequest{Action: personal.DeliveryResend, DeliveryID: resultID}
	firstResend, err := s.ControlDelivery(ctx, resendEvent.ID, "same-resend", resendRequest)
	if err != nil {
		t.Fatal(err)
	}
	replayedResend, err := s.ControlDelivery(ctx, resendEvent.ID, "same-resend", resendRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertDeliveryControlReplay(t, firstResend, replayedResend)
	if _, err := s.ControlDelivery(ctx, resendEvent.ID, "same-resend", personal.DeliveryControlRequest{Action: personal.DeliveryResend, DeliveryID: resultID + 1}); err == nil {
		t.Fatal("same resend key accepted a different payload")
	}
	resendIntents, err := s.EventDeliveries(ctx, resendEvent.ID)
	if err != nil || len(resendIntents) != 1 || resendIntents[0].ID != firstResend.DeliveryID || resendIntents[0].OriginalDeliveryID != resultID {
		t.Fatalf("resend replay created duplicate effects: intents=(%+v,%v)", resendIntents, err)
	}
}

func TestControlDeliveryResendsHistoricalRevisionAndPreservesUnknownOriginal(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := deliveryTestCallback(t, s)
	first, err := s.SaveAttachment(ctx, "first.txt", "text/plain", strings.NewReader("first file"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveAttachment(ctx, "second.txt", "text/plain", strings.NewReader("second file"))
	if err != nil {
		t.Fatal(err)
	}
	settlement := personal.TaskSettlement{
		Status:  personal.TaskCompleted,
		Summary: "Historical result",
		Delivery: personal.DeliveryDraft{
			Text:        "Original revision findings",
			Attachments: []string{second.ID, first.ID},
		},
	}
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, settlement); err != nil {
		t.Fatal(err)
	}
	originals, err := s.EventDeliveries(ctx, callback.ID)
	if err != nil || len(originals) != 1 || originals[0].Purpose != personal.DeliveryResult || len(originals[0].Scopes) != 1 {
		t.Fatalf("original delivery=(%+v,%v)", originals, err)
	}
	original := originals[0]
	finishEvent(t, s, callback, personal.TurnOutput{Silent: true}, nil)
	firstPart, err := s.ClaimDelivery(ctx)
	if err != nil || firstPart == nil || firstPart.DeliveryID != original.ID || firstPart.DeliveryPart != 1 {
		t.Fatalf("claim original text=(%+v,%v)", firstPart, err)
	}
	receipt := channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "accepted-original-prefix"}
	if err := s.UpdateDelivery(ctx, firstPart.ID, "sent", receipt, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	unknownPart, err := s.ClaimDelivery(ctx)
	if err != nil || unknownPart == nil || unknownPart.DeliveryID != original.ID || unknownPart.DeliveryPart != 2 || unknownPart.AttachmentID != second.ID {
		t.Fatalf("claim original first attachment=(%+v,%v)", unknownPart, err)
	}
	if err := s.UpdateDelivery(ctx, unknownPart.ID, "uncertain", channel.Receipt{}, "remote result unknown", time.Time{}); err != nil {
		t.Fatal(err)
	}

	reviseEvent := openUserEvent(t, s, 2, 2, "Revise task after original output")
	revised, _, err := s.ReviseTask(ctx, reviseEvent.ID, "revise-after-result", task.ID, task.Revision, personal.TaskInput{Goal: "New revision goal"})
	if err != nil || revised.Revision != task.Revision+1 {
		t.Fatalf("ReviseTask()=(%+v,%v)", revised, err)
	}
	finishEvent(t, s, reviseEvent, personal.TurnOutput{Silent: true}, nil)

	resendEvent := openUserEvent(t, s, 3, 3, "Resend prior revision")
	resend, err := s.ControlDelivery(ctx, resendEvent.ID, "resend-old", personal.DeliveryControlRequest{Action: personal.DeliveryResend, DeliveryID: original.ID})
	if err != nil || resend.DeliveryID <= 0 {
		t.Fatalf("ControlDelivery(resend historical)=(%+v,%v)", resend, err)
	}
	finishEvent(t, s, resendEvent, personal.TurnOutput{Silent: true}, nil)
	snapshot, err := s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	historical := policyIntent(t, snapshot, resend.DeliveryID)
	if historical.Purpose != personal.DeliveryHistorical || historical.OriginalDeliveryID != original.ID || !reflect.DeepEqual(historical.Scopes, original.Scopes) {
		t.Fatalf("historical reference/scope=(%+v), original=(%+v)", historical, original)
	}
	if !reflect.DeepEqual(historical.Draft.Attachments, []string{second.ID, first.ID}) || !strings.Contains(historical.Draft.Text, "Original revision findings") {
		t.Fatalf("historical draft lost original content/order: %+v", historical.Draft)
	}
	if item := policyOutboxPart(t, snapshot, resend.DeliveryID, 2); item.AttachmentID != second.ID {
		t.Fatalf("historical attachment part 2=(%+v), want %q", item, second.ID)
	}
	if item := policyOutboxPart(t, snapshot, resend.DeliveryID, 3); item.AttachmentID != first.ID {
		t.Fatalf("historical attachment part 3=(%+v), want %q", item, first.ID)
	}

	stopEvent := openUserEvent(t, s, 4, 4, "Stop historical resend")
	if _, err := s.ControlDelivery(ctx, stopEvent.ID, "stop-history", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, stopEvent, personal.TurnOutput{Silent: true}, nil)
	snapshot, err = s.Diagnostic(ctx, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent := policyIntent(t, snapshot, resend.DeliveryID); intent.State != personal.DeliveryIntentSuppressed {
		t.Fatalf("historical resend after Task stop=%q, want suppressed", intent.State)
	}
	for part := 1; part <= 3; part++ {
		if item := policyOutboxPart(t, snapshot, resend.DeliveryID, part); item.Status != "suppressed" {
			t.Fatalf("historical resend part %d after stop=(%+v), want suppressed", part, item)
		}
	}
	if item := policyOutboxPart(t, snapshot, original.ID, 1); item.Status != "sent" || item.Receipt == nil || *item.Receipt != receipt {
		t.Fatalf("accepted original prefix changed after revision/stop=(%+v), want receipt %+v", item, receipt)
	}
	if item := policyOutboxPart(t, snapshot, original.ID, 2); item.Status != "uncertain" {
		t.Fatalf("unknown original part changed after revision/stop=(%+v)", item)
	}
}

func TestControlDeliveryRejectsTaskCallbackAuthority(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, _, callback := deliveryTestCallback(t, s)
	if _, err := s.ControlDelivery(ctx, callback.ID, "callback-stop", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID}); err == nil {
		t.Fatal("task callback gained control authority")
	}
	var controls, intents int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_delivery_controls WHERE event_id=?`, callback.ID).Scan(&controls); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_delivery_intents WHERE event_id=?`, callback.ID).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if controls != 0 || intents != 0 {
		t.Fatalf("rejected callback mutated control state: controls=%d intents=%d", controls, intents)
	}
}

func TestControlDeliveryStopRollsBackWhenOutboxSuppressionFails(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := deliveryTestCallback(t, s)
	resultID := settleDeliveryPolicyResult(t, s, task, run, callback, personal.TaskCompleted, "Result stays pending")
	stopEvent := openUserEvent(t, s, 2, 2, "Stop task")
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_delivery_suppression BEFORE UPDATE OF status ON personal_outbox
		WHEN NEW.status='suppressed' BEGIN SELECT RAISE(ABORT,'forced suppression failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ControlDelivery(ctx, stopEvent.ID, "rollback-stop", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID}); err == nil {
		t.Fatal("ControlDelivery(stop) succeeded despite rejected outbox suppression")
	}
	var controls, stopIntents int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_delivery_controls WHERE event_id=? AND request_key='rollback-stop'`, stopEvent.ID).Scan(&controls); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_delivery_intents WHERE event_id=?`, stopEvent.ID).Scan(&stopIntents); err != nil {
		t.Fatal(err)
	}
	var state personal.DeliveryIntentState
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM personal_delivery_intents WHERE id=?`, resultID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	var outboxStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE delivery_id=?`, resultID).Scan(&outboxStatus); err != nil {
		t.Fatal(err)
	}
	if controls != 0 || stopIntents != 0 || state != personal.DeliveryIntentRendered || outboxStatus != "pending" {
		t.Fatalf("failed stop was partially committed: controls=%d intents=%d result=%q outbox=%q", controls, stopIntents, state, outboxStatus)
	}
}

func TestCancelTaskReplayReturnsOriginalSnapshotAfterRevision(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	originalEvent := openUserEvent(t, s, 1, 1, "Create task")
	task, _ := createTask(t, s, originalEvent, "create-for-cancel-replay", "Cancel replay", "Original goal", "")
	cancelled, err := s.CancelTask(ctx, originalEvent.ID, task.ID, task.Revision)
	if err != nil || cancelled.Status != personal.TaskCancelled {
		t.Fatalf("CancelTask()=(%+v,%v)", cancelled, err)
	}
	finishEvent(t, s, originalEvent, personal.TurnOutput{Silent: true}, nil)

	reviseEvent := openUserEvent(t, s, 2, 2, "Revise after cancel")
	revised, _, err := s.ReviseTask(ctx, reviseEvent.ID, "revise-after-cancel", task.ID, task.Revision, personal.TaskInput{Goal: "Revised goal"})
	if err != nil || revised.Revision != task.Revision+1 || revised.Status != personal.TaskQueued {
		t.Fatalf("ReviseTask()=(%+v,%v)", revised, err)
	}
	finishEvent(t, s, reviseEvent, personal.TurnOutput{Silent: true}, nil)

	replayed, err := s.CancelTask(ctx, originalEvent.ID, task.ID, task.Revision)
	if err != nil {
		t.Fatalf("CancelTask replay after revision: %v", err)
	}
	if replayed.ID != cancelled.ID || replayed.Revision != cancelled.Revision || replayed.Status != cancelled.Status || replayed.Goal != cancelled.Goal || replayed.Summary != cancelled.Summary || !replayed.UpdatedAt.Equal(cancelled.UpdatedAt) {
		t.Fatalf("CancelTask replay=(%+v), original snapshot=(%+v)", replayed, cancelled)
	}
	current, err := s.Task(ctx, task.ID)
	if err != nil || current.Revision != revised.Revision || current.Status != revised.Status || current.Goal != revised.Goal {
		t.Fatalf("replaying old cancellation changed revised Task=(%+v,%v), revised snapshot=(%+v)", current, err, revised)
	}
	var controls, intents int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_delivery_controls WHERE event_id=? AND action='cancel_task'`, originalEvent.ID).Scan(&controls); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_delivery_intents WHERE event_id=? AND purpose='control'`, originalEvent.ID).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if controls != 1 || intents != 1 {
		t.Fatalf("cancellation replay duplicated effects: controls=%d intents=%d", controls, intents)
	}
}
