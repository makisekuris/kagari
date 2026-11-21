package assistant

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func memoryReviewResponse(review personal.MemoryReview) testkit.Response {
	encoded, _ := json.Marshal(review)
	return testkit.ResponseText(string(encoded))
}

func newMemoryReviewer(t *testing.T, fixture *testkit.ResponsesFixture) *MainAgent {
	t.Helper()
	return &MainAgent{Model: fixture.Model(t), Timeout: 4 * time.Second}
}

func TestReviewMemoryExtractsCurrentFactsAndUsesExistingVersion(t *testing.T) {
	tests := []struct {
		name   string
		input  personal.MemoryReviewInput
		review personal.MemoryReview
	}{
		{
			name:   "new durable preference",
			input:  personal.MemoryReviewInput{Event: personal.Event{ID: 40, Kind: personal.EventUser, Text: "I prefer concise answers."}},
			review: personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "answer_style", Value: "concise", ExpectedVersion: 0, Evidence: "I prefer concise answers."}}},
		},
		{
			name:   "correction keeps key and version",
			input:  personal.MemoryReviewInput{Event: personal.Event{ID: 41, Kind: personal.EventUser, Text: "Correction: I now prefer detailed answers."}, Memories: []personal.Memory{{Key: "answer_style", Value: "concise", Version: 3}}},
			review: personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "answer_style", Value: "detailed", ExpectedVersion: 3, Evidence: "I now prefer detailed answers."}}},
		},
		{
			name:   "reintroduces tombstone by its version",
			input:  personal.MemoryReviewInput{Event: personal.Event{ID: 42, Kind: personal.EventUser, Text: "Please remember again: I prefer concise answers."}, Memories: []personal.Memory{{Key: "answer_style", Value: "old forgotten value", Version: 4, Deleted: true}}},
			review: personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "answer_style", Value: "concise", ExpectedVersion: 4, Evidence: "I prefer concise answers."}}},
		},
		{
			name:   "one-off operation settings stay local",
			input:  personal.MemoryReviewInput{Event: personal.Event{ID: 43, Kind: personal.EventUser, Text: "For this session, set the timer to 30 minutes of work and 10 minutes of rest, then message me at each boundary."}, Memories: []personal.Memory{{Key: "reminder_interval_minutes", Value: "40", Version: 7}}},
			review: personal.MemoryReview{Changes: []personal.MemoryChange{}},
		},
		{
			name:   "explicit long-term default updates existing key",
			input:  personal.MemoryReviewInput{Event: personal.Event{ID: 44, Kind: personal.EventUser, Text: "From now on, make the 30+10 timer pattern the default for all Pomodoro sessions."}, Memories: []personal.Memory{{Key: "pomodoro.default_pattern", Value: "25 minutes work and 5 minutes rest", Version: 1}}},
			review: personal.MemoryReview{Changes: []personal.MemoryChange{{Operation: "put", Key: "pomodoro.default_pattern", Value: "30 minutes work and 10 minutes rest", ExpectedVersion: 1, Evidence: "make the 30+10 timer pattern the default for all Pomodoro sessions"}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{memoryReviewResponse(tc.review)}}
			review, err := newMemoryReviewer(t, fixture).ReviewMemory(context.Background(), tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if len(review.Changes) != len(tc.review.Changes) {
				t.Fatalf("review=%+v want=%+v", review, tc.review)
			}
			for i, change := range tc.review.Changes {
				if review.Changes[i] != change {
					t.Fatalf("review change %d=%+v want %+v", i, review.Changes[i], change)
				}
			}
			if len(tc.input.Memories) > 0 {
				inputText := testkit.RequestInput(fixture.Requests[0])
				for _, memory := range tc.input.Memories {
					if !strings.Contains(inputText, memory.Key) || !strings.Contains(inputText, `\"version\":`+strconv.FormatInt(memory.Version, 10)) {
						t.Fatalf("review input omitted existing memory key/version %q/%d: %s", memory.Key, memory.Version, inputText)
					}
				}
			}
			schema := assertStrictSchema(t, fixture.Requests[0], "personal_memory_review")
			properties := schema["properties"].(map[string]any)
			changeSchema := properties["changes"].(map[string]any)["items"].(map[string]any)
			if changeSchema["additionalProperties"] != false {
				t.Fatalf("memory change schema is not closed: %#v", changeSchema)
			}
			inputText := testkit.RequestInput(fixture.Requests[0])
			if tc.input.Event.ID == 42 && strings.Contains(inputText, "old forgotten value") {
				t.Fatalf("review input included tombstone value: %s", inputText)
			}
			instructions, _ := fixture.Requests[0]["instructions"].(string)
			instructions += testkit.RequestInput(fixture.Requests[0])
			if strings.Contains(instructions, "Persona style token") || strings.Contains(instructions, "Profile facts token") {
				t.Fatalf("memory review inherited persona/profile: %q", instructions)
			}
			for _, guidance := range []string{
				"Their existence, names, goals, and complete definitions are not personal facts; never copy a task or plan into memory, even if the user repeats or triggers it again.",
				"From a message that also contains a plan, extract only a separately stated stable fact or preference that applies beyond that plan.",
				"registering “Daily Go drills at 09:00 UTC” alone yields no memory",
				"adding “I generally prefer short explanations with exercises, regardless of the plan” permits only that preference",
				"A request to perform an operation now does not by itself establish a preference for future operations",
				"Only extend a setting beyond the current operation when the user clearly assigns it broader scope",
			} {
				if !strings.Contains(instructions, guidance) {
					t.Fatalf("memory review omitted scope guidance %q: %q", guidance, instructions)
				}
			}
		})
	}
}

