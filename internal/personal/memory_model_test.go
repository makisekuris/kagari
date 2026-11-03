package personal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func memoryReviewResponse(review MemoryReview) fixtureResponse {
	encoded, _ := json.Marshal(review)
	return responseText(string(encoded))
}

func newMemoryReviewer(t *testing.T, fixture *responsesFixture) *MainAgent {
	t.Helper()
	return &MainAgent{Model: fixture.model(t), Timeout: 4 * time.Second}
}

func TestReviewMemoryExtractsCurrentFactsAndUsesExistingVersion(t *testing.T) {
	tests := []struct {
		name    string
		input   MemoryReviewInput
		change  MemoryChange
		wantKey string
	}{
		{
			name:    "new durable preference",
			input:   MemoryReviewInput{Event: Event{ID: 40, Kind: EventUser, Text: "I prefer concise answers."}},
			change:  MemoryChange{Operation: "put", Key: "answer_style", Value: "concise", ExpectedVersion: 0, Evidence: "I prefer concise answers."},
			wantKey: "answer_style",
		},
		{
			name:    "correction keeps key and version",
			input:   MemoryReviewInput{Event: Event{ID: 41, Kind: EventUser, Text: "Correction: I now prefer detailed answers."}, Memories: []Memory{{Key: "answer_style", Value: "concise", Version: 3}}},
			change:  MemoryChange{Operation: "put", Key: "answer_style", Value: "detailed", ExpectedVersion: 3, Evidence: "I now prefer detailed answers."},
			wantKey: "answer_style",
		},
		{
			name:    "reintroduces tombstone by its version",
			input:   MemoryReviewInput{Event: Event{ID: 42, Kind: EventUser, Text: "Please remember again: I prefer concise answers."}, Memories: []Memory{{Key: "answer_style", Value: "old forgotten value", Version: 4, Deleted: true}}},
			change:  MemoryChange{Operation: "put", Key: "answer_style", Value: "concise", ExpectedVersion: 4, Evidence: "I prefer concise answers."},
			wantKey: "answer_style",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{memoryReviewResponse(MemoryReview{Changes: []MemoryChange{tc.change}})}}
			review, err := newMemoryReviewer(t, fixture).ReviewMemory(context.Background(), tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if len(review.Changes) != 1 || review.Changes[0].Key != tc.wantKey || review.Changes[0].ExpectedVersion != tc.change.ExpectedVersion || review.Changes[0].Evidence != tc.change.Evidence {
				t.Fatalf("review=%+v", review)
			}
			schema := assertStrictSchema(t, fixture.requests[0], "personal_memory_review")
			properties := schema["properties"].(map[string]any)
			changeSchema := properties["changes"].(map[string]any)["items"].(map[string]any)
			if changeSchema["additionalProperties"] != false {
				t.Fatalf("memory change schema is not closed: %#v", changeSchema)
			}
			inputText := requestInput(fixture.requests[0])
			if tc.input.Event.ID == 42 && strings.Contains(inputText, "old forgotten value") {
				t.Fatalf("review input included tombstone value: %s", inputText)
			}
			instructions, _ := fixture.requests[0]["instructions"].(string)
			if strings.Contains(instructions, "Persona style token") || strings.Contains(instructions, "Profile facts token") {
				t.Fatalf("memory review inherited persona/profile: %q", instructions)
			}
		})
	}
}

func TestReviewMemoryReturnsEmptyForNoFactsAndRejectsNonUserEvents(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{memoryReviewResponse(MemoryReview{Changes: []MemoryChange{}})}}
	review, err := newMemoryReviewer(t, fixture).ReviewMemory(context.Background(), MemoryReviewInput{
		Event: Event{ID: 50, Kind: EventUser, Text: "Thanks, that is useful."},
	})
	if err != nil || len(review.Changes) != 0 || review.Question != "" {
		t.Fatalf("review=%+v err=%v", review, err)
	}
	if len(fixture.requests) != 1 {
		t.Fatalf("requests=%d, want 1", len(fixture.requests))
	}

	nonUser := &responsesFixture{t: t}
	if _, err := newMemoryReviewer(t, nonUser).ReviewMemory(context.Background(), MemoryReviewInput{Event: Event{ID: 51, Kind: EventMaterial, Text: "I live in Tokyo."}}); err == nil {
		t.Fatal("material event was accepted as memory evidence")
	}
	if len(nonUser.requests) != 0 {
		t.Fatalf("non-user event reached model: %d requests", len(nonUser.requests))
	}

	empty := &responsesFixture{t: t}
	review, err = newMemoryReviewer(t, empty).ReviewMemory(context.Background(), MemoryReviewInput{Event: Event{ID: 52, Kind: EventUser}})
	if err != nil || len(review.Changes) != 0 || len(empty.requests) != 0 {
		t.Fatalf("empty event review=%+v err=%v requests=%d", review, err, len(empty.requests))
	}
}

func TestReviewMemoryRejectsUnprovenOrStaleChanges(t *testing.T) {
	tests := []struct {
		name   string
		input  MemoryReviewInput
		change MemoryChange
	}{
		{
			name:   "evidence outside current message",
			input:  MemoryReviewInput{Event: Event{ID: 60, Kind: EventUser, Text: "I prefer concise answers."}},
			change: MemoryChange{Operation: "put", Key: "answer_style", Value: "detailed", ExpectedVersion: 0, Evidence: "I prefer detailed answers."},
		},
		{
			name:   "stale existing version",
			input:  MemoryReviewInput{Event: Event{ID: 61, Kind: EventUser, Text: "I now prefer detailed answers."}, Memories: []Memory{{Key: "answer_style", Value: "concise", Version: 3}}},
			change: MemoryChange{Operation: "put", Key: "answer_style", Value: "detailed", ExpectedVersion: 2, Evidence: "I now prefer detailed answers."},
		},
		{
			name:   "automatic forget forbidden",
			input:  MemoryReviewInput{Event: Event{ID: 62, Kind: EventUser, Text: "Forget answer style."}},
			change: MemoryChange{Operation: "forget", Key: "answer_style", ExpectedVersion: 1, Evidence: "Forget answer style."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{memoryReviewResponse(MemoryReview{Changes: []MemoryChange{tc.change}})}}
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
		resp fixtureResponse
	}{
		{name: "extra field", resp: responseText(`{"changes":[],"question":"","extra":true}`)},
		{name: "refusal", resp: responseRefusal()},
		{name: "incomplete", resp: fixtureResponse{status: "incomplete", output: responseText(`{"changes":[],"question":""}`).output}},
		{name: "blank", resp: responseText(" ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{tc.resp}}
			_, err := newMemoryReviewer(t, fixture).ReviewMemory(context.Background(), MemoryReviewInput{Event: Event{ID: 70, Kind: EventUser, Text: "I like short answers."}})
			if err == nil {
				t.Fatal("invalid output was accepted")
			}
		})
	}
}
