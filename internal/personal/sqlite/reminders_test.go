package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func reminderTestInput(start time.Time, repeat bool) personal.ReminderInput {
	return personal.ReminderInput{
		Title:   "Hydration",
		Repeat:  repeat,
		StartAt: &start,
		Steps: []personal.ReminderStep{
			{AfterMinutes: 30, Text: "Drink water"},
			{AfterMinutes: 10, Text: "Stand up"},
		},
	}
}

func TestReminderFiniteRelativeStepsOutboxAndReplay(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	start := time.Date(2026, 11, 6, 10, 0, 30, 0, time.UTC)
	event := openUserEvent(t, s, 1, 1, "Remind me to hydrate")
	input := reminderTestInput(start, false)
	created, replayed, err := s.CreateReminder(ctx, event.ID, "create", input, start)
	if err != nil || replayed {
		t.Fatalf("CreateReminder() = (%+v,%v,%v)", created, replayed, err)
	}
	if created.Status != "active" || created.NextStep != 0 || created.NextRunAt == nil || !created.NextRunAt.Equal(start.Add(30*time.Minute)) {
		t.Fatalf("created reminder = %+v", created)
	}
	if sent, err := s.DispatchDueReminder(ctx, start.Add(31*time.Minute)); err != nil || sent {
		t.Fatalf("uncompleted source event dispatch = (%t,%v), want false", sent, err)
	}
	var initialOutbox int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox`).Scan(&initialOutbox); err != nil || initialOutbox != 0 {
		t.Fatalf("outbox rows before the source event completes = (%d,%v), want 0", initialOutbox, err)
	}
	finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)

	for _, due := range []struct {
		at   time.Time
		text string
	}{
		{start.Add(30 * time.Minute), "Drink water"},
		{start.Add(40 * time.Minute), "Stand up"},
	} {
		if sent, err := s.DispatchDueReminder(ctx, due.at); err != nil || !sent {
			t.Fatalf("DispatchDueReminder(%s) = (%t,%v)", due.at, sent, err)
		}
		var text string
		if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_outbox ORDER BY id DESC LIMIT 1`).Scan(&text); err != nil || text != due.text {
			t.Fatalf("last outbox text = %q, %v; want %q", text, err, due.text)
		}
	}
	if sent, err := s.DispatchDueReminder(ctx, start.Add(24*time.Hour)); err != nil || sent {
		t.Fatalf("completed reminder dispatched again = (%t,%v)", sent, err)
	}

	listed, err := s.Reminders(ctx, false, 0, 0)
	if err != nil || len(listed) != 1 || listed[0].Status != "completed" || listed[0].NextRunAt != nil {
		t.Fatalf("Reminders() = (%+v,%v)", listed, err)
	}
	var outboxCount, historyCount, scopeCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=? AND role='assistant'`, event.ID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_delivery_scopes sc JOIN personal_delivery_intents d ON d.id=sc.delivery_id WHERE d.event_id=?`, event.ID).Scan(&scopeCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 2 || historyCount != 2 || scopeCount != 0 {
		t.Fatalf("reminder delivery rows: outbox=%d history=%d task scopes=%d", outboxCount, historyCount, scopeCount)
	}
	var firstStatus, secondStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE event_id=? ORDER BY part LIMIT 1`, event.ID).Scan(&firstStatus); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE event_id=? ORDER BY part DESC LIMIT 1`, event.ID).Scan(&secondStatus); err != nil {
		t.Fatal(err)
	}
	if firstStatus != "suppressed" || secondStatus != "pending" {
		t.Fatalf("offline reminder backlog statuses = (%q,%q), want only the latest pending", firstStatus, secondStatus)
	}

	replayedReminder, replayed, err := s.CreateReminder(ctx, event.ID, "create", input, start.Add(time.Hour))
	if err != nil || !replayed || !reflect.DeepEqual(replayedReminder, created) {
		t.Fatalf("CreateReminder replay = (%+v,%v,%v), want saved creation snapshot", replayedReminder, replayed, err)
	}
	if _, _, err := s.CreateReminder(ctx, event.ID, "create", reminderTestInput(start.Add(time.Minute), false), start); err == nil {
		t.Fatal("reused create key with different payload succeeded")
	}
}

