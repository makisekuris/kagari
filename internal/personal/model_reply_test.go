package personal

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDecodePersonalTurnNotificationContract(t *testing.T) {
	for _, raw := range []string{
		`{"text":"","notify":false,"attachments":[]}`,
		`{"text":"Internal progress","notify":false,"attachments":[]}`,
		`{"text":"Done.","notify":true,"attachments":[]}`,
		`{"text":"","notify":true,"attachments":["file-1"]}`,
	} {
		if _, err := decodePersonalTurnResponse(raw); err != nil {
			t.Errorf("decode %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"text":" ","notify":true,"attachments":[]}`,
		`{"text":"","notify":false,"attachments":["file-1"]}`,
	} {
		if _, err := decodePersonalTurnResponse(raw); err == nil {
			t.Errorf("decode accepted invalid response %s", raw)
		}
	}
}

func TestThinkCanReturnSilentEmptyDirectReply(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseText(`{"text":"","notify":false,"attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, newToolRepo()).Think(context.Background(), TurnInput{
		Event: Event{ID: 51, Kind: EventUser, Text: "A routine request with a real background continuation."},
	})
	if err != nil || !output.Silent || output.Text != "" || len(output.Attachments) != 0 {
		t.Fatalf("silent direct response=%+v error=%v", output, err)
	}
}

func TestThinkDoesNotNotifyOnlyBecauseTaskResultHasArtifacts(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseText(`{"text":"","notify":false,"attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, newToolRepo()).Think(context.Background(), TurnInput{
		Event:      Event{ID: 52, Kind: EventTask, TaskID: 7},
		CurrentRun: &Run{TaskID: 7, Result: `{"sources":[{}],"files":["report.md"],"attachments":[{}]}`},
	})
	if err != nil || !output.Silent || output.Text != "" || len(output.Attachments) != 0 {
		t.Fatalf("artifact-only callback response=%+v error=%v", output, err)
	}
}

func TestThinkReportsExplicitMutationFailure(t *testing.T) {
	for _, name := range []string{"cancel_task", "memory_put", "memory_forget"} {
		t.Run(name, func(t *testing.T) {
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{
				responseTool(name, map[string]any{}, "invalid-action"),
				responseText(`{"text":"这项操作未能完成，请补充必要信息。","notify":false,"attachments":[]}`),
			}}
			output, err := newMainAgent(t, fixture, newToolRepo()).Think(context.Background(), TurnInput{
				Event: Event{ID: 53, Kind: EventUser, Text: "请执行我要求的操作。"},
			})
			if err != nil || output.Silent || output.Text == "" {
				t.Fatalf("explicit failure was hidden: %+v, %v", output, err)
			}
		})
	}
}

func TestThinkSuppressesBareLinkQueueReceiptUnlessActionNeedsReply(t *testing.T) {
	for _, outcome := range []string{"queued", "failed_explicit_action", "denied_callback_action"} {
		t.Run(outcome, func(t *testing.T) {
			failed := outcome == "failed_explicit_action"
			ctx := context.Background()
			s := testStore(t, filepath.Join(t.TempDir(), "reply.db"), time.Minute)
			event := openUserEvent(t, s, 1, 1, "https://example.org/page")
			responses := []fixtureResponse{responseTool("create_task", createTaskArgs{Title: "初步阅读", Goal: "Read the supplied page", Context: event.Text}, "read")}
			if failed {
				responses = append(responses, responseTool("memory_forget", memoryForgetArgs{}, "failed-forget"))
			} else if outcome == "denied_callback_action" {
				responses = append(responses, responseTool("settle_task", settleTaskArgs{TaskID: 1, ExpectedRevision: 1, Status: TaskWaitingInput, Summary: "No run result exists yet."}, "premature-settle"))
			}
			responses = append(responses, responseText(`{"text":"已安排阅读，稍后回复。","notify":true,"attachments":[]}`))
			output, err := newMainAgent(t, &responsesFixture{t: t, responses: responses}, s).Think(ctx, livePersonalInput(t, ctx, s, event))
			if err != nil || output.Silent == failed || (!failed && output.Text != "") {
				t.Fatalf("receipt policy failed: output=%+v error=%v toolFailed=%v", output, err, failed)
			}
		})
	}
}
