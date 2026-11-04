package personal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

	// Queue malformed input directly to exercise ApplyMemory's persisted validation failure.
	validationSource := openUserEvent(t, s, 1, 901, "Remember my preference")
	invalid, err := json.Marshal(MemoryChange{Operation: "put", Key: "Invalid Key", Value: "value", Evidence: "Remember my preference"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO personal_runs(event_id,kind,input,status,created_at) VALUES(?, 'memory', ?, 'queued', ?)`, validationSource.ID, string(invalid), timestamp(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	validationRunID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, validationSource, TurnOutput{Silent: true}, nil)
	validationRun, err := s.ClaimRun(ctx, RunMemory)
	if err != nil || validationRun == nil || validationRun.ID != validationRunID {
		t.Fatalf("invalid memory run claim = (%+v,%v)", validationRun, err)
	}
	if err := s.ApplyMemory(ctx, validationRun.ID); err != nil {
		t.Fatal(err)
	}
	failed, err := s.Run(ctx, validationRun.ID)
	if err != nil || failed == nil || failed.Status != RunFailed || failed.Error == "" {
		t.Fatalf("validation failure run = (%+v,%v)", failed, err)
	}
	if err := s.ApplyMemory(ctx, validationRun.ID); err != nil {
		t.Fatal(err)
	}
	failedAgain, err := s.Run(ctx, validationRun.ID)
	if err != nil || failedAgain == nil || *failedAgain != *failed {
		t.Fatalf("repeated validation failure changed run = (%+v,%v), want %+v", failedAgain, err, failed)
	}
	reviewRun, err := s.ClaimRun(ctx, RunMemoryReview)
	if err != nil || reviewRun == nil || reviewRun.EventID != validationSource.ID {
		t.Fatalf("validation source review claim = (%+v,%v)", reviewRun, err)
	}
	if err := s.FinishMemoryReview(ctx, reviewRun.ID, MemoryReview{}, ""); err != nil {
		t.Fatal(err)
	}

	reviewFailureEvent := openUserEvent(t, s, 2, 902, "Review this preference")
	finishEvent(t, s, reviewFailureEvent, TurnOutput{Silent: true}, nil)
	reviewFailureRun, err := s.ClaimRun(ctx, RunMemoryReview)
	if err != nil || reviewFailureRun == nil || reviewFailureRun.EventID != reviewFailureEvent.ID {
		t.Fatalf("review failure claim = (%+v,%v)", reviewFailureRun, err)
	}
	failureReview := MemoryReview{Changes: []MemoryChange{{Operation: "put", Key: "style", Value: "brief", Evidence: "Review this preference"}}}
	if err := s.FinishMemoryReview(ctx, reviewFailureRun.ID, failureReview, "review failed"); err != nil {
		t.Fatal(err)
	}
	failedReview, err := s.Run(ctx, reviewFailureRun.ID)
	if err != nil || failedReview == nil || failedReview.Status != RunFailed || failedReview.Error != "review failed" || failedReview.Result == "" {
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
	finishEvent(t, s, invalidReviewEvent, TurnOutput{Silent: true}, nil)
	invalidReviewRun, err := s.ClaimRun(ctx, RunMemoryReview)
	if err != nil || invalidReviewRun == nil || invalidReviewRun.EventID != invalidReviewEvent.ID {
		t.Fatalf("invalid review claim = (%+v,%v)", invalidReviewRun, err)
	}
	duplicate := MemoryChange{Operation: "put", Key: "style.summary", Value: "brief", Evidence: "My summaries should be brief"}
	invalidReview := MemoryReview{Changes: []MemoryChange{duplicate, duplicate}}
	if err := s.FinishMemoryReview(ctx, invalidReviewRun.ID, invalidReview, ""); err != nil {
		t.Fatal(err)
	}
	invalidReviewResult, err := s.Run(ctx, invalidReviewRun.ID)
	if err != nil || invalidReviewResult == nil || invalidReviewResult.Status != RunFailed || !strings.Contains(invalidReviewResult.Error, "duplicate key") || invalidReviewResult.Result == "" {
		t.Fatalf("invalid review run = (%+v,%v)", invalidReviewResult, err)
	}
	if memory, err := s.Memory(ctx, "style.summary"); err != nil || memory != nil {
		t.Fatalf("invalid review applied a change = (%+v,%v)", memory, err)
	}

	questionEvent := openUserEvent(t, s, 4, 904, "I sometimes prefer short answers")
	finishEvent(t, s, questionEvent, TurnOutput{Silent: true}, nil)
	questionRun, err := s.ClaimRun(ctx, RunMemoryReview)
	if err != nil || questionRun == nil || questionRun.EventID != questionEvent.ID {
		t.Fatalf("clarification review claim = (%+v,%v)", questionRun, err)
	}
	clarification := MemoryReview{Question: "Should this apply to summaries too?"}
	if err := s.FinishMemoryReview(ctx, questionRun.ID, clarification, ""); err != nil {
		t.Fatal(err)
	}
	completedReview, err := s.Run(ctx, questionRun.ID)
	if err != nil || completedReview == nil || completedReview.Status != RunCompleted || completedReview.Error != "" {
		t.Fatalf("clarification run = (%+v,%v)", completedReview, err)
	}
	var storedReview MemoryReview
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