func TestReminderRepeatingCoalescesToLatestOriginalOccurrence(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	start := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	event := openUserEvent(t, s, 2, 2, "Repeat a 30 plus 10 minute reminder")
	input := reminderTestInput(start, true)
	reminder, _, err := s.CreateReminder(ctx, event.ID, "create-repeat", input, start)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
	now := time.Date(2026, 11, 6, 12, 2, 0, 0, time.UTC)
	if sent, err := s.DispatchDueReminder(ctx, now); err != nil || !sent {
		t.Fatalf("delayed DispatchDueReminder() = (%t,%v)", sent, err)
	}
	var text string
	if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_outbox`).Scan(&text); err != nil || text != "Stand up" {
		t.Fatalf("coalesced text = %q, %v; want latest scheduled stage", text, err)
	}
	active, err := s.Reminders(ctx, true, 0, 0)
	if err != nil || len(active) != 1 || active[0].ID != reminder.ID || active[0].NextStep != 0 || active[0].NextRunAt == nil || !active[0].NextRunAt.Equal(start.Add(2*time.Hour+30*time.Minute)) {
		t.Fatalf("reminder progress after catch-up = (%+v,%v), want stage 1 at original 12:30", active, err)
	}
	if sent, err := s.DispatchDueReminder(ctx, now); err != nil || sent {
		t.Fatalf("same dispatch time enqueued a duplicate = (%t,%v)", sent, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("coalesced outbox rows = (%d,%v), want one", count, err)
	}
}

func TestReminderCancellationPreservesInFlightDelivery(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	event := openUserEvent(t, s, 20_001, 20_001, "Remind me soon")
	input := personal.ReminderInput{Title: "Soon", StartAt: &start, Steps: []personal.ReminderStep{{AfterMinutes: 1, Text: "Ready"}}}
	reminder, _, err := s.CreateReminder(ctx, event.ID, "create", input, start)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
	if sent, err := s.DispatchDueReminder(ctx, *reminder.NextRunAt); err != nil || !sent {
		t.Fatalf("DispatchDueReminder() = (%t,%v)", sent, err)
	}
	outgoing, err := s.ClaimDelivery(ctx)
	if err != nil || outgoing == nil {
		t.Fatalf("ClaimDelivery() = (%+v,%v)", outgoing, err)
	}
	cancelEvent := openUserEvent(t, s, 20_002, 20_002, "Cancel that reminder")
	if _, _, err := s.CancelReminder(ctx, cancelEvent.ID, "cancel", reminder.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, outgoing.ID).Scan(&status); err != nil || status != "sending" {
		t.Fatalf("in-flight reminder delivery status = (%q,%v), want sending", status, err)
	}
	if err := s.UpdateDelivery(ctx, outgoing.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "sent-reminder"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledInFlightReminderRetryIsSuppressed(t *testing.T) {
	for _, test := range []struct {
		name       string
		stepCount  int
		result     string
		wantStatus string
		wantOutbox string
	}{
		{name: "active pending retry", stepCount: 2, result: "pending", wantStatus: "cancelled", wantOutbox: "suppressed"},
		{name: "active waiting retry", stepCount: 2, result: "waiting", wantStatus: "cancelled", wantOutbox: "suppressed"},
		{name: "completed pending retry", stepCount: 1, result: "pending", wantStatus: "completed", wantOutbox: "suppressed"},
		{name: "completed waiting retry", stepCount: 1, result: "waiting", wantStatus: "completed", wantOutbox: "suppressed"},
		{name: "completed uncertain result", stepCount: 1, result: "uncertain", wantStatus: "completed", wantOutbox: "uncertain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			start := time.Now().UTC().Truncate(time.Second).Add(-62 * time.Second)
			event := openUserEvent(t, s, 21_000+int64(test.stepCount), 21_000+int64(test.stepCount), "Remind me soon")
			steps := []personal.ReminderStep{{AfterMinutes: 1, Text: "First"}}
			if test.stepCount == 2 {
				steps = append(steps, personal.ReminderStep{AfterMinutes: 1, Text: "Second"})
			}
			reminder, _, err := s.CreateReminder(ctx, event.ID, "create", personal.ReminderInput{Title: "Soon", StartAt: &start, Steps: steps}, start)
			if err != nil {
				t.Fatal(err)
			}
			finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
			if dispatched, err := s.DispatchDueReminder(ctx, time.Now().UTC()); err != nil || !dispatched {
				t.Fatalf("DispatchDueReminder() = (%t,%v)", dispatched, err)
			}
			outgoing, err := s.ClaimDelivery(ctx)
			if err != nil || outgoing == nil {
				t.Fatalf("ClaimDelivery() = (%+v,%v)", outgoing, err)
			}
			cancelEvent := openUserEvent(t, s, 21_100+int64(test.stepCount), 21_100+int64(test.stepCount), "Cancel that reminder")
			cancelled, _, err := s.CancelReminder(ctx, cancelEvent.ID, "cancel", reminder.ID, time.Now().UTC())
			if err != nil || cancelled.Status != test.wantStatus {
				t.Fatalf("CancelReminder() = (%+v,%v), want status %q", cancelled, err, test.wantStatus)
			}
			if err := s.UpdateDelivery(ctx, outgoing.ID, test.result, channel.Receipt{}, "temporary send error", time.Now().UTC().Add(-time.Second)); err != nil {
				t.Fatalf("UpdateDelivery(%q) = %v", test.result, err)
			}
			var status, intentState string
			if err := s.db.QueryRowContext(ctx, `SELECT o.status,i.state FROM personal_outbox o JOIN personal_delivery_intents i ON i.id=o.delivery_id WHERE o.id=?`, outgoing.ID).Scan(&status, &intentState); err != nil {
				t.Fatal(err)
			}
			if status != test.wantOutbox || intentState != string(personal.DeliveryIntentSuppressed) {
				t.Fatalf("cancelled retry states = (%q,%q), want (%q,suppressed)", status, intentState, test.wantOutbox)
			}
			if next, err := s.ClaimDelivery(ctx); err != nil || next != nil {
				t.Fatalf("ClaimDelivery after cancellation = (%+v,%v), want no retry", next, err)
			}
		})
	}
}

func TestReminderDispatchPersistsAcrossStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reminder-restart.sqlite")
	s, err := NewStore(path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.BindChannel(ctx, "telegram", "test-bot", "test-owner", false); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Second)
	event := openUserEvent(t, s, 30_001, 30_001, "Remind me once")
	input := personal.ReminderInput{Title: "Once", StartAt: &start, Steps: []personal.ReminderStep{{AfterMinutes: 1, Text: "Once"}}}
	reminder, _, err := s.CreateReminder(ctx, event.ID, "create", input, start)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
	due := *reminder.NextRunAt
	if dispatched, err := s.DispatchDueReminder(ctx, due); err != nil || !dispatched {
		t.Fatalf("first DispatchDueReminder() = (%t,%v)", dispatched, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(path, time.Minute)
	if err != nil {
		t.Fatalf("reopen reminder database: %v", err)
	}
	if dispatched, err := s.DispatchDueReminder(ctx, due); err != nil || dispatched {
		t.Fatalf("restarted DispatchDueReminder() = (%t,%v), want no duplicate", dispatched, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("persisted reminder outbox count = (%d,%v), want one", count, err)
	}
}

func TestClaimDeliveryWaitsForReminderCoalescingAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reminder-sender-restart.sqlite")
	s := testStore(t, path, time.Minute)
	start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	reminderEvent := openUserEvent(t, s, 31_001, 31_001, "Remind me twice")
	input := personal.ReminderInput{Title: "Two step", StartAt: &start, Steps: []personal.ReminderStep{
		{AfterMinutes: 1, Text: "stale first step"},
		{AfterMinutes: 1, Text: "latest second step"},
	}}
	reminder, _, err := s.CreateReminder(ctx, reminderEvent.ID, "create", input, start)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, reminderEvent, personal.TurnOutput{Silent: true}, nil)
	if dispatched, err := s.DispatchDueReminder(ctx, *reminder.NextRunAt); err != nil || !dispatched {
		t.Fatalf("first reminder dispatch = (%t,%v)", dispatched, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(path, time.Minute)
	if err != nil {
		t.Fatalf("reopen reminder database: %v", err)
	}
	defer s.Close()
	if out, err := s.ClaimDelivery(ctx); err != nil || out != nil {
		t.Fatalf("sender claimed stale reminder before scheduler = (%+v,%v), want no delivery", out, err)
	}

	// A due reminder blocks only its own previous outbox item.
	unrelatedEvent := openUserEvent(t, s, 31_002, 31_002, "Send an unrelated reply")
	finishEvent(t, s, unrelatedEvent, personal.TurnOutput{Text: "unrelated reply"}, []string{"unrelated reply"})
	unrelated, err := s.ClaimDelivery(ctx)
	if err != nil || unrelated == nil || unrelated.Text != "unrelated reply" {
		t.Fatalf("sender claim with unrelated reply = (%+v,%v), want unrelated delivery", unrelated, err)
	}
	if err := s.UpdateDelivery(ctx, unrelated.ID, "sent", channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner", MessageID: "unrelated"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}

	if dispatched, err := s.DispatchDueReminder(ctx, time.Now().UTC()); err != nil || !dispatched {
		t.Fatalf("scheduler catch-up = (%t,%v)", dispatched, err)
	}
	latest, err := s.ClaimDelivery(ctx)
	if err != nil || latest == nil || latest.Text != "latest second step" {
		t.Fatalf("sender claim after scheduler = (%+v,%v), want latest reminder step", latest, err)
	}
	var staleStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE event_id=? AND text='stale first step'`, reminderEvent.ID).Scan(&staleStatus); err != nil || staleStatus != "suppressed" {
		t.Fatalf("stale reminder outbox status = (%q,%v), want suppressed", staleStatus, err)
	}
}

