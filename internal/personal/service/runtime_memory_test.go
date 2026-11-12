package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRuntimeMemoryReviewResultsStaySilent(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	r := testRuntime(s)
	r.ReviewMemory = func(_ context.Context, input personal.MemoryReviewInput) (personal.MemoryReview, error) {
		switch input.Event.Text {
		case "I prefer short answers.":
			return personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "answer_style", Value: "short", Evidence: input.Event.Text}}}, nil
		case "Review failed.":
			return personal.MemoryReview{}, errors.New("review failed")
		default:
			return personal.MemoryReview{Question: "Should this preference apply to reports?"}, nil
		}
	}

	for i, text := range []string{"I prefer short answers.", "Review failed.", "I sometimes prefer short answers."} {
		event := testkit.OpenUserEvent(t, s, int64(i+1), int64(i+1), text)
		testkit.FinishEvent(t, s, event, personal.TurnOutput{Silent: true}, nil)
		if worked, err := r.memory(ctx); err != nil || !worked {
			t.Fatalf("review %q: worked=%v err=%v", text, worked, err)
		}
		snapshot, err := s.Diagnostic(ctx, event.ID, 0)
		if err != nil || len(snapshot.Runs) != 1 {
			t.Fatalf("review diagnostic runs: %+v, %v", snapshot.Runs, err)
		}
		run := snapshot.Runs[0]
		if run.Kind != personal.RunMemoryReview {
			t.Fatalf("review diagnostic run kind for %q: %q", text, run.Kind)
		}
		switch text {
		case "I prefer short answers.":
			if run.Status != personal.RunCompleted {
				t.Fatalf("successful review status: %+v", run)
			}
		case "Review failed.":
			if run.Status != personal.RunFailed || run.Error == "" {
				t.Fatalf("failed review status: %+v", run)
			}
		default:
			var review personal.MemoryReview
			if run.Status != personal.RunCompleted || json.Unmarshal([]byte(run.Result), &review) != nil || review.Question == "" {
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
	events, err := s.RecentEvents(ctx, 100)
	if err != nil || len(events) != 3 {
		t.Fatalf("memory review created callback events: %+v, %v", events, err)
	}
	for _, event := range events {
		if event.Kind != personal.EventUser {
			t.Fatalf("memory review emitted event kind %q", event.Kind)
		}
	}
	if event, err := s.ClaimEvent(ctx); err != nil || event != nil {
		t.Fatalf("memory review queued a conversation event: %+v, %v", event, err)
	}
	if delivery, err := s.ClaimDelivery(ctx); err != nil || delivery != nil {
		t.Fatalf("memory review queued a delivery: %+v, %v", delivery, err)
	}
}
