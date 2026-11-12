package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func deliveryTestCallback(t *testing.T, s *Store) (personal.Event, personal.Task, personal.Run, personal.Event) {
	t.Helper()
	ctx := context.Background()
	user := openUserEvent(t, s, 1, 1, "Create a task")
	task, _, err := s.CreateTask(ctx, user.ID, "create", "Research", personal.TaskInput{Goal: "Collect useful findings"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, user, personal.TurnOutput{Silent: true}, nil)
	run, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatalf("ClaimRun()=(%+v,%v)", run, err)
	}
	if err := s.FinishStage(ctx, run.ID, personal.StageResult{Summary: "Stage finding"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask {
		t.Fatalf("ClaimEvent()=(%+v,%v)", callback, err)
	}
	return user, task, *run, *callback
}

func TestSettlementIntentsCoverEverySettledStatus(t *testing.T) {
	for _, status := range []personal.TaskStatus{personal.TaskCompleted, personal.TaskFailed, personal.TaskWaitingInput} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			user, task, run, callback := deliveryTestCallback(t, s)
			settlement := testTaskSettlement(status, "Saved stage findings")
			settled, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, settlement)
			if err != nil || settled.Status != status || settled.Summary != settlement.Summary {
				t.Fatalf("SettleTask()=(%+v,%v)", settled, err)
			}
			intents, err := s.EventDeliveries(ctx, callback.ID)
			if err != nil || len(intents) != 1 || intents[0].Purpose != personal.DeliveryResult || intents[0].Draft.Text != settlement.Delivery.Text {
				t.Fatalf("EventDeliveries()=(%+v,%v)", intents, err)
			}
			// A settlement's saved payload wins over the later model output and its
			// task link, even if that link is invalid.
			finishEvent(t, s, callback, personal.TurnOutput{Text: "dynamic output", TaskID: task.ID + 999}, []string{"dynamic output"})
			var historyText string
			if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_history WHERE event_id=? AND role='assistant'`, callback.ID).Scan(&historyText); err != nil || historyText != settlement.Delivery.Text {
				t.Fatalf("assistant History text=(%q,%v), want saved draft", historyText, err)
			}
			var groupContent, groupClosed int
			if err := s.db.QueryRowContext(ctx, `SELECT has_content,closed FROM personal_reply_groups WHERE id=?`, user.ID).Scan(&groupContent, &groupClosed); err != nil || groupContent != 1 || groupClosed != 1 {
				t.Fatalf("settlement did not close the reply ledger: content=%d closed=%d err=%v", groupContent, groupClosed, err)
			}
			if intents, err := s.EventDeliveries(ctx, callback.ID); err != nil || len(intents) != 0 {
				t.Fatalf("rendered settlement remains pending: (%+v,%v)", intents, err)
			}
		})
	}
}

func TestFileOnlySettlementKeepsTaskSummaryAndAttachmentOrder(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	user, task, run, callback := deliveryTestCallback(t, s)
	first, err := s.SaveAttachment(ctx, "result.txt", "text/plain", strings.NewReader("first version"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveAttachment(ctx, "result.txt", "text/plain", strings.NewReader("second version"))
	if err != nil {
		t.Fatal(err)
	}
	settlement := personal.TaskSettlement{Status: personal.TaskCompleted, Summary: "Findings stay in the Task summary",
		Delivery: personal.DeliveryDraft{Text: "", Attachments: []string{second.ID, first.ID}}}
	settled, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, settlement)
	if err != nil || settled.Summary != settlement.Summary {
		t.Fatalf("file-only settlement=(%+v,%v)", settled, err)
	}
	intents, err := s.EventDeliveries(ctx, callback.ID)
	if err != nil || len(intents) != 1 || len(intents[0].Draft.Attachments) != 2 || intents[0].Draft.Attachments[0] != second.ID || intents[0].Draft.Attachments[1] != first.ID {
		t.Fatalf("saved attachment order=(%+v,%v)", intents, err)
	}
	finishEvent(t, s, callback, personal.TurnOutput{Text: "discard this final selection"}, []string{"discard this final selection"})
	var historyText, rawAttachments string
	if err := s.db.QueryRowContext(ctx, `SELECT text,attachments FROM personal_history WHERE event_id=? AND role='assistant'`, callback.ID).Scan(&historyText, &rawAttachments); err != nil || historyText != "" {
		t.Fatalf("file-only History=(%q,%q,%v)", historyText, rawAttachments, err)
	}
	var historyAttachments []channel.Attachment
	if err := json.Unmarshal([]byte(rawAttachments), &historyAttachments); err != nil || len(historyAttachments) != 2 || historyAttachments[0].ID != second.ID || historyAttachments[1].ID != first.ID {
		t.Fatalf("History attachment order=(%+v,%v)", historyAttachments, err)
	}
	var deliveryID int64
	var outboxAttachments []string
	rows, err := s.db.QueryContext(ctx, `SELECT delivery_id,attachment_id FROM personal_outbox WHERE event_id=? ORDER BY part`, callback.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var attachmentID sql.NullString
		if err := rows.Scan(&id, &attachmentID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		deliveryID = id
		outboxAttachments = append(outboxAttachments, attachmentID.String)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(outboxAttachments) != 2 || outboxAttachments[0] != second.ID || outboxAttachments[1] != first.ID {
		t.Fatalf("outbox attachment order=%v", outboxAttachments)
	}
	claimed, err := s.ClaimDelivery(ctx)
	if err != nil || claimed == nil || claimed.DeliveryID != deliveryID || claimed.DeliveryPart != 1 || claimed.AttachmentID != second.ID {
		t.Fatalf("ClaimDelivery()=(%+v,%v)", claimed, err)
	}
	var content, closed int
	if err := s.db.QueryRowContext(ctx, `SELECT has_content,closed FROM personal_reply_groups WHERE id=?`, user.ID).Scan(&content, &closed); err != nil || content != 1 || closed != 1 {
		t.Fatalf("file-only result did not close reply ledger: content=%d closed=%d err=%v", content, closed, err)
	}
}

func TestSettlementRejectsUnavailableAttachmentBeforeMutation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := deliveryTestCallback(t, s)
	attachment, err := s.SaveAttachment(ctx, "missing.txt", "text/plain", strings.NewReader("stage output"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
		t.Fatal(err)
	}
	settlement := personal.TaskSettlement{Status: personal.TaskCompleted, Summary: "Findings remain pending",
		Delivery: personal.DeliveryDraft{Attachments: []string{attachment.ID}}}
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, settlement); !errors.Is(err, personal.ErrAttachmentUnavailable) {
		t.Fatalf("SettleTask unavailable attachment error=%v", err)
	}
	current, err := s.Task(ctx, task.ID)
	if err != nil || current.Status != personal.TaskAwaitingReview || current.Summary != "Stage finding" {
		t.Fatalf("failed settlement changed Task=(%+v,%v)", current, err)
	}
	var intents int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_delivery_intents WHERE event_id=?`, callback.ID).Scan(&intents); err != nil || intents != 0 {
		t.Fatalf("failed settlement created %d intents: %v", intents, err)
	}
}

