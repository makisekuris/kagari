package assistant

import (
	"context"
	"encoding/json"
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
		name    string
		input   personal.MemoryReviewInput
		change  personal.MemoryChange
		wantKey string
	}{
		{
			name:    "new durable preference",
			input:   personal.MemoryReviewInput{Event: personal.Event{ID: 40, Kind: personal.EventUser, Text: "I prefer concise answers."}},
			change:  personal.MemoryChange{Operation: "put", Key: "answer_style", Value: "concise", ExpectedVersion: 0, Evidence: "I prefer concise answers."},
			wantKey: "answer_style",
		},
		{
			name:    "correction keeps key and version",
			input:   personal.MemoryReviewInput{Event: personal.Event{ID: 41, Kind: personal.EventUser, Text: "Correction: I now prefer detailed answers."}, Memories: []personal.Memory{{Key: "answer_style", Value: "concise", Version: 3}}},
			change:  personal.MemoryChange{Operation: "put", Key: "answer_style", Value: "detailed", ExpectedVersion: 3, Evidence: "I now prefer detailed answers."},
			wantKey: "answer_style",
		},
		{
			name:    "reintroduces tombstone by its version",
			input:   personal.MemoryReviewInput{Event: personal.Event{ID: 42, Kind: personal.EventUser, Text: "Please remember again: I prefer concise answers."}, Memories: []personal.Memory{{Key: "answer_style", Value: "old forgotten value", Version: 4, Deleted: true}}},
			change:  personal.MemoryChange{Operation: "put", Key: "answer_style", Value: "concise", ExpectedVersion: 4, Evidence: "I prefer concise answers."},
			wantKey: "answer_style",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{memoryReviewResponse(personal.MemoryReview{Changes: []personal.MemoryChange{tc.change}})}}
			review, err := newMemoryReviewer(t, fixture).ReviewMemory(context.Background(), tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if len(review.Changes) != 1 || review.Changes[0].Key != tc.wantKey || review.Changes[0].ExpectedVersion != tc.change.ExpectedVersion || review.Changes[0].Evidence != tc.change.Evidence {
				t.Fatalf("review=%+v", review)
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
			if strings.Contains(instructions, "Persona style token") || strings.Contains(instructions, "Profile facts token") {
				t.Fatalf("memory review inherited persona/profile: %q", instructions)
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
