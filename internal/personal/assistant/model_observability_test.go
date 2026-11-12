package assistant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"kagari/internal/logging"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

// A real tool round trip verifies Eino's call ID propagation and distinguishes
// a business rejection from nil Go error, without spending an extra model call.
func TestThinkObservabilityCorrelatesToolOutcomeWithoutExtraRequests(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("get_task", taskArgs{TaskID: 9}, "get-task-9"),
		testkit.ResponseText(`{"text":"I could not find that task.","reply_kind":"content","attachments":[]}`),
	}}
	model := fixture.Model(t)
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := logging.WithLogger(context.Background(), zap.New(core).With(zap.Int64("event_id", 71)))
	agent := &MainAgent{
		Store: missingTaskRepository{}, Model: model, Persona: "fixture persona", Profile: "fixture profile",
		MaxIterations: 3, Timeout: 3 * time.Second,
	}
	output, err := agent.Think(ctx, personal.TurnInput{Event: personal.Event{ID: 71, Kind: personal.EventUser, Text: "Please look up task 9."}})
	if err != nil {
		t.Fatal(err)
	}
	if output.Text != "I could not find that task." || output.Silent {
		t.Fatalf("turn output = %+v", output)
	}
	if len(fixture.Requests) != 2 {
		t.Fatalf("model requests = %d; callback observation must not issue calls", len(fixture.Requests))
	}

	entries := logs.All()
	starts, finishes := events(entries, "model.started"), events(entries, "model.finished")
	if len(starts) != 2 || len(finishes) != 2 {
		t.Fatalf("model lifecycle events = starts %d finishes %d entries=%+v", len(starts), len(finishes), entries)
	}
	for i := range starts {
		startFields, finishFields := starts[i].ContextMap(), finishes[i].ContextMap()
		if startFields["model_call_id"] != finishFields["model_call_id"] || startFields["model_call_id"] == starts[1-i].ContextMap()["model_call_id"] {
			t.Fatalf("model call correlation is wrong: starts=%v finishes=%v", starts, finishes)
		}
		if startFields["event_id"] != int64(71) || startFields["phase"] != "think" || finishFields["usage_reported"] != true {
			t.Fatalf("model event metadata missing: start=%v finish=%v", startFields, finishFields)
		}
	}
	startPayload, _ := json.Marshal(starts[0].ContextMap()["payload"])
	finishPayload, _ := json.Marshal(finishes[1].ContextMap()["payload"])
	if !strings.Contains(string(startPayload), "Please look up task 9.") || !strings.Contains(string(startPayload), "get_task") || !strings.Contains(string(finishPayload), "I could not find that task.") {
		t.Fatalf("full model payload missing: input=%s output=%s", startPayload, finishPayload)
	}

	toolStart := findEvent(t, entries, "tool.started")
	toolFinish := findEvent(t, entries, "tool.finished")
	if toolStart.ContextMap()["tool_call_id"] != "get-task-9" || toolFinish.ContextMap()["tool_call_id"] != "get-task-9" {
		t.Fatalf("tool call ID was not retained: start=%v finish=%v", toolStart.ContextMap(), toolFinish.ContextMap())
	}
	if toolFinish.ContextMap()["outcome"] != "business_failure" {
		t.Fatalf("nil Go error with ok:false must be a business failure: %v", toolFinish.ContextMap())
	}
	payload, _ := json.Marshal(toolFinish.ContextMap()["payload"])
	if !strings.Contains(string(payload), "task_id") || !strings.Contains(string(payload), "task not found") {
		t.Fatalf("tool input/output payload incomplete: %s", payload)
	}
}

type missingTaskRepository struct{ Repository }

func (missingTaskRepository) Task(context.Context, int64) (*personal.Task, error) { return nil, nil }

func events(entries []observer.LoggedEntry, message string) []observer.LoggedEntry {
	var found []observer.LoggedEntry
	for _, entry := range entries {
		if entry.Message == message {
			found = append(found, entry)
		}
	}
	return found
}

func findEvent(t *testing.T, entries []observer.LoggedEntry, message string) observer.LoggedEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.Message == message {
			return entry
		}
	}
	t.Fatalf("event %q not found: %v", message, entries)
	return observer.LoggedEntry{}
}