func TestReminderMutationRequiresDirectUserEvent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO personal_events(kind,text,status,created_at) VALUES('material','reference','completed',?)`, timestamp(now)); err != nil {
		t.Fatal(err)
	}
	var materialID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_events WHERE kind='material'`).Scan(&materialID); err != nil {
		t.Fatal(err)
	}
	input := personal.ReminderInput{Title: "Soon", Steps: []personal.ReminderStep{{AfterMinutes: 1, Text: "Ready"}}}
	if _, _, err := s.CreateReminder(ctx, materialID, "create", input, now); err == nil {
		t.Fatal("material event created a reminder")
	}
	if _, _, err := s.CancelReminder(ctx, materialID, "cancel", 1, now); err == nil {
		t.Fatal("material event cancelled a reminder")
	}
}

func TestReminderCancellationReplayPaginationAndDateBoundary(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	start := time.Date(2026, 12, 31, 23, 40, 0, 0, time.UTC)
	event := openUserEvent(t, s, 3, 3, "Set a reminder across midnight")
	input := reminderTestInput(start, false)
	input.Steps[0].AfterMinutes = 30
	input.Steps[1].AfterMinutes = 10
	reminder, _, err := s.CreateReminder(ctx, event.ID, "create-midnight", input, start)
	if err != nil {
		t.Fatal(err)
	}
	if reminder.NextRunAt == nil || !reminder.NextRunAt.Equal(time.Date(2027, 1, 1, 0, 10, 0, 0, time.UTC)) {
		t.Fatalf("cross-date first stage due at %v", reminder.NextRunAt)
	}
	finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
	if sent, err := s.DispatchDueReminder(ctx, *reminder.NextRunAt); err != nil || !sent {
		t.Fatalf("midnight DispatchDueReminder() = (%t,%v)", sent, err)
	}
	updated, err := scanReminder(s.db.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM personal_reminders WHERE id=?`, reminder.ID))
	if err != nil || updated.NextRunAt == nil || !updated.NextRunAt.Equal(time.Date(2027, 1, 1, 0, 20, 0, 0, time.UTC)) {
		t.Fatalf("second stage date = (%+v,%v)", updated, err)
	}

	cancelEvent := openUserEvent(t, s, 4, 4, "Cancel that reminder")
	cancelled, replayed, err := s.CancelReminder(ctx, cancelEvent.ID, "cancel", reminder.ID, start.Add(time.Hour))
	if err != nil || replayed || cancelled.Status != "cancelled" || cancelled.NextRunAt != nil {
		t.Fatalf("CancelReminder() = (%+v,%v,%v)", cancelled, replayed, err)
	}
	var stoppedPending int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=? AND status='suppressed'`, event.ID).Scan(&stoppedPending); err != nil || stoppedPending != 1 {
		t.Fatalf("cancelled reminder pending delivery = (%d,%v), want suppressed", stoppedPending, err)
	}
	finishEvent(t, s, cancelEvent, personal.TurnOutput{Silent: true}, nil)
	replay, replayed, err := s.CancelReminder(ctx, cancelEvent.ID, "cancel", reminder.ID, start.Add(2*time.Hour))
	if err != nil || !replayed || !reflect.DeepEqual(replay, cancelled) {
		t.Fatalf("CancelReminder replay = (%+v,%v,%v)", replay, replayed, err)
	}
	if sent, err := s.DispatchDueReminder(ctx, start.Add(24*time.Hour)); err != nil || sent {
		t.Fatalf("cancelled reminder dispatched = (%t,%v)", sent, err)
	}
	if _, _, err := s.CancelReminder(ctx, cancelEvent.ID, "cancel", reminder.ID+1, start); err == nil {
		t.Fatal("cancel request key reused with a different reminder ID succeeded")
	}

	// More than one page of terminal reminders remains discoverable in id order.
	for i := int64(0); i < personal.DefaultReminderPageSize+2; i++ {
		e := openUserEvent(t, s, 10+i, 10+i, "Create a short reminder")
		one := personal.ReminderInput{Title: "One shot", Steps: []personal.ReminderStep{{AfterMinutes: 1, Text: "Done"}}}
		if _, _, err := s.CreateReminder(ctx, e.ID, "create", one, start); err != nil {
			t.Fatal(err)
		}
		finishEvent(t, s, e, personal.TurnOutput{Silent: true}, nil)
	}
	firstPage, err := s.Reminders(ctx, false, 0, 10)
	if err != nil || len(firstPage) != 10 {
		t.Fatalf("first reminder page = (%d,%v)", len(firstPage), err)
	}
	secondPage, err := s.Reminders(ctx, false, firstPage[len(firstPage)-1].ID, 10)
	if err != nil || len(secondPage) != 10 || secondPage[0].ID >= firstPage[len(firstPage)-1].ID {
		t.Fatalf("second reminder page = (%+v,%v)", secondPage, err)
	}
	active, err := s.Reminders(ctx, true, 0, 10)
	if err != nil || len(active) != 10 {
		t.Fatalf("active reminder page = (%d,%v)", len(active), err)
	}
}

