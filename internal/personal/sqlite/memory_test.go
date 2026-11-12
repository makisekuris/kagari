package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/personal"
)

func assertNoMemoryNotifications(t *testing.T, s *Store) {
	t.Helper()
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_events WHERE kind='memory'`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("memory events = (%d,%v), want none", events, err)
	}
	if event, err := s.ClaimEvent(context.Background()); err != nil || event != nil {
		t.Fatalf("memory maintenance callback = (%+v,%v), want none", event, err)
	}
	if delivery, err := s.ClaimDelivery(context.Background()); err != nil || delivery != nil {
		t.Fatalf("memory maintenance delivery = (%+v,%v), want none", delivery, err)
	}
	var outbox int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_outbox`).Scan(&outbox); err != nil || outbox != 0 {
		t.Fatalf("memory maintenance outbox rows = (%d,%v), want none", outbox, err)
	}
}

func TestMemoryMaintenanceFailuresAndClarificationStaySilent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", 30*time.Minute)

	reviewFailureEvent := openUserEvent(t, s, 2, 902, "Review this preference")
	finishEvent(t, s, reviewFailureEvent, personal.TurnOutput{Silent: true}, nil)
	reviewFailureRun, err := s.ClaimRun(ctx, personal.RunMemoryReview)
	if err != nil || reviewFailureRun == nil || reviewFailureRun.EventID != reviewFailureEvent.ID {
		t.Fatalf("review failure claim = (%+v,%v)", reviewFailureRun, err)
	}
	failureReview := personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "style", Value: "brief", Evidence: "Review this preference"}}}
	if err := s.FinishMemoryReview(ctx, reviewFailureRun.ID, failureReview, "review failed"); err != nil {
		t.Fatal(err)
	}
	failedReview, err := s.Run(ctx, reviewFailureRun.ID)
	if err != nil || failedReview == nil || failedReview.Status != personal.RunFailed || failedReview.Error != "review failed" || failedReview.Result == "" {
		t.Fatalf("failed review run = (%+v,%v)", failedReview, err)
	}
	if err := s.FinishMemoryReview(ctx, reviewFailureRun.ID, failureReview, "review failed"); err != nil {
		t.Fatal(err)
	}
	failedReviewAgain, err := s.Run(ctx, reviewFailureRun.ID)
	if err != nil || failedReviewAgain == nil || *failedReviewAgain != *failedReview {
		t.Fatalf("repeated review failure changed run = (%+v,%v), want %+v", failedReviewAgain, err, failedReview)
	}

	invalidReviewEvent := openUserEvent(t, s, 3, 903, "My summaries should be brief")
	finishEvent(t, s, invalidReviewEvent, personal.TurnOutput{Silent: true}, nil)
	invalidReviewRun, err := s.ClaimRun(ctx, personal.RunMemoryReview)
	if err != nil || invalidReviewRun == nil || invalidReviewRun.EventID != invalidReviewEvent.ID {
		t.Fatalf("invalid review claim = (%+v,%v)", invalidReviewRun, err)
	}
	duplicate := personal.MemoryChange{Operation: "put", Key: "style.summary", Value: "brief", Evidence: "My summaries should be brief"}
	invalidReview := personal.MemoryReview{Changes: []personal.MemoryChange{duplicate, duplicate}}
	if err := s.FinishMemoryReview(ctx, invalidReviewRun.ID, invalidReview, ""); err != nil {
		t.Fatal(err)
	}
	invalidReviewResult, err := s.Run(ctx, invalidReviewRun.ID)
	if err != nil || invalidReviewResult == nil || invalidReviewResult.Status != personal.RunFailed || !strings.Contains(invalidReviewResult.Error, "duplicate key") || invalidReviewResult.Result == "" {
		t.Fatalf("invalid review run = (%+v,%v)", invalidReviewResult, err)
	}
	if memory, err := s.Memory(ctx, "style.summary"); err != nil || memory != nil {
		t.Fatalf("invalid review applied a change = (%+v,%v)", memory, err)
	}

	questionEvent := openUserEvent(t, s, 4, 904, "I sometimes prefer short answers")
	finishEvent(t, s, questionEvent, personal.TurnOutput{Silent: true}, nil)
	questionRun, err := s.ClaimRun(ctx, personal.RunMemoryReview)
	if err != nil || questionRun == nil || questionRun.EventID != questionEvent.ID {
		t.Fatalf("clarification review claim = (%+v,%v)", questionRun, err)
	}
	clarification := personal.MemoryReview{Question: "Should this apply to summaries too?"}
	if err := s.FinishMemoryReview(ctx, questionRun.ID, clarification, ""); err != nil {
		t.Fatal(err)
	}
	completedReview, err := s.Run(ctx, questionRun.ID)
	if err != nil || completedReview == nil || completedReview.Status != personal.RunCompleted || completedReview.Error != "" {
		t.Fatalf("clarification run = (%+v,%v)", completedReview, err)
	}
	var storedReview personal.MemoryReview
	if err := json.Unmarshal([]byte(completedReview.Result), &storedReview); err != nil || storedReview.Question != clarification.Question {
		t.Fatalf("clarification result = (%+v,%v)", storedReview, err)
	}
	if err := s.FinishMemoryReview(ctx, questionRun.ID, clarification, ""); err != nil {
		t.Fatal(err)
	}
	completedAgain, err := s.Run(ctx, questionRun.ID)
	if err != nil || completedAgain == nil || *completedAgain != *completedReview {
		t.Fatalf("repeated clarification changed run = (%+v,%v), want %+v", completedAgain, err, completedReview)
	}
	assertNoMemoryNotifications(t, s)
}

func TestWriteMemorySequentialChangesReplayAndRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.db")
	s := testStore(t, path, time.Minute)
	event := openUserEvent(t, s, 1, 1001, "Remember tea, correct it to coffee, then forget it.")
	first := personal.MemoryChange{Operation: "put", Key: "drink", Value: "tea", Evidence: "Remember tea"}
	original, err := s.WriteMemory(ctx, event.ID, "put", first)
	if err != nil || original.Version != 1 {
		t.Fatalf("first write = (%+v,%v)", original, err)
	}
	correct := personal.MemoryChange{Operation: "put", Key: "drink", Value: "coffee", ExpectedVersion: 1, Evidence: "correct it to coffee"}
	corrected, err := s.WriteMemory(ctx, event.ID, "correct", correct)
	if err != nil || corrected.Version != 2 || corrected.Value != "coffee" {
		t.Fatalf("same-event correction = (%+v,%v)", corrected, err)
	}
	if _, err := s.WriteMemory(ctx, event.ID, "stale", first); !errors.Is(err, errMemoryVersionConflict) {
		t.Fatalf("stale write error = %v", err)
	}
	forget := personal.MemoryChange{Operation: "forget", Key: "drink", ExpectedVersion: 2, Evidence: "forget it"}
	forgotten, err := s.WriteMemory(ctx, event.ID, "forget", forget)
	if err != nil || forgotten.Version != 3 || !forgotten.Deleted || forgotten.Value != "" {
		t.Fatalf("same-event forget = (%+v,%v)", forgotten, err)
	}
	for _, replay := range []struct {
		key    string
		change personal.MemoryChange
		want   personal.Memory
	}{{"put", first, original}, {"correct", correct, corrected}, {"forget", forget, forgotten}} {
		if got, err := s.WriteMemory(ctx, event.ID, replay.key, replay.change); err != nil || got != replay.want {
			t.Fatalf("replay %s = (%+v,%v), want %+v", replay.key, got, err, replay.want)
		}
	}
	if memory, err := s.Memory(ctx, "drink"); err != nil || memory == nil || *memory != forgotten {
		t.Fatalf("replay changed current memory = (%+v,%v)", memory, err)
	}
	var requests, histories, runs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_requests`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_memory_history WHERE run_id IS NULL`).Scan(&histories); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if requests != 3 || histories != 3 || runs != 0 {
		t.Fatalf("explicit write records: requests=%d histories=%d runs=%d", requests, histories, runs)
	}
	// The user turn never completed, but its committed memory must survive a restart.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, path, time.Minute)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if memory, err := s.Memory(ctx, "drink"); err != nil || memory == nil || *memory != forgotten {
		t.Fatalf("interrupted turn lost memory = (%+v,%v)", memory, err)
	}
	recovered, err := s.ClaimEvent(ctx)
	if err != nil || recovered == nil || !recovered.Interrupted {
		t.Fatalf("recovered event = (%+v,%v)", recovered, err)
	}
	if replay, err := s.WriteMemory(ctx, recovered.ID, "put", first); err != nil || replay != original {
		t.Fatalf("restarted replay = (%+v,%v)", replay, err)
	}
}

func TestWriteMemoryRollsBackWithoutSuccessReceipt(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1002, "Remember tea")
	change := personal.MemoryChange{Operation: "put", Key: "drink", Value: "tea", Evidence: event.Text}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_request BEFORE INSERT ON personal_requests BEGIN SELECT RAISE(ABORT, 'receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteMemory(ctx, event.ID, "put", change); err == nil {
		t.Fatal("receipt failure was ignored")
	}
	if memory, err := s.Memory(ctx, "drink"); err != nil || memory != nil {
		t.Fatalf("failed transaction left memory = (%+v,%v)", memory, err)
	}
	if history, err := s.MemoryHistory(ctx, "drink", 10); err != nil || len(history) != 0 {
		t.Fatalf("failed transaction left history = (%+v,%v)", history, err)
	}
	var requests int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM personal_requests`).Scan(&requests); err != nil || requests != 0 {
		t.Fatalf("failed transaction left requests = (%d,%v)", requests, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_request`); err != nil {
		t.Fatal(err)
	}
	if memory, err := s.WriteMemory(ctx, event.ID, "put", change); err != nil || memory.Version != 1 {
		t.Fatalf("retry after failed transaction = (%+v,%v)", memory, err)
	}
	if _, err := s.WriteMemory(ctx, event.ID, "forget-no-evidence", personal.MemoryChange{Operation: "forget", Key: "drink", ExpectedVersion: 1}); err == nil {
		t.Fatal("forget without source evidence was accepted")
	}
}

func TestDelayedReviewCannotReplaceNewerExplicitMemory(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	older := openUserEvent(t, s, 1, 1003, "I prefer tea")
	finishEvent(t, s, older, personal.TurnOutput{Silent: true}, nil)
	review, err := s.ClaimRun(ctx, personal.RunMemoryReview)
	if err != nil || review == nil {
		t.Fatalf("claim older review = (%+v,%v)", review, err)
	}
	newer := openUserEvent(t, s, 2, 1004, "Remember coffee")
	written, err := s.WriteMemory(ctx, newer.ID, "put", personal.MemoryChange{Operation: "put", Key: "drink", Value: "coffee", Evidence: newer.Text})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishMemoryReview(ctx, review.ID, personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "drink", Value: "tea", ExpectedVersion: written.Version, Evidence: older.Text}}}, ""); err != nil {
		t.Fatal(err)
	}
	if memory, err := s.Memory(ctx, "drink"); err != nil || memory == nil || *memory != written {
		t.Fatalf("older review replaced explicit memory = (%+v,%v)", memory, err)
	}
}
