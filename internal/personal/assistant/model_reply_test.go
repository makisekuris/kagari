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
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseText(`{"text":"","reply_kind":"silent","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, newToolRepo()).Think(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 51, Kind: personal.EventUser, Text: "A routine request with a real background continuation."},
	})
	if err != nil || !output.Silent || output.Text != "" || len(output.Attachments) != 0 {
		t.Fatalf("silent direct response=%+v error=%v", output, err)
	}
}

func TestThinkDoesNotNotifyOnlyBecauseTaskResultHasArtifacts(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseText(`{"text":"","reply_kind":"silent","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, newToolRepo()).Think(context.Background(), personal.TurnInput{
		Event:      personal.Event{ID: 52, Kind: personal.EventTask, TaskID: 7},
		CurrentRun: &personal.Run{TaskID: 7, Result: `{"sources":[{}],"files":["report.md"],"attachments":[{}]}`},
	})
	if err != nil || !output.Silent || output.Text != "" || len(output.Attachments) != 0 {
		t.Fatalf("artifact-only callback response=%+v error=%v", output, err)
	}
}

func TestThinkReportsExplicitMutationFailure(t *testing.T) {
	for _, name := range []string{"cancel_task", "memory_put", "memory_forget"} {
		t.Run(name, func(t *testing.T) {
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
				testkit.ResponseTool(name, map[string]any{}, "invalid-action"),
				testkit.ResponseText(`{"text":"这项操作未能完成，请补充必要信息。","reply_kind":"silent","attachments":[]}`),
			}}
			output, err := newMainAgent(t, fixture, newToolRepo()).Think(context.Background(), personal.TurnInput{
				Event: personal.Event{ID: 53, Kind: personal.EventUser, Text: "请执行我要求的操作。"},
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
			s := testkit.Store(t, filepath.Join(t.TempDir(), "reply.db"), time.Minute)
			event := testkit.OpenUserEvent(t, s, 1, 1, "https://example.org/page")
			responses := []testkit.Response{testkit.ResponseTool("create_task", createTaskArgs{Title: "初步阅读", Goal: "Read the supplied page", Context: event.Text}, "read")}
			if failed {
				responses = append(responses, testkit.ResponseTool("memory_forget", memoryForgetArgs{}, "failed-forget"))
			} else if outcome == "recovered_input_error" {
				responses = append(responses,
					testkit.ResponseTool("add_task_input", addTaskInputArgs{ActionID: "add-material", TaskID: 1, ExpectedRevision: 1}, "invalid-input"),
					testkit.ResponseTool("add_task_input", addTaskInputArgs{ActionID: "add-material", TaskID: 1, ExpectedRevision: 1, InputEventIDs: []int64{event.ID}}, "corrected-input"))
			}
			responses = append(responses, testkit.ResponseText(`{"text":"已安排阅读，稍后回复。","reply_kind":"receipt","attachments":[]}`))
			output, err := newMainAgent(t, &testkit.ResponsesFixture{T: t, Responses: responses}, s).Think(ctx, personal.TurnInput{Event: event})
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
			s := testkit.Store(t, ":memory:", time.Hour)
			event := testkit.OpenUserEvent(t, s, 1, 1, "https://example.com/one https://example.org/two")
			responses := []testkit.Response{testkit.ResponseTool("create_task", createTaskArgs{Title: "First page", Goal: "Read https://example.com/one", Context: "Only the first supplied page."}, "first")}
			answer := "第一个链接继续阅读；第二个链接需要访问权限，请提供可公开访问的版本。"
			if partialFailure {
				responses = append(responses, testkit.ResponseTool("create_task", createTaskArgs{Title: "Second page", Context: "https://example.org/two"}, "second"))
				answer = "第二个链接的任务未能创建，需要补充目标；第一个链接仍会继续阅读。"
			} else {
				responses = append(responses, testkit.ResponseTool("set_user_questions", setUserQuestionsArgs{Questions: &[]string{answer}}, "question"))
			}
			responses = append(responses, testkit.ResponseText(`{"text":"`+answer+`","reply_kind":"content","attachments":[]}`))
			output, err := newMainAgent(t, &testkit.ResponsesFixture{T: t, Responses: responses}, s).Think(ctx, personal.TurnInput{Event: event})
			if err != nil {
				t.Fatal(err)
			}
			if output.Silent || (!partialFailure && !strings.Contains(output.Text, answer)) || (partialFailure && (!strings.Contains(output.Text, "Second page") || !strings.Contains(output.Text, "未能创建任务"))) {
				t.Fatalf("necessary reply erased after partial queue: %+v", output)
			}
		})
	}
}