func TestReminderAdditiveSchemaUpgradesExistingVersion6(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "existing-v6.sqlite")
	db, err := openDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(personalSchema + scheduleSchema + scheduleSuggestionSchema + replySchema + taskInputSchema + materialSchema + progressSchema + deliverySchema); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path, time.Minute)
	if err != nil {
		t.Fatalf("NewStore on existing version 6 database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var version, reminderTables int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM personal_schema`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('personal_reminders','personal_reminder_requests')`).Scan(&reminderTables); err != nil {
		t.Fatal(err)
	}
	if version != 6 || reminderTables != 2 {
		t.Fatalf("schema version=%d reminder tables=%d", version, reminderTables)
	}
}

func TestReminderCalendarColumnsUpgradeExistingReminderTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "existing-v6-reminders.sqlite")
	db, err := openDatabase(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(personalSchema + scheduleSchema + scheduleSuggestionSchema + replySchema + taskInputSchema + materialSchema + progressSchema + deliverySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`CREATE TABLE personal_reminders (
		id INTEGER PRIMARY KEY,title TEXT NOT NULL,steps_json TEXT NOT NULL CHECK(json_valid(steps_json)),
		repeat INTEGER NOT NULL CHECK(repeat IN (0,1)),start_at TEXT NOT NULL,status TEXT NOT NULL
		CHECK(status IN ('active','completed','cancelled')),next_run_at TEXT,next_step INTEGER NOT NULL
		CHECK(next_step>=0),created_event_id INTEGER NOT NULL REFERENCES personal_events(id),
		created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
		CHECK((status='active' AND next_run_at IS NOT NULL) OR (status<>'active' AND next_run_at IS NULL)))`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path, time.Minute)
	if err != nil {
		t.Fatalf("NewStore on the earlier reminder schema: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.BindChannel(ctx, "telegram", "test-bot", "test-owner", false); err != nil {
		t.Fatal(err)
	}
	var migrated int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('personal_reminders') WHERE name IN ('cron','timezone','last_delivery_id')`).Scan(&migrated); err != nil || migrated != 3 {
		t.Fatalf("migrated reminder columns = (%d,%v), want all three", migrated, err)
	}
	event := openUserEvent(t, s, 40_004, 40_004, "Create after an additive reminder migration")
	input := personal.ReminderInput{Title: "After migration", Cron: "29 * * * *", Steps: []personal.ReminderStep{{AfterMinutes: 0, Text: "Fixed"}}}
	created, _, err := s.CreateReminder(ctx, event.ID, "calendar", input, time.Now().UTC())
	if err != nil {
		t.Fatalf("create calendar reminder after migration: %v", err)
	}
	if created.Timezone != personal.DefaultScheduleTimezone || created.Cron != input.Cron {
		t.Fatalf("calendar defaults after migration = cron %q timezone %q", created.Cron, created.Timezone)
	}
}

func TestCalendarReminderFixedTextAndMissedRuns(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	createdAt := time.Date(2026, 11, 6, 0, 28, 0, 0, time.UTC)
	event := openUserEvent(t, s, 40_001, 40_001, "Every hour at minute 29, send one fixed sentence")
	input := personal.ReminderInput{
		Title: "Hourly nudge", Cron: "29 * * * *", Timezone: "Asia/Shanghai",
		Steps: []personal.ReminderStep{{AfterMinutes: 0, Text: "喝水，起身活动一下。"}},
	}
	reminder, replayed, err := s.CreateReminder(ctx, event.ID, "calendar", input, createdAt)
	if err != nil || replayed {
		t.Fatalf("CreateReminder(calendar) = (%+v,%v,%v)", reminder, replayed, err)
	}
	wantFirst := time.Date(2026, 11, 6, 0, 29, 0, 0, time.UTC)
	if reminder.NextRunAt == nil || !reminder.NextRunAt.Equal(wantFirst) || reminder.Cron != input.Cron || reminder.Timezone != input.Timezone || reminder.Repeat {
		t.Fatalf("calendar reminder snapshot = %+v", reminder)
	}
	finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
	if dispatched, err := s.DispatchDueReminder(ctx, wantFirst.Add(time.Second)); err != nil || !dispatched {
		t.Fatalf("first calendar dispatch = (%t,%v)", dispatched, err)
	}
	var text string
	if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_outbox ORDER BY id DESC LIMIT 1`).Scan(&text); err != nil || text != input.Steps[0].Text {
		t.Fatalf("fixed reminder text = (%q,%v)", text, err)
	}
	late := time.Date(2026, 11, 6, 2, 30, 0, 0, time.UTC)
	if dispatched, err := s.DispatchDueReminder(ctx, late); err != nil || !dispatched {
		t.Fatalf("late calendar dispatch = (%t,%v)", dispatched, err)
	}
	current, err := scanReminder(s.db.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM personal_reminders WHERE id=?`, reminder.ID))
	if err != nil || current.NextRunAt == nil || !current.NextRunAt.Equal(time.Date(2026, 11, 6, 3, 29, 0, 0, time.UTC)) {
		t.Fatalf("calendar next occurrence = (%+v,%v)", current, err)
	}
	var tasks, runs, outboxCount, pendingCount, suppressedCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_tasks`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE kind='task'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=?`, event.ID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=? AND status='pending'`, event.ID).Scan(&pendingCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE event_id=? AND status='suppressed'`, event.ID).Scan(&suppressedCount); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 || runs != 0 || outboxCount != 2 || pendingCount != 1 || suppressedCount != 1 {
		t.Fatalf("calendar delivery side effects: tasks=%d runs=%d outbox=%d pending=%d suppressed=%d", tasks, runs, outboxCount, pendingCount, suppressedCount)
	}

	replay, wasReplay, err := s.CreateReminder(ctx, event.ID, "calendar", input, createdAt.Add(time.Hour))
	if err != nil || !wasReplay || !reflect.DeepEqual(replay, reminder) {
		t.Fatalf("calendar create replay = (%+v,%v,%v)", replay, wasReplay, err)
	}
}

func TestCalendarReminderUsesScheduleDSTRules(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	after := time.Date(2024, time.March, 10, 5, 0, 0, 0, time.UTC)
	event := openUserEvent(t, s, 40_002, 40_002, "Send this fixed sentence at 2:30 am")
	input := personal.ReminderInput{
		Title: "DST check", Cron: "30 2 * * *", Timezone: "America/New_York",
		Steps: []personal.ReminderStep{{AfterMinutes: 0, Text: "DST fixed message"}},
	}
	reminder, _, err := s.CreateReminder(ctx, event.ID, "dst", input, after)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2024, time.March, 11, 6, 30, 0, 0, time.UTC)
	if reminder.NextRunAt == nil || !reminder.NextRunAt.Equal(want) {
		t.Fatalf("calendar reminder next run = %v, want %s", reminder.NextRunAt, want)
	}
	finishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
	if dispatched, err := s.DispatchDueReminder(ctx, want); err != nil || !dispatched {
		t.Fatalf("DST calendar dispatch = (%t,%v)", dispatched, err)
	}
	current, err := scanReminder(s.db.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM personal_reminders WHERE id=?`, reminder.ID))
	if err != nil || current.NextRunAt == nil || !current.NextRunAt.Equal(time.Date(2024, time.March, 12, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("DST next calendar occurrence = (%+v,%v)", current, err)
	}
}

func TestCalendarReminderRequiresSingleNonRelativeText(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 40_003, 40_003, "Create a fixed calendar reminder")
	valid := personal.ReminderInput{Title: "Hourly", Cron: "29 * * * *", Steps: []personal.ReminderStep{{AfterMinutes: 0, Text: "Fixed"}}}
	invalid := []personal.ReminderInput{
		{Title: "Hourly", Cron: valid.Cron, Steps: []personal.ReminderStep{{AfterMinutes: 1, Text: "Fixed"}}},
		{Title: "Hourly", Cron: valid.Cron, Repeat: true, Steps: valid.Steps},
		{Title: "Hourly", Cron: valid.Cron, StartAt: ptrTime(time.Now()), Steps: valid.Steps},
		{Title: "Hourly", Timezone: "UTC", Steps: valid.Steps},
		{Title: "Hourly", Cron: "60 * * * *", Steps: []personal.ReminderStep{{AfterMinutes: 0, Text: "Fixed"}}},
	}
	for i, input := range invalid {
		if _, _, err := s.CreateReminder(ctx, event.ID, "invalid-"+strconv.Itoa(i), input, time.Now().UTC()); err == nil {
			t.Fatalf("invalid calendar input %d succeeded", i)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
