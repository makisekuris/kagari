package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func TestCompleteEventKeepsSupplementalDeliveryAssociation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "Stop task and check a schedule")
	task, _, err := s.CreateTask(ctx, event.ID, "create-task", "Research", personal.TaskInput{Goal: "Investigate the report"})
	if err != nil {
		t.Fatal(err)
	}
	stop, err := s.ControlDelivery(ctx, event.ID, "stop-task", personal.DeliveryControlRequest{Action: personal.DeliveryStop, TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	scheduleInput := scheduleTestInput(personal.WindowNone)
	scheduleInput.Title = "Schedule B"
	schedule, _, err := s.CreateSchedule(ctx, event.ID, "create-schedule", scheduleInput, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	intents, err := s.EventDeliveries(ctx, event.ID)
	if err != nil || len(intents) != 1 || intents[0].ID != stop.DeliveryID {
		t.Fatalf("EventDeliveries()=(%+v,%v), want the committed Task control", intents, err)
	}
	fixedChunks, err := channel.PrepareText(intents[0].Draft.Text)
	if err != nil {
		t.Fatal(err)
	}
	supplementText := "Schedule B is configured."
	supplementChunks, err := channel.PrepareText(supplementText)
	if err != nil {
		t.Fatal(err)
	}
	output := personal.TurnOutput{Silent: true, SupplementalReply: &personal.SupplementalReply{
		Draft:      personal.DeliveryDraft{Text: supplementText, Attachments: []string{}},
		ScheduleID: schedule.ID, Purpose: personal.DeliveryResult, Chunks: supplementChunks,
	}}
	if err := s.CompleteEvent(ctx, event.ID, output, nil, personal.RenderedDelivery{
		DeliveryID: intents[0].ID, Text: intents[0].Draft.Text, Chunks: fixedChunks,
	}); err != nil {
		t.Fatal(err)
	}
	var eventTask, eventSchedule sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT task_id,schedule_id FROM personal_events WHERE id=?`, event.ID).Scan(&eventTask, &eventSchedule); err != nil {
		t.Fatal(err)
	}
	if eventTask.Valid || eventSchedule.Valid {
		t.Fatalf("mixed event association task=%v schedule=%v, want no single association", eventTask, eventSchedule)
	}
	var controlHistorySchedule, supplementHistorySchedule sql.NullInt64
	var supplementID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_delivery_intents WHERE event_id=? AND purpose='result'`, event.ID).Scan(&supplementID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT schedule_id FROM personal_history WHERE delivery_id=?`, stop.DeliveryID).Scan(&controlHistorySchedule); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT schedule_id FROM personal_history WHERE delivery_id=?`, supplementID).Scan(&supplementHistorySchedule); err != nil {
		t.Fatal(err)
	}
	if controlHistorySchedule.Valid || !supplementHistorySchedule.Valid || supplementHistorySchedule.Int64 != schedule.ID {
		t.Fatalf("delivery history schedules control=%v supplemental=%v, want NULL and %d", controlHistorySchedule, supplementHistorySchedule, schedule.ID)
	}
}

func TestSuppressedStaleCallbackDoesNotAffectNewReplyGroup(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	createEvent := openUserEvent(t, s, 1, 1, "Create the task")
	task, _, err := s.CreateTask(ctx, createEvent.ID, "create", "Versioned task", personal.TaskInput{Goal: "Investigate the first version"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, createEvent, personal.TurnOutput{Silent: true}, nil)
	run, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil || run.TaskID != task.ID {
		t.Fatalf("ClaimRun()=(%+v,%v)", run, err)
	}
	if err := s.FinishStage(ctx, run.ID, personal.StageResult{Summary: "Saved old findings"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask {
		t.Fatalf("ClaimEvent()=(%+v,%v), want task callback", callback, err)
	}
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, task.Revision, personal.TaskSettlement{
		Status: personal.TaskWaitingInput, Summary: "Old revision findings",
		Delivery: personal.DeliveryDraft{Text: "Old revision reply", Attachments: []string{}},
	}); err != nil {
		t.Fatal(err)
	}

	revisionEvent := openUserEvent(t, s, 2, 2, "Update the task")
	if _, _, err := s.ReviseTask(ctx, revisionEvent.ID, "revise", task.ID, task.Revision, personal.TaskInput{Goal: "Investigate the revised version"}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, revisionEvent, personal.TurnOutput{Silent: true}, nil)
	legacyWaitFixture(t, s, revisionEvent.ID)
	var replyGroupID, waitOutboxID int64
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, revisionEvent.ID).Scan(&replyGroupID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT wait_outbox_id FROM personal_reply_groups WHERE id=?`, replyGroupID).Scan(&waitOutboxID); err != nil {
		t.Fatal(err)
	}
	var oldDeliveryID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_delivery_intents WHERE event_id=? AND purpose='result'`, callback.ID).Scan(&oldDeliveryID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEvent(ctx, callback.ID, personal.TurnOutput{Silent: true}, nil); err != nil {
		t.Fatal(err)
	}
	var hasContent, closed int
	var retainedWait int64
	if err := s.db.QueryRowContext(ctx, `SELECT has_content,closed,wait_outbox_id FROM personal_reply_groups WHERE id=?`, replyGroupID).Scan(&hasContent, &closed, &retainedWait); err != nil {
		t.Fatal(err)
	}
	var waitStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, waitOutboxID).Scan(&waitStatus); err != nil {
		t.Fatal(err)
	}
	var oldHistory int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE delivery_id=? AND role='assistant'`, oldDeliveryID).Scan(&oldHistory); err != nil {
		t.Fatal(err)
	}
	if hasContent != 0 || closed != 0 || retainedWait != waitOutboxID || waitStatus != "pending" || oldHistory != 1 {
		t.Fatalf("stale callback changed new group: content=%d closed=%d wait=%d/%d status=%q oldHistory=%d",
			hasContent, closed, retainedWait, waitOutboxID, waitStatus, oldHistory)
	}
}

func TestSuppressedSupplementalDeliveryDoesNotMarkTaskReplyGroup(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	taskAEvent := openUserEvent(t, s, 1, 1, "Create task A")
	taskA, _, err := s.CreateTask(ctx, taskAEvent.ID, "create-a", "Task A", personal.TaskInput{Goal: "Finish A"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, taskAEvent, personal.TurnOutput{Silent: true}, nil)
	taskBEvent := openUserEvent(t, s, 2, 2, "Create task B")
	taskB, _, err := s.CreateTask(ctx, taskBEvent.ID, "create-b", "Task B", personal.TaskInput{Goal: "Finish B"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, taskBEvent, personal.TurnOutput{Silent: true}, nil)
	var taskBGroupID int64
	if err := s.db.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_tasks WHERE task_id=?`, taskB.ID).Scan(&taskBGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE personal_tasks SET status='completed' WHERE id=?`, taskB.ID); err != nil {
		t.Fatal(err)
	}

	settlementEvent := openUserEvent(t, s, 3, 3, "Save task A findings")
	if _, err := s.SettleTask(ctx, settlementEvent.ID, taskA.ID, taskA.Revision, personal.TaskSettlement{
		Status: personal.TaskWaitingInput, Summary: "A findings",
		Delivery: personal.DeliveryDraft{Text: "Task A findings", Attachments: []string{}},
	}); err != nil {
		t.Fatal(err)
	}
	intents, err := s.EventDeliveries(ctx, settlementEvent.ID)
	if err != nil || len(intents) != 1 {
		t.Fatalf("EventDeliveries()=(%+v,%v), want Task A result", intents, err)
	}
	fixedChunks, err := channel.PrepareText(intents[0].Draft.Text)
	if err != nil {
		t.Fatal(err)
	}
	statusText := "A status notice about Task B."
	statusChunks, err := channel.PrepareText(statusText)
	if err != nil {
		t.Fatal(err)
	}
	output := personal.TurnOutput{Silent: true, SupplementalReply: &personal.SupplementalReply{
		Draft:   personal.DeliveryDraft{Text: statusText, Attachments: []string{}},
		TaskIDs: []int64{taskB.ID}, Purpose: personal.DeliveryStatus, Chunks: statusChunks,
	}}
	if err := s.CompleteEvent(ctx, settlementEvent.ID, output, nil, personal.RenderedDelivery{
		DeliveryID: intents[0].ID, Text: intents[0].Draft.Text, Chunks: fixedChunks,
	}); err != nil {
		t.Fatal(err)
	}
	var hasContent int
	if err := s.db.QueryRowContext(ctx, `SELECT has_content FROM personal_reply_groups WHERE id=?`, taskBGroupID).Scan(&hasContent); err != nil {
		t.Fatal(err)
	}
	var suppressedState string
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM personal_delivery_intents WHERE event_id=? AND purpose='status'`, settlementEvent.ID).Scan(&suppressedState); err != nil {
		t.Fatal(err)
	}
	if hasContent != 0 || suppressedState != string(personal.DeliveryIntentSuppressed) {
		t.Fatalf("suppressed Task B status changed ledger: content=%d intent=%q", hasContent, suppressedState)
	}
}

func TestSchemaFiveIsRejectedWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema5.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE personal_schema(version INTEGER PRIMARY KEY CHECK(version=5));
		INSERT INTO personal_schema(version) VALUES(5);
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
		t.Fatal("schema5 database was accepted")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	var value string
	var controlTables int
	if err := db.QueryRow(`SELECT version FROM personal_schema`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT value FROM personal_kept`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='personal_delivery_controls'`).Scan(&controlTables); err != nil {
		t.Fatal(err)
	}
	if version != 5 || value != "leave unchanged" || controlTables != 0 {
		t.Fatalf("rejected database changed: version=%d value=%q controlTables=%d", version, value, controlTables)
	}
}
