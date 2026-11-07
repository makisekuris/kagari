package personal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRuntimeMemoryReviewResultsStaySilent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	r := testRuntime(s)
	r.ReviewMemory = func(_ context.Context, input MemoryReviewInput) (MemoryReview, error) {
		switch input.Event.Text {
		case "I prefer short answers.":
			return MemoryReview{Changes: []MemoryChange{{Operation: "put", Key: "answer_style", Value: "short", Evidence: input.Event.Text}}}, nil
		case "Review failed.":
			return MemoryReview{}, errors.New("review failed")
		default:
			return MemoryReview{Question: "Should this preference apply to reports?"}, nil
		}
	}

	for i, text := range []string{"I prefer short answers.", "Review failed.", "I sometimes prefer short answers."} {
		event := openUserEvent(t, s, int64(i+1), int64(i+1), text)
		finishEvent(t, s, event, TurnOutput{Silent: true}, nil)
		if worked, err := r.memory(ctx); err != nil || !worked {
			t.Fatalf("review %q: worked=%v err=%v", text, worked, err)
		}
		var runID int64
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_runs WHERE event_id=? AND kind=?`, event.ID, RunMemoryReview).Scan(&runID); err != nil {
			t.Fatal(err)
		}
		run, err := s.Run(ctx, runID)
		if err != nil || run == nil {
			t.Fatalf("review run for %q: %+v, %v", text, run, err)
		}
		switch text {
		case "I prefer short answers.":
			if run.Status != RunCompleted {
				t.Fatalf("successful review status: %+v", run)
			}
		case "Review failed.":
			if run.Status != RunFailed || run.Error == "" {
				t.Fatalf("failed review status: %+v", run)
			}
		default:
			var review MemoryReview
			if run.Status != RunCompleted || json.Unmarshal([]byte(run.Result), &review) != nil || review.Question == "" {
				t.Fatalf("clarification review was not retained: %+v", run)
			}
		}
	}
	if worked, err := r.memory(ctx); err != nil || worked {
		t.Fatalf("empty review queue: worked=%v err=%v", worked, err)
	}
	if memory, err := s.Memory(ctx, "answer_style"); err != nil || memory == nil || memory.Value != "short" {
		t.Fatalf("successful review was not applied: %+v, %v", memory, err)
	}
	var callbacks, outbox int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_events WHERE kind='memory'`).Scan(&callbacks); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 || outbox != 0 {
		t.Fatalf("memory review emitted callbacks or messages: callbacks=%d outbox=%d", callbacks, outbox)
	}
	if event, err := s.ClaimEvent(ctx); err != nil || event != nil {
		t.Fatalf("memory review queued a conversation event: %+v, %v", event, err)
	}
	if delivery, err := s.ClaimDelivery(ctx); err != nil || delivery != nil {
		t.Fatalf("memory review queued a delivery: %+v, %v", delivery, err)
	}
}
