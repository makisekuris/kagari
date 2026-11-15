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

func TestThinkSuppressesQueueOnlyReceiptForOrdinaryRequest(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "请研究这份产品需求中最合适的实现方案。")
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_task", createTaskArgs{Title: "实现方案研究", Goal: "Research the best implementation for the supplied product request", Context: event.Text}, "research"),
		testkit.ResponseText(`{"text":"已安排，稍后给你结果。","reply_kind":"receipt","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, s).Think(ctx, personal.TurnInput{Event: event})
	if err != nil || !output.Silent || output.Text != "" {
		t.Fatalf("ordinary queue receipt was not silenced: output=%+v error=%v", output, err)
	}
}

func TestThinkPreservesInputGroundedContentAlongsideQueuedTask(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "现有系统用了 PostgreSQL 扩展，比较迁移到 MySQL 的成本与运维复杂度，并排一个研究任务。")
	const insight = "现有系统依赖 PostgreSQL 扩展，因此迁移到 MySQL 时需要先核对这些扩展能否替代，不能只比较数据库的部署成本。"
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("create_task", createTaskArgs{Title: "数据库比较", Goal: "Compare PostgreSQL and MySQL migration costs and operational complexity", Context: event.Text}, "research"),
		testkit.ResponseText(`{"text":"` + insight + `","reply_kind":"content","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, s).Think(ctx, personal.TurnInput{Event: event})
	if err != nil || output.Silent || output.Text != insight {
		t.Fatalf("input-grounded initial content was not preserved: output=%+v error=%v", output, err)
	}
}

func TestThinkPreservesRequiredQuestionAndFailureAlongsideQueueReceipt(t *testing.T) {
	for _, name := range []string{"required_question", "task_failure"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Minute)
			event := testkit.OpenUserEvent(t, s, 1, 1, "请研究并整理这项需求。")
			responses := []testkit.Response{}
			want := ""
			if name == "required_question" {
				want = "这项方案需要只比较开源选项吗？"
				responses = append(responses,
					testkit.ResponseTool("create_task", createTaskArgs{Title: "需求研究", Goal: "Research the supplied requirement"}, "research"),
					testkit.ResponseTool("set_user_questions", setUserQuestionsArgs{Questions: &[]string{want}}, "question"),
				)
			} else {
				want = "「需求研究」未能创建任务。"
				responses = append(responses, testkit.ResponseTool("create_task", createTaskArgs{Title: "需求研究"}, "failed-research"))
			}
			responses = append(responses, testkit.ResponseText(`{"text":"已收到，稍后给你结果。","reply_kind":"receipt","attachments":[]}`))
			output, err := newMainAgent(t, &testkit.ResponsesFixture{T: t, Responses: responses}, s).Think(ctx, personal.TurnInput{Event: event})
			if err != nil || output.Silent || !strings.Contains(output.Text, want) {
				t.Fatalf("required reply was lost: want=%q output=%+v error=%v", want, output, err)
			}
		})
	}
}

func TestThinkPreservesTaskStatusContentAlongsideQueuedTask(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	seedEvent := testkit.OpenUserEvent(t, s, 1, 1, "创建一项现有研究任务。")
	existing, _ := testkit.CreateTask(t, s, seedEvent, "existing", "现有研究", "Summarize the existing research", "")
	testkit.FinishEvent(t, s, seedEvent, personal.TurnOutput{Silent: true}, nil)
	event := testkit.OpenUserEvent(t, s, 2, 2, "查看现有研究的状态，并排一项新的方案研究。")
	const answer = "现有研究仍在排队；新的方案研究也已排队。"
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("get_task", taskArgs{TaskID: existing.ID}, "query-existing"),
		testkit.ResponseTool("create_task", createTaskArgs{Title: "方案研究", Goal: "Research the best implementation for the supplied product request"}, "research-new"),
		testkit.ResponseText(`{"text":"` + answer + `","reply_kind":"content","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, s).Think(ctx, personal.TurnInput{Event: event})
	if err != nil || output.Silent || output.Text != answer {
		t.Fatalf("task status content was hidden by the new queued task: output=%+v error=%v", output, err)
	}
}

func TestThinkSilencesReceiptAfterTaskReadAndSuccessfulContinuation(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	seedEvent := testkit.OpenUserEvent(t, s, 1, 1, "创建一项需要补充信息的研究任务。")
	existing, _ := testkit.CreateTask(t, s, seedEvent, "waiting", "待补充研究", "Research the requested topic", "")
	if _, err := s.SettleTask(ctx, seedEvent.ID, existing.ID, existing.Revision, personal.TaskSettlement{
		Status:   personal.TaskWaitingInput,
		Summary:  "等待用户补充研究范围。",
		Delivery: personal.DeliveryDraft{Text: "请补充研究范围。", Attachments: []string{}},
	}); err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, s, seedEvent, personal.TurnOutput{TaskID: existing.ID}, nil)
	event := testkit.OpenUserEvent(t, s, 2, 2, "按现有范围继续研究。")
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("get_task", taskArgs{TaskID: existing.ID}, "query-waiting"),
		testkit.ResponseTool("continue_task", continueTaskArgs{TaskID: existing.ID, ExpectedRevision: existing.Revision, Step: "Continue using the saved scope."}, "continue"),
		testkit.ResponseText(`{"text":"已继续安排，稍后回复。","reply_kind":"receipt","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, s).Think(ctx, personal.TurnInput{Event: event, Tasks: []personal.Task{existing}})
	if err != nil || !output.Silent || output.Text != "" {
		t.Fatalf("task read prerequisite caused a queue-only receipt: output=%+v error=%v", output, err)
	}
}

func TestThinkPreservesCancellationOutcomeAlongsideQueuedTask(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	seedEvent := testkit.OpenUserEvent(t, s, 1, 1, "创建一项待取消的研究任务。")
	existing, _ := testkit.CreateTask(t, s, seedEvent, "existing", "旧研究", "Summarize the existing research", "")
	testkit.FinishEvent(t, s, seedEvent, personal.TurnOutput{Silent: true}, nil)
	event := testkit.OpenUserEvent(t, s, 2, 2, "取消旧研究，并排一项新研究。")
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("cancel_task", cancelTaskArgs{TaskID: existing.ID, ExpectedRevision: existing.Revision}, "cancel-existing"),
		testkit.ResponseTool("create_task", createTaskArgs{Title: "新研究", Goal: "Research the requested topic"}, "research-new"),
		testkit.ResponseText(`{"text":"旧研究已取消，新研究已排队。","reply_kind":"receipt","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, s).Think(ctx, personal.TurnInput{Event: event})
	if err != nil || output.Silent || !strings.Contains(output.Text, "取消任务") || output.SupplementalReply != nil || output.RequiredFeedback != "" {
		t.Fatalf("queue receipt hid persisted cancellation outcome: output=%+v error=%v", output, err)
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