func TestUnavailableSettlementAttachmentUsesVerifiedTextFallback(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := deliveryTestCallback(t, s)
	attachment, err := s.SaveAttachment(ctx, "stage.txt", "text/plain", strings.NewReader("stage output"))
	if err != nil {
		t.Fatal(err)
	}
	settlement := personal.TaskSettlement{Status: personal.TaskCompleted, Summary: "Saved findings",
		Delivery: personal.DeliveryDraft{Attachments: []string{attachment.ID}}}
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, settlement); err != nil {
		t.Fatal(err)
	}
	intents, err := s.EventDeliveries(ctx, callback.ID)
	if err != nil || len(intents) != 1 {
		t.Fatalf("EventDeliveries()=(%+v,%v)", intents, err)
	}
	if err := s.DeleteAttachment(ctx, attachment.ID); err != nil {
		t.Fatal(err)
	}
	err = s.CompleteEvent(ctx, callback.ID, personal.TurnOutput{Silent: true}, nil,
		personal.RenderedDelivery{DeliveryID: intents[0].ID})
	var unavailable *personal.DeliveryAttachmentError
	if !errors.As(err, &unavailable) || unavailable.DeliveryID != intents[0].ID || !errors.Is(err, personal.ErrAttachmentUnavailable) {
		t.Fatalf("CompleteEvent unavailable attachment error=%v", err)
	}
	var state string
	var outbox int
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, callback.ID).Scan(&state); err != nil || state != string(personal.EventRunning) {
		t.Fatalf("failed preparation changed event status=(%q,%v)", state, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, callback.ID).Scan(&outbox); err != nil || outbox != 0 {
		t.Fatalf("failed preparation partially queued %d rows: %v", outbox, err)
	}
	fallback := "The selected attachment is no longer available."
	chunks, err := channel.PrepareText(fallback)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEvent(ctx, callback.ID, personal.TurnOutput{Silent: true}, nil,
		personal.RenderedDelivery{DeliveryID: intents[0].ID, Text: fallback, Chunks: chunks, AttachmentFailure: true}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Diagnostic(ctx, callback.ID, 0)
	if err != nil || len(snapshot.DeliveryIntents) != 1 {
		t.Fatalf("Diagnostic()=(%+v,%v)", snapshot.DeliveryIntents, err)
	}
	stored := snapshot.DeliveryIntents[0]
	if stored.ID != intents[0].ID || stored.State != personal.DeliveryIntentRendered || !strings.Contains(stored.Detail, attachment.ID) || len(stored.Draft.Attachments) != 1 || stored.Draft.Attachments[0] != attachment.ID {
		t.Fatalf("fallback changed immutable intent or omitted detail: %+v", stored)
	}
	var sentText string
	var sentAttachment sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT text,attachment_id FROM personal_outbox WHERE event_id=?`, callback.ID).Scan(&sentText, &sentAttachment); err != nil || sentText != fallback || sentAttachment.Valid {
		t.Fatalf("fallback outbox=(%q,%+v,%v)", sentText, sentAttachment, err)
	}
}

func TestSettlementPreparationRecoveryKeepsIntentIdentity(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := deliveryTestCallback(t, s)
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, testTaskSettlement(personal.TaskWaitingInput, "Saved stage findings")); err != nil {
		t.Fatal(err)
	}
	intents, err := s.EventDeliveries(ctx, callback.ID)
	if err != nil || len(intents) != 1 {
		t.Fatalf("EventDeliveries()=(%+v,%v)", intents, err)
	}
	if err := s.CompleteEvent(ctx, callback.ID, personal.TurnOutput{Silent: true}, nil); err == nil {
		t.Fatal("event completed without the required prepared delivery")
	}
	var outbox int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, callback.ID).Scan(&outbox); err != nil || outbox != 0 {
		t.Fatalf("incomplete preparation wrote %d outbox rows: %v", outbox, err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.ClaimEvent(ctx)
	if err != nil || recovered == nil || recovered.ID != callback.ID {
		t.Fatalf("recovered event=(%+v,%v)", recovered, err)
	}
	again, err := s.EventDeliveries(ctx, recovered.ID)
	if err != nil || len(again) != 1 || again[0].ID != intents[0].ID {
		t.Fatalf("recovered delivery identity=(%+v,%v), original=%d", again, err, intents[0].ID)
	}
	finishEvent(t, s, *recovered, personal.TurnOutput{Silent: true}, nil)
	if err := s.CompleteEvent(ctx, callback.ID, personal.TurnOutput{Silent: true}, nil); err == nil {
		t.Fatal("already completed event was replayed")
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, callback.ID).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("recovery queued %d rows, want one: %v", outbox, err)
	}
}

func TestCallbackSettlementEndsMutationAuthorityButUserCanContinue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	_, task, run, callback := deliveryTestCallback(t, s)
	settlement := testTaskSettlement(personal.TaskWaitingInput, "Saved findings; awaiting a choice")
	original, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, settlement)
	if err != nil {
		t.Fatal(err)
	}
	var runsBefore int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE task_id=?`, task.ID).Scan(&runsBefore); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ContinueTask(ctx, callback.ID, "after-settlement", task.ID, run.Revision, "continue after final callback", false); err == nil || errors.Is(err, personal.ErrStaleTaskCallback) {
		t.Fatalf("same callback continuation error=%v, want an authorization error", err)
	}
	var runsAfter int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE task_id=?`, task.ID).Scan(&runsAfter); err != nil || runsAfter != runsBefore {
		t.Fatalf("rejected callback continuation created a Run: before=%d after=%d err=%v", runsBefore, runsAfter, err)
	}
	replayed, err := s.SettleTask(ctx, callback.ID, task.ID, run.Revision, settlement)
	if err != nil || replayed.UpdatedAt != original.UpdatedAt || replayed.Summary != original.Summary {
		t.Fatalf("same-payload settlement replay=(%+v,%v), original=(%+v)", replayed, err, original)
	}
	finishEvent(t, s, callback, personal.TurnOutput{Silent: true}, nil)
	userEvent := openUserEvent(t, s, 2, 2, "Continue the task")
	continued, nextRun, err := s.ContinueTask(ctx, userEvent.ID, "user-continue", task.ID, run.Revision, "continue from the saved findings", false)
	if err != nil || continued.Status != personal.TaskQueued || nextRun.TaskID != task.ID || nextRun.Revision != run.Revision {
		t.Fatalf("user continuation=(%+v,%+v,%v)", continued, nextRun, err)
	}
	finishEvent(t, s, userEvent, personal.TurnOutput{Silent: true}, nil)
}

func TestOrdinaryDeliveryScopesLatestTaskRevisionAndMergedTasks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "Create and revise two tasks")
	first, _, err := s.CreateTask(ctx, event.ID, "create-first", "First", personal.TaskInput{Goal: "First goal"})
	if err != nil {
		t.Fatal(err)
	}
	second, secondRun, err := s.CreateTask(ctx, event.ID, "create-second", "Second", personal.TaskInput{Goal: "Second goal"})
	if err != nil {
		t.Fatal(err)
	}
	updated, revisedRun, err := s.ReviseTask(ctx, event.ID, "revise-first", first.ID, first.Revision, personal.TaskInput{Goal: "Revised first goal"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{TaskID: updated.ID, Text: "Both task runs are queued."}, []string{"Both task runs are queued."})
	var scopes []personal.DeliveryScope
	rows, err := s.db.QueryContext(ctx, `SELECT s.task_id,s.revision,COALESCE(s.run_id,0) FROM personal_delivery_scopes s
		JOIN personal_delivery_intents d ON d.id=s.delivery_id WHERE d.event_id=? ORDER BY s.task_id`, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var scope personal.DeliveryScope
		if err := rows.Scan(&scope.TaskID, &scope.Revision, &scope.RunID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 || scopes[0] != (personal.DeliveryScope{TaskID: first.ID, Revision: updated.Revision, RunID: revisedRun.ID}) || scopes[1] != (personal.DeliveryScope{TaskID: second.ID, Revision: second.Revision, RunID: secondRun.ID}) {
		t.Fatalf("ordinary reply captured stale or missing Task scopes: %+v", scopes)
	}
}

func TestSchemaFourIsRejectedWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema4.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE personal_schema(version INTEGER PRIMARY KEY CHECK(version=4));
		INSERT INTO personal_schema(version) VALUES(4);
		CREATE TABLE personal_kept(value TEXT);
		INSERT INTO personal_kept(value) VALUES('leave unchanged')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path, time.Minute)
	if err == nil {
		store.Close()
		t.Fatal("schema4 database was accepted")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	var value string
	var deliveryTables int
	if err := db.QueryRow(`SELECT version FROM personal_schema`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT value FROM personal_kept`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('personal_delivery_intents','personal_delivery_scopes')`).Scan(&deliveryTables); err != nil {
		t.Fatal(err)
	}
	if version != 4 || value != "leave unchanged" || deliveryTables != 0 {
		t.Fatalf("rejected database changed: version=%d value=%q deliveryTables=%d", version, value, deliveryTables)
	}
}