func TestReviewMemoryReturnsEmptyForNoFactsAndRejectsNonUserEvents(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{memoryReviewResponse(personal.MemoryReview{Changes: []personal.MemoryChange{}})}}
	review, err := newMemoryReviewer(t, fixture).ReviewMemory(context.Background(), personal.MemoryReviewInput{
		Event: personal.Event{ID: 50, Kind: personal.EventUser, Text: "Thanks, that is useful."},
	})
	if err != nil || len(review.Changes) != 0 || review.Question != "" {
		t.Fatalf("review=%+v err=%v", review, err)
	}
	if len(fixture.Requests) != 1 {
		t.Fatalf("requests=%d, want 1", len(fixture.Requests))
	}

	nonUser := &testkit.ResponsesFixture{T: t}
	if _, err := newMemoryReviewer(t, nonUser).ReviewMemory(context.Background(), personal.MemoryReviewInput{Event: personal.Event{ID: 51, Kind: personal.EventMaterial, Text: "I live in Tokyo."}}); err == nil {
		t.Fatal("material event was accepted as memory evidence")
	}
	if len(nonUser.Requests) != 0 {
		t.Fatalf("non-user event reached model: %d requests", len(nonUser.Requests))
	}

	empty := &testkit.ResponsesFixture{T: t}
	review, err = newMemoryReviewer(t, empty).ReviewMemory(context.Background(), personal.MemoryReviewInput{Event: personal.Event{ID: 52, Kind: personal.EventUser}})
	if err != nil || len(review.Changes) != 0 || len(empty.Requests) != 0 {
		t.Fatalf("empty event review=%+v err=%v requests=%d", review, err, len(empty.Requests))
	}
}

func TestReviewMemoryRejectsUnprovenOrStaleChanges(t *testing.T) {
	tests := []struct {
		name   string
		input  personal.MemoryReviewInput
		change personal.MemoryChange
	}{
		{
			name:   "evidence outside current message",
			input:  personal.MemoryReviewInput{Event: personal.Event{ID: 60, Kind: personal.EventUser, Text: "I prefer concise answers."}},
			change: personal.MemoryChange{Operation: "put", Key: "answer_style", Value: "detailed", ExpectedVersion: 0, Evidence: "I prefer detailed answers."},
		},
		{
			name:   "stale existing version",
			input:  personal.MemoryReviewInput{Event: personal.Event{ID: 61, Kind: personal.EventUser, Text: "I now prefer detailed answers."}, Memories: []personal.Memory{{Key: "answer_style", Value: "concise", Version: 3}}},
			change: personal.MemoryChange{Operation: "put", Key: "answer_style", Value: "detailed", ExpectedVersion: 2, Evidence: "I now prefer detailed answers."},
		},
		{
			name:   "automatic forget forbidden",
			input:  personal.MemoryReviewInput{Event: personal.Event{ID: 62, Kind: personal.EventUser, Text: "Forget answer style."}},
			change: personal.MemoryChange{Operation: "forget", Key: "answer_style", ExpectedVersion: 1, Evidence: "Forget answer style."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{memoryReviewResponse(personal.MemoryReview{Changes: []personal.MemoryChange{tc.change}})}}
			_, err := newMemoryReviewer(t, fixture).ReviewMemory(context.Background(), tc.input)
			if err == nil {
				t.Fatal("invalid proposal was accepted")
			}
		})
	}
}

func TestReviewMemoryRejectsMalformedAndRefusedResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp testkit.Response
	}{
		{name: "extra field", resp: testkit.ResponseText(`{"changes":[],"question":"","extra":true}`)},
		{name: "refusal", resp: testkit.ResponseRefusal()},
		{name: "incomplete", resp: testkit.Response{Status: "incomplete", Output: testkit.ResponseText(`{"changes":[],"question":""}`).Output}},
		{name: "blank", resp: testkit.ResponseText(" ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{tc.resp}}
			_, err := newMemoryReviewer(t, fixture).ReviewMemory(context.Background(), personal.MemoryReviewInput{Event: personal.Event{ID: 70, Kind: personal.EventUser, Text: "I like short answers."}})
			if err == nil {
				t.Fatal("invalid output was accepted")
			}
		})
	}
}
