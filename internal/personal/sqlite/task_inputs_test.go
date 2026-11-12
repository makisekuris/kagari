package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func TestPendingInputsStopAtNonURLAndPreserveSourceIsolation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	current := openUserEvent(t, s, 1, 101, "Research this page")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO personal_events(kind,text,channel,account,sender,message_id,attachments,status,created_at)
		VALUES('user','https://example.com/other-owner','telegram','test-bot','other-owner','107','[]','queued',?)`, timestamp(time.Now())); err != nil {
		t.Fatal(err)
	}
	urlID := acceptInput(t, s, "2", sourceInput("102", "https://example.com/article"))
	acceptInput(t, s, "3", sourceInput("103", "Read https://example.com/mixed"))
	afterInstructionID := acceptInput(t, s, "4", sourceInput("104", "https://example.com/after-instruction"))
	acceptInput(t, s, "5", sourceInput("105", "/continue_task https://example.com/command"))
	forwarded := sourceInput("106", "https://example.com/forwarded")
	forwarded.Material = true
	acceptInput(t, s, "6", forwarded)
	withAttachment := sourceInput("107", "https://example.com/attachment")
	withAttachment.Attachments = []channel.Attachment{{Name: "forwarded.txt", Error: "attachment unavailable"}}
	acceptInput(t, s, "7", withAttachment)
	pending, err := s.PendingInputs(ctx, current.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != urlID || pending[0].Text != "https://example.com/article" {
		t.Fatalf("PendingInputs()=(%+v,%v), want URLs only through the first non-URL message", pending, err)
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, afterInstructionID).Scan(&status); err != nil || status != string(personal.EventQueued) {
		t.Fatalf("URL after intervening text status=(%q,%v), want queued", status, err)
	}
	if _, err := s.PendingInputs(ctx, urlID); err == nil {
		t.Fatal("PendingInputs accepted a queued event instead of the current running user event")
	}
}

func TestPendingInputsDoNotCrossInterveningUserMessageOrAttachment(t *testing.T) {
	for _, blocker := range []struct {
		name           string
		text           string
		withAttachment bool
	}{
		{name: "user instruction", text: "下一条链接先不要读取，等我确认。"},
		{name: "attachment", text: "https://example.com/attached", withAttachment: true},
	} {
		t.Run(blocker.name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Hour)
			current := openUserEvent(t, s, 1, 101, "https://example.com/first")
			middle := sourceInput("102", blocker.text)
			if blocker.withAttachment {
				middle.Attachments = []channel.Attachment{{Name: "forwarded.txt", Error: "attachment unavailable"}}
			}
			middleID := acceptInput(t, s, "2", middle)
			secondURLID := acceptInput(t, s, "3", sourceInput("103", "https://example.com/second"))
			task, _ := createTask(t, s, current, "create-before-instruction", "Read first link", "Read the first link", "")

			pending, err := s.PendingInputs(ctx, current.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, addErr := s.AddTaskInputs(ctx, current.ID, "cross-instruction", task.ID, task.Revision, []int64{current.ID, secondURLID})
			if len(pending) != 0 {
				t.Errorf("PendingInputs() crossed the intervening %s: %+v", blocker.name, pending)
			}
			if addErr == nil {
				t.Errorf("AddTaskInputs accepted a later URL across the intervening %s", blocker.name)
			}
			for _, id := range []int64{middleID, secondURLID} {
				var status string
				if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, id).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != string(personal.EventQueued) {
					t.Errorf("event %d status=%q, want queued", id, status)
				}
			}
		})
	}
}

func TestAddTaskInputsInitialRunSnapshotIdempotencyAndMemory(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	current := openUserEvent(t, s, 10, 110, "Please compare the two sources.")
	firstURL := acceptInput(t, s, "11", sourceInput("111", "https://example.com/one"))
	secondURL := acceptInput(t, s, "12", sourceInput("112", "https://example.org/two"))
	task, originalRun := createTask(t, s, current, "create-task-inputs", "Compare sources", "Compare the referenced sources", "Base constraints")
	updated, err := s.AddTaskInputs(ctx, current.ID, "add-inputs", task.ID, task.Revision, []int64{current.ID, firstURL, secondURL})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Goal != task.Goal || updated.Revision != task.Revision || updated.Context != task.Context {
		t.Fatalf("adding inputs changed the task definition: before=%+v after=%+v", task, updated)
	}
	initial, err := s.Run(ctx, originalRun.ID)
	if err != nil || initial == nil || initial.Status != personal.RunQueued || initial.EventID != current.ID {
		t.Fatalf("queued initial run changed identity/state: (%+v,%v)", initial, err)
	}
	var input personal.TaskInput
	if err := json.Unmarshal([]byte(initial.Input), &input); err != nil {
		t.Fatal(err)
	}
	if input.Context != task.Context || input.Goal != task.Goal {
		t.Fatalf("initial run snapshot=%+v", input)
	}
	if len(input.InputEvents) != 3 {
		t.Fatalf("initial Run input references=%+v, want current event and two selected inputs", input.InputEvents)
	}
	for i, want := range []struct {
		id   int64
		kind string
		text string
	}{{current.ID, "feedback", current.Text}, {firstURL, "material", "https://example.com/one"}, {secondURL, "material", "https://example.org/two"}} {
		ref := input.InputEvents[i]
		if ref.EventID != want.id || ref.Kind != want.kind || ref.Order != i || ref.TextRunes != utf8.RuneCountInString(want.text) || ref.TextPreview != want.text {
			t.Fatalf("input reference %d=%+v, want id=%d kind=%q text=%q", i, ref, want.id, want.kind, want.text)
		}
	}
	for _, fragment := range []string{current.Text, "https://example.com/one", "https://example.org/two"} {
		if strings.Contains(input.Context, fragment) {
			t.Fatalf("Run context copied source body %q: %s", fragment, input.Context)
		}
	}
	if len(input.Attachments) != 0 {
		t.Fatalf("unexpected task attachments: %+v", input.Attachments)
	}
	for _, id := range []int64{firstURL, secondURL} {
		var status string
		var boundTask sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT status,task_id FROM personal_events WHERE id=?`, id).Scan(&status, &boundTask); err != nil || status != string(personal.EventCompleted) || !boundTask.Valid || boundTask.Int64 != task.ID {
			t.Fatalf("selected event %d state=(%q,%+v,%v)", id, status, boundTask, err)
		}
		var reviews int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE event_id=? AND kind='memory_review'`, id).Scan(&reviews); err != nil || reviews != 1 {
			t.Fatalf("selected event %d memory review count=(%d,%v)", id, reviews, err)
		}
		var inputStatus string
		var revision int64
		var assignedRun sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT status,revision,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, id).
			Scan(&inputStatus, &revision, &assignedRun); err != nil || inputStatus != "assigned" || revision != task.Revision || !assignedRun.Valid || assignedRun.Int64 != originalRun.ID {
			t.Fatalf("selected input %d assignment=(%q,%d,%+v,%v)", id, inputStatus, revision, assignedRun, err)
		}
	}
	var currentStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, current.ID).Scan(&currentStatus); err != nil || currentStatus != string(personal.EventRunning) {
		t.Fatalf("current event status=(%q,%v), want running", currentStatus, err)
	}
	var currentReviews int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE event_id=? AND kind='memory_review'`, current.ID).Scan(&currentReviews); err != nil || currentReviews != 0 {
		t.Fatalf("running current event review count=(%d,%v), want 0", currentReviews, err)
	}
	var requestTaskID, requestRunID int64
	var operation, payload string
	if err := s.db.QueryRowContext(ctx, `SELECT operation,payload,task_id,run_id FROM personal_requests WHERE event_id=? AND request_key=?`, current.ID, "add-inputs").
		Scan(&operation, &payload, &requestTaskID, &requestRunID); err != nil || operation != "add_task_input" || requestTaskID != task.ID || requestRunID != originalRun.ID {
		t.Fatalf("request receipt=(%q,%q,%d,%d,%v)", operation, payload, requestTaskID, requestRunID, err)
	}
	var request taskInputRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil || request.TaskID != task.ID || request.ExpectedRevision != task.Revision ||
		len(request.InputEventIDs) != 3 || request.InputEventIDs[1] != firstURL || request.InputEventIDs[2] != secondURL {
		t.Fatalf("request payload=(%+v,%v)", request, err)
	}
	var inputCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_task_inputs WHERE task_id=?`, task.ID).Scan(&inputCount); err != nil || inputCount != 3 {
		t.Fatalf("input association count=(%d,%v)", inputCount, err)
	}

	finishEvent(t, s, current, personal.TurnOutput{TaskID: task.ID, Text: "Queued"}, []string{"Queued"})
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE event_id=? AND kind='memory_review'`, current.ID).Scan(&currentReviews); err != nil || currentReviews != 1 {
		t.Fatalf("completed current event review count=(%d,%v), want 1", currentReviews, err)
	}
	replayed, err := s.AddTaskInputs(ctx, current.ID, "add-inputs", task.ID, task.Revision, []int64{current.ID, firstURL, secondURL})
	if err != nil || replayed.ID != task.ID {
		t.Fatalf("completed-source idempotent replay=(%+v,%v)", replayed, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_task_inputs WHERE task_id=?`, task.ID).Scan(&inputCount); err != nil || inputCount != 3 {
		t.Fatalf("replay duplicated task inputs: (%d,%v)", inputCount, err)
	}
	if _, err := s.AddTaskInputs(ctx, current.ID, "add-inputs", task.ID, task.Revision, []int64{current.ID, firstURL}); err == nil {
		t.Fatal("request key replay with a different payload was accepted")
	}
	if _, err := s.AddTaskInputs(ctx, current.ID, "new-after-completion", task.ID, task.Revision, []int64{current.ID}); err == nil {
		t.Fatal("completed source event authorized a new input operation")
	}
}

func TestAddTaskInputsRejectsIneligibleSourcesAndTerminalTasks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	current := openUserEvent(t, s, 20, 120, "Use these references for the same goal")
	commandID := acceptInput(t, s, "21", sourceInput("121", "/cancel_task https://example.com"))
	forwarded := sourceInput("122", "https://example.net/forwarded")
	forwarded.Material = true
	forwardedID := acceptInput(t, s, "22", forwarded)
	otherUserID := int64(0)
	result, err := s.db.ExecContext(ctx, `INSERT INTO personal_events(kind,text,channel,account,sender,message_id,attachments,status,created_at)
		VALUES('user','https://example.org/other-user','telegram','test-bot','someone-else','123','[]','queued',?)`, timestamp(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	otherUserID, err = result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	task, _ := createTask(t, s, current, "create-ineligible", "Research", "Research this", "")
	for i, badID := range []int64{commandID, forwardedID, otherUserID} {
		if _, err := s.AddTaskInputs(ctx, current.ID, fmt.Sprintf("reject-%d", i), task.ID, task.Revision, []int64{current.ID, badID}); err == nil {
			t.Fatalf("ineligible event %d was consumed", badID)
		}
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, badID).Scan(&status); err != nil || status != string(personal.EventQueued) {
			t.Fatalf("rejected event %d state=(%q,%v)", badID, status, err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_task_inputs WHERE task_id=?`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed validation left input associations=(%d,%v)", count, err)
	}
	if _, err := s.AddTaskInputs(ctx, current.ID, "stale", task.ID, task.Revision+1, []int64{current.ID}); err == nil {
		t.Fatal("stale task revision was accepted")
	}
	if _, err := s.CancelTask(ctx, current.ID, task.ID, task.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskInputs(ctx, current.ID, "cancelled", task.ID, task.Revision, []int64{current.ID}); err == nil {
		t.Fatal("cancelled task accepted additional input")
	}
	completed, _, err := s.CreateTask(ctx, current.ID, "create-completed", "Complete", personal.TaskInput{Goal: "Already complete"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleTask(ctx, current.ID, completed.ID, completed.Revision, personal.TaskCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskInputs(ctx, current.ID, "completed", completed.ID, completed.Revision, []int64{current.ID}); err == nil {
		t.Fatal("completed task accepted additional input")
	}
}

func TestAddTaskInputsRebuildsQueuedRunFromAllAssociatedMessages(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	current := openUserEvent(t, s, 25, 125, "Use both links for the same comparison")
	firstURL := acceptInput(t, s, "26", sourceInput("126", "https://example.com/first"))
	secondURL := acceptInput(t, s, "27", sourceInput("127", "https://example.org/second"))
	task, run := createTask(t, s, current, "create-repeated-input", "Compare links", "Compare these references", "Original context")
	if _, err := s.AddTaskInputs(ctx, current.ID, "add-first-input", task.ID, task.Revision, []int64{current.ID, firstURL}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskInputs(ctx, current.ID, "add-second-input", task.ID, task.Revision, []int64{current.ID, secondURL}); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Run(ctx, run.ID)
	if err != nil || updated == nil || updated.ID != run.ID || updated.Status != personal.RunQueued {
		t.Fatalf("updated initial run=(%+v,%v)", updated, err)
	}
	var input personal.TaskInput
	if err := json.Unmarshal([]byte(updated.Input), &input); err != nil {
		t.Fatal(err)
	}
	if input.Context != task.Context || len(input.InputEvents) != 3 || input.InputEvents[1].EventID != firstURL || input.InputEvents[2].EventID != secondURL {
		t.Fatalf("repeated additions duplicated or lost source references: %+v", input)
	}
	for _, fragment := range []string{current.Text, "https://example.com/first", "https://example.org/second"} {
		if strings.Contains(input.Context, fragment) {
			t.Fatalf("queued Run context copied source body %q: %s", fragment, input.Context)
		}
	}
	var firstRunID, secondRunID sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, firstURL).Scan(&firstRunID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, secondURL).Scan(&secondRunID); err != nil {
		t.Fatal(err)
	}
	if !firstRunID.Valid || !secondRunID.Valid || firstRunID.Int64 != run.ID || secondRunID.Int64 != run.ID {
		t.Fatalf("queued input run bindings=(%+v,%+v), want %d", firstRunID, secondRunID, run.ID)
	}
}

func TestTaskInputRunSnapshotReusesAndDeduplicatesAttachments(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "personal.db"), time.Hour)
	attachment, err := s.SaveAttachment(ctx, "source.txt", "text/plain", strings.NewReader("source contents"))
	if err != nil {
		t.Fatal(err)
	}
	input := sourceInput("128", "Use the attached source")
	input.Attachments = []channel.Attachment{attachment}
	id := acceptInput(t, s, "28", input)
	event, err := s.ClaimEvent(ctx)
	if err != nil || event == nil || event.ID != id {
		t.Fatalf("ClaimEvent()=(%+v,%v)", event, err)
	}
	task, run, err := s.CreateTask(ctx, event.ID, "create-attachment-task", "Review source", personal.TaskInput{
		Goal: "Review the attached source", Attachments: []channel.Attachment{attachment},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskInputs(ctx, event.ID, "add-attachment-source", task.ID, task.Revision, []int64{event.ID}); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Run(ctx, run.ID)
	if err != nil || updated == nil {
		t.Fatalf("Run()=(%+v,%v)", updated, err)
	}
	var runInput personal.TaskInput
	if err := json.Unmarshal([]byte(updated.Input), &runInput); err != nil {
		t.Fatal(err)
	}
	if len(runInput.Attachments) != 1 || runInput.Attachments[0].ID != attachment.ID {
		t.Fatalf("attachment refs were not reused and deduplicated: %+v", runInput.Attachments)
	}
}

func TestAddTaskInputsLeavesActiveRunAndFeedsNextStage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	firstEvent := openUserEvent(t, s, 30, 130, "Compare current information")
	task, firstRun := createTask(t, s, firstEvent, "create-active-input", "Compare sources", "Compare the sources", "Keep the original scope")
	finishEvent(t, s, firstEvent, personal.TurnOutput{TaskID: task.ID, Text: "Queued"}, []string{"Queued"})
	active, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || active == nil || active.ID != firstRun.ID || active.Status != personal.RunRunning {
		t.Fatalf("first run claim=(%+v,%v)", active, err)
	}
	originalInput := active.Input
	current := openUserEvent(t, s, 31, 131, "Add this clarification to the comparison")
	urlID := acceptInput(t, s, "32", sourceInput("132", "https://example.com/new-source"))
	if _, err := s.AddTaskInputs(ctx, current.ID, "append-active", task.ID, task.Revision, []int64{current.ID, urlID}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.Run(ctx, active.ID)
	if err != nil || unchanged == nil || unchanged.Input != originalInput || unchanged.Status != personal.RunRunning {
		t.Fatalf("active run changed after new input: (%+v,%v)", unchanged, err)
	}
	pending, err := s.PendingTaskInputs(ctx, task.ID)
	if err != nil || len(pending) != 2 || pending[0].ID != current.ID || pending[1].ID != urlID {
		t.Fatalf("pending task inputs=(%+v,%v)", pending, err)
	}
	if err := s.FinishStage(ctx, active.ID, personal.StageResult{Summary: "Initial comparison"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask || callback.TaskID != task.ID {
		t.Fatalf("task callback=(%+v,%v)", callback, err)
	}
	if _, err := s.SettleTask(ctx, callback.ID, task.ID, task.Revision, personal.TaskCompleted, "old result is complete"); err == nil {
		t.Fatal("task with not-yet-included inputs was completed")
	}
	_, nextRun, err := s.ContinueTask(ctx, callback.ID, "continue-with-input", task.ID, task.Revision, "Use the newly authorized source", false)
	if err != nil {
		t.Fatal(err)
	}
	var nextInput personal.TaskInput
	if err := json.Unmarshal([]byte(nextRun.Input), &nextInput); err != nil {
		t.Fatal(err)
	}
	if nextInput.Context != task.Context || len(nextInput.InputEvents) != 2 || nextInput.InputEvents[0].EventID != current.ID || nextInput.InputEvents[1].EventID != urlID {
		t.Fatalf("next Run did not preserve bounded input references: %+v", nextInput)
	}
	for _, fragment := range []string{current.Text, "https://example.com/new-source"} {
		if strings.Contains(nextInput.Context, fragment) {
			t.Fatalf("next Run context copied source body %q: %s", fragment, nextInput.Context)
		}
	}
	if nextRun.ID == active.ID || nextRun.Status != personal.RunQueued {
		t.Fatalf("next run=%+v", nextRun)
	}
	if pending, err := s.PendingTaskInputs(ctx, task.ID); err != nil || len(pending) != 2 {
		t.Fatalf("assigned inputs should remain unprocessed until reported=(%+v,%v)", pending, err)
	}
	var includedRunID sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, urlID).Scan(&includedRunID); err != nil || !includedRunID.Valid || includedRunID.Int64 != nextRun.ID {
		t.Fatalf("input run association=(%+v,%v), want run %d", includedRunID, err, nextRun.ID)
	}
	finishEvent(t, s, *callback, personal.TurnOutput{TaskID: task.ID, Silent: true}, nil)
	claimedNext, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimedNext == nil || claimedNext.ID != nextRun.ID {
		t.Fatalf("next run claim=(%+v,%v)", claimedNext, err)
	}
}

func TestTaskInputRecoveryReassignsOnlyFrozenRunReferences(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	event, task, run, originalInputIDs := createActiveTaskWithInputs(t, s, 60, "Initial request", "https://example.com/initial")
	activeBefore, err := s.Run(ctx, run.ID)
	if err != nil || activeBefore == nil {
		t.Fatalf("active run before recovery=(%+v,%v)", activeBefore, err)
	}
	frozenInput := activeBefore.Input

	lateEvent := openUserEventDirect(t, s, 63, 163, "Add this later note")
	lateURL := acceptInput(t, s, "64", sourceInput("164", "https://example.net/later"))
	if _, err := s.AddTaskInputs(ctx, lateEvent.ID, "append-later", task.ID, task.Revision, []int64{lateEvent.ID, lateURL}); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range originalInputIDs {
		var status string
		var assignedRun sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT status,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, id).
			Scan(&status, &assignedRun); err != nil || status != "pending" || assignedRun.Valid {
			t.Fatalf("interrupted input %d after recovery=(%q,%+v,%v), want pending and unassigned", id, status, assignedRun, err)
		}
	}
	for _, id := range []int64{lateEvent.ID, lateURL} {
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, id).Scan(&status); err != nil || status != "pending" {
			t.Fatalf("input arriving after frozen run %d status=(%q,%v), want pending", id, status, err)
		}
	}

	reclaimed, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || reclaimed == nil || reclaimed.ID != run.ID || reclaimed.Input != frozenInput {
		t.Fatalf("recovered run=(%+v,%v), want same run with frozen input", reclaimed, err)
	}
	var input personal.TaskInput
	if err := json.Unmarshal([]byte(reclaimed.Input), &input); err != nil {
		t.Fatal(err)
	}
	if len(input.InputEvents) != 2 || input.InputEvents[0].EventID != originalInputIDs[0] || input.InputEvents[1].EventID != originalInputIDs[1] {
		t.Fatalf("recovered frozen refs=%+v", input.InputEvents)
	}
	for _, id := range originalInputIDs {
		var status string
		var assignedRun sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT status,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, id).
			Scan(&status, &assignedRun); err != nil || status != "assigned" || !assignedRun.Valid || assignedRun.Int64 != run.ID {
			t.Fatalf("reclaimed input %d assignment=(%q,%+v,%v)", id, status, assignedRun, err)
		}
	}
	for _, id := range []int64{lateEvent.ID, lateURL} {
		var status string
		var assignedRun sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT status,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, id).
			Scan(&status, &assignedRun); err != nil || status != "pending" || assignedRun.Valid {
			t.Fatalf("late input %d was absorbed by frozen run=(%q,%+v,%v)", id, status, assignedRun, err)
		}
	}
	if _, err := s.ReadTaskInput(ctx, reclaimed.ID, lateEvent.ID); err == nil {
		t.Fatal("recovered Run read a later input outside its frozen references")
	}
	if event.ID != run.EventID {
		t.Fatalf("test setup event %d does not own run %d", event.ID, run.ID)
	}
}

func TestTaskInputRevisionChangeRebasesAssignments(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Hour)
	event, task, oldRun, ids := createActiveTaskWithInputs(t, s, 70, "Original request", "https://example.com/original")
	if err := s.FinishStage(ctx, oldRun.ID, personal.StageResult{}, "failed stage"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, ids[0]).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("failed input status=(%q,%v), want pending", status, err)
	}
	editEvent := openUserEventDirect(t, s, 73, 173, "Change the task scope")
	revised, newRun, err := s.ReviseTask(ctx, editEvent.ID, "revise-after-failure", task.ID, task.Revision, personal.TaskInput{
		Goal: "Revised task goal", Context: "Revised context",
	})
	if err != nil {
		t.Fatal(err)
	}
	if revised.Revision != task.Revision+1 || newRun.Revision != revised.Revision {
		t.Fatalf("revised task/run=(%+v,%+v)", revised, newRun)
	}
	finishEvent(t, s, editEvent, personal.TurnOutput{TaskID: revised.ID}, nil)
	oldStored, err := s.Run(ctx, oldRun.ID)
	if err != nil || oldStored == nil || oldStored.Status != personal.RunFailed {
		t.Fatalf("old run after revision=(%+v,%v)", oldStored, err)
	}
	for _, id := range ids {
		var inputRevision int64
		var inputStatus string
		var assignedRun sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT revision,status,run_id FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, id).
			Scan(&inputRevision, &inputStatus, &assignedRun); err != nil || inputRevision != revised.Revision || inputStatus != "assigned" || !assignedRun.Valid || assignedRun.Int64 != newRun.ID {
			t.Fatalf("rebased input %d=(%d,%q,%+v,%v)", id, inputRevision, inputStatus, assignedRun, err)
		}
	}
	if _, err := s.ReadTaskInput(ctx, oldRun.ID, ids[0]); err == nil {
		t.Fatal("stale Run read an input after revision changed")
	}
	claimed, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimed == nil || claimed.ID != newRun.ID {
		t.Fatalf("revised Run claim=(%+v,%v)", claimed, err)
	}
	if _, err := s.ReadTaskInput(ctx, claimed.ID, ids[0]); err != nil {
		t.Fatalf("current revised Run could not read rebased input: %v", err)
	}
	if event.ID != oldRun.EventID {
		t.Fatalf("test setup event %d does not own old run %d", event.ID, oldRun.ID)
	}
}

