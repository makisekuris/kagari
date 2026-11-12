package assistant

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestPresentReplyUsesOnlyDraftAndOwnerRequest(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "presentation.db"), time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "Please report the final price.")
	task, _ := testkit.CreateTask(t, s, event, "research", "Price check", "Check the displayed monthly price.", "PRIVATE_TASK_CONTEXT")
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseText(`{"omitted_text":"Internal draft:","text":"The monthly price is 17 USD."}`),
		testkit.ResponseText(`{"omitted_text":"Internal draft:","text":"The monthly price is 17 USD."}`),
	}}
	a := &MainAgent{Model: fixture.Model(t), Store: s, Timeout: time.Second, Profile: "PRIVATE_PROFILE", Persona: "PRIVATE_PERSONA"}

	cases := []struct {
		event personal.Event
		draft string
		want  string
	}{
		{event: event, draft: "Internal draft: the monthly price is 17 USD.", want: "Please report the final price."},
		{event: personal.Event{Kind: personal.EventTask, TaskID: task.ID}, draft: "Internal draft: the monthly price is 17 USD.", want: task.Goal},
	}
	for _, tc := range cases {
		if text, err := a.PresentReply(ctx, tc.event, tc.draft); err != nil || text != "The monthly price is 17 USD." {
			t.Fatalf("PresentReply() = (%q, %v)", text, err)
		}
	}
	if len(fixture.Requests) != 2 {
		t.Fatalf("presenter requests = %d, want 2", len(fixture.Requests))
	}
	for i, request := range fixture.Requests {
		assertStrictSchema(t, request, "personal_presentation")
		if tools, _ := request["tools"].([]any); len(tools) != 0 {
			t.Fatalf("presenter request %d received %d tools", i, len(tools))
		}
		input := testkit.RequestInput(request)
		if !strings.Contains(input, cases[i].want) || !strings.Contains(input, "Internal draft") {
			t.Fatalf("presenter request %d omitted its draft or owner request: %s", i, input)
		}
		for _, private := range []string{"PRIVATE_PROFILE", "PRIVATE_PERSONA", "PRIVATE_TASK_CONTEXT"} {
			if strings.Contains(input, private) {
				t.Fatalf("presenter request %d included unrelated context %q", i, private)
			}
		}
	}
}
