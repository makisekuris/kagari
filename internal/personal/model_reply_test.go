package personal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodePersonalTurnNotificationContract(t *testing.T) {
	for _, raw := range []string{
		`{"text":"","reply_kind":"silent","attachments":[]}`,
		`{"text":"Internal progress","reply_kind":"silent","attachments":[]}`,
		`{"text":"Done.","reply_kind":"content","attachments":[]}`,
		`{"text":"Queued.","reply_kind":"receipt","attachments":[]}`,
		`{"text":"","reply_kind":"content","attachments":["file-1"]}`,
	} {
		if _, err := decodePersonalTurnResponse(raw); err != nil {
			t.Errorf("decode %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"text":" ","reply_kind":"content","attachments":[]}`,
		`{"text":"","reply_kind":"silent","attachments":["file-1"]}`,
		`{"text":"Queued.","reply_kind":"receipt","attachments":["file-1"]}`,
		`{"text":"","reply_kind":"receipt","attachments":[]}`,
		`{"text":"Hello.","reply_kind":"unknown","attachments":[]}`,
		`{"text":"Hello.","reply_kind":null,"attachments":[]}`,
		`{"text":"Hello.","reply_kind":true,"attachments":[]}`,
	} {
		if _, err := decodePersonalTurnResponse(raw); err == nil {
			t.Errorf("decode accepted invalid response %s", raw)
		}
	}
}

func TestThinkCanReturnSilentEmptyDirectReply(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseText(`{"text":"","reply_kind":"silent","attachments":[]}`),
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
		responseText(`{"text":"","reply_kind":"silent","attachments":[]}`),
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
				responseText(`{"text":"这项操作未能完成，请补充必要信息。","reply_kind":"silent","attachments":[]}`),
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
	for _, outcome := range []string{"queued", "failed_explicit_action", "recovered_input_error"} {
		t.Run(outcome, func(t *testing.T) {
			failed := outcome == "failed_explicit_action"
			ctx := context.Background()
			s := testStore(t, filepath.Join(t.TempDir(), "reply.db"), time.Minute)
			event := openUserEvent(t, s, 1, 1, "https://example.org/page")
			responses := []fixtureResponse{responseTool("create_task", createTaskArgs{Title: "初步阅读", Goal: "Read the supplied page", Context: event.Text}, "read")}
			if failed {
				responses = append(responses, responseTool("memory_forget", memoryForgetArgs{}, "failed-forget"))
			} else if outcome == "recovered_input_error" {
				responses = append(responses,
					responseTool("add_task_input", addTaskInputArgs{ActionID: "add-material", TaskID: 1, ExpectedRevision: 1}, "invalid-input"),
					responseTool("add_task_input", addTaskInputArgs{ActionID: "add-material", TaskID: 1, ExpectedRevision: 1, InputEventIDs: []int64{event.ID}}, "corrected-input"))
			}
			responses = append(responses, responseText(`{"text":"已安排阅读，稍后回复。","reply_kind":"receipt","attachments":[]}`))
			output, err := newMainAgent(t, &responsesFixture{t: t, responses: responses}, s).Think(ctx, livePersonalInput(t, ctx, s, event))
			if err != nil || output.Silent == failed || (!failed && output.Text != "") {
				t.Fatalf("receipt policy failed: output=%+v error=%v toolFailed=%v", output, err, failed)
			}
		})
	}
}

func TestThinkPreservesNecessaryReplyAfterPartialQueue(t *testing.T) {
	for _, partialFailure := range []bool{true, false} {
		name := "clarification"
		if partialFailure {
			name = "partial_failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Hour)
			event := openUserEvent(t, s, 1, 1, "https://example.com/one https://example.org/two")
			responses := []fixtureResponse{responseTool("create_task", createTaskArgs{Title: "First page", Goal: "Read https://example.com/one", Context: "Only the first supplied page."}, "first")}
			answer := "第一个链接继续阅读；第二个链接需要访问权限，请提供可公开访问的版本。"
			if partialFailure {
				responses = append(responses, responseTool("create_task", createTaskArgs{Title: "Second page", Context: "https://example.org/two"}, "second"))
				answer = "第二个链接的任务未能创建，需要补充目标；第一个链接仍会继续阅读。"
			} else {
				responses = append(responses, responseTool("set_user_questions", setUserQuestionsArgs{Questions: &[]string{answer}}, "question"))
			}
			responses = append(responses, responseText(`{"text":"`+answer+`","reply_kind":"content","attachments":[]}`))
			output, err := newMainAgent(t, &responsesFixture{t: t, responses: responses}, s).Think(ctx, livePersonalInput(t, ctx, s, event))
			if err != nil {
				t.Fatal(err)
			}
			if output.Silent || (!partialFailure && !strings.Contains(output.Text, answer)) || (partialFailure && (!strings.Contains(output.Text, "Second page") || !strings.Contains(output.Text, "未能创建任务"))) {
				t.Fatalf("necessary reply erased after partial queue: %+v", output)
			}
		})
	}
}