func createActiveTaskWithInputs(t *testing.T, s *Store, updateID int64, text, link string) (personal.Event, personal.Task, personal.Run, []int64) {
	t.Helper()
	ctx := context.Background()
	event := openUserEvent(t, s, updateID, updateID+100, text)
	linkID := acceptInput(t, s, fmt.Sprint(updateID+1), sourceInput(fmt.Sprint(updateID+101), link))
	task, initialRun := createTask(t, s, event, fmt.Sprintf("create-input-task-%d", updateID), "Input test", "Handle selected inputs", "")
	if _, err := s.AddTaskInputs(ctx, event.ID, fmt.Sprintf("add-inputs-%d", updateID), task.ID, task.Revision, []int64{event.ID, linkID}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{TaskID: task.ID}, nil)
	active, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || active == nil || active.ID != initialRun.ID {
		t.Fatalf("input test run claim=(%+v,%v)", active, err)
	}
	return event, task, *active, []int64{event.ID, linkID}
}

func openUserEventDirect(t *testing.T, s *Store, updateID, messageID int64, text string) personal.Event {
	t.Helper()
	ctx := context.Background()
	id := acceptInput(t, s, fmt.Sprint(updateID), sourceInput(fmt.Sprint(messageID), text))
	res, err := s.db.ExecContext(ctx, `UPDATE personal_events SET status='running' WHERE id=? AND status='queued'`, id)
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := res.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("open direct event %d rows=(%d,%v)", id, affected, err)
	}
	event, err := scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM personal_events WHERE id=?`, id))
	if err != nil {
		t.Fatal(err)
	}
	return *event
}
