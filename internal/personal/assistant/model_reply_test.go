package assistant

import (
	"context"
	"errors"
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

func TestSelectReplyExtractsStandaloneTrailingEnvelopeWithoutFormatter(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "English explanation and receipt",
			raw:  "I have registered the scheduled goal successfully.\n\n{\"text\":\"已登记每日练习。\",\"reply_kind\":\"receipt\",\"attachments\":[]}",
			want: "已登记每日练习。",
		},
		{
			name: "Chinese draft and receipt",
			raw:  "每日练习已安排，会在早上提醒你。\n\n{\"text\":\"每日练习已登记。\",\"reply_kind\":\"receipt\",\"attachments\":[]}",
			want: "每日练习已登记。",
		},
		{
			name: "pretty JSON with an internal blank line",
			raw:  "The scheduled goal is registered.\n\n{\n\"text\":\"每日练习已登记。\",\n\n\"reply_kind\":\"receipt\",\n\"attachments\":[]\n}",
			want: "每日练习已登记。",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Minute)
			event := testkit.OpenUserEvent(t, s, 1, 1, "请登记每日练习提醒。")
			fixture := &testkit.ResponsesFixture{T: t}
			response, err := newMainAgent(t, fixture, s).selectReply(ctx, personal.TurnInput{Event: event}, &personalTools{}, tc.raw, nil, nil)
			if err != nil || response.Text != tc.want || response.ReplyKind != personal.ReplyReceipt {
				t.Fatalf("response=%+v error=%v", response, err)
			}
			if len(fixture.Requests) != 0 {
				t.Fatalf("formatter requests=%d, want none", len(fixture.Requests))
			}
		})
	}
}

func TestSelectReplyExtractsSupplementalTrailingEnvelopeWithoutFormatter(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "检查已登记的提醒。")
	fixture := &testkit.ResponsesFixture{T: t}
	tools := &personalTools{
		persistedControl:    true,
		supplementalWork:    true,
		supplementalPurpose: personal.DeliveryControl,
		controlDeliveries:   []personal.DeliveryIntent{{Draft: personal.DeliveryDraft{Text: "提醒状态已确认。", Attachments: []string{}}}},
	}
	raw := "The scheduled goal was registered successfully.\n\n{\"text\":\"每日练习已登记。\",\"reply_kind\":\"content\",\"attachments\":[]}"
	response, err := newMainAgent(t, fixture, s).selectReply(ctx, personal.TurnInput{Event: event}, tools, raw, nil, nil)
	if err != nil || response.Text != "提醒状态已确认。" || response.SupplementalReply == nil {
		t.Fatalf("response=%+v error=%v", response, err)
	}
	if draft := response.SupplementalReply.Draft; draft.Text != "每日练习已登记。" || len(draft.Attachments) != 0 {
		t.Fatalf("supplemental draft=%+v", draft)
	}
	if len(fixture.Requests) != 0 {
		t.Fatalf("formatter requests=%d, want none", len(fixture.Requests))
	}
}

func TestSelectReplyFormatsAmbiguousTrailingEnvelope(t *testing.T) {
	const envelope = `{"text":"已登记每日练习。","reply_kind":"receipt","attachments":[]}`
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "multiple objects", raw: `Draft object: {"text":"older","reply_kind":"content","attachments":[]}` + "\n\n" + envelope},
		{name: "fenced code example", raw: "Example:\n```json\n" + envelope + "\n```\n\n" + envelope},
		{name: "Chinese example label", raw: "例如：\n\n" + envelope},
		{name: "English example label", raw: "For example:\n\n" + envelope},
		{name: "missing field", raw: "Reminder registered.\n\n" + `{"text":"已登记每日练习。","reply_kind":"receipt"}`},
		{name: "trailing explanation", raw: "Reminder registered.\n\n" + envelope + "\n\nThis is the final explanation."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
				testkit.ResponseText(`{"text":"formatter result","reply_kind":"content","attachments":[]}`),
			}}
			event := personal.Event{ID: 51, Kind: personal.EventUser, Text: "请登记每日练习提醒。"}
			response, err := newMainAgent(t, fixture, newToolRepo()).selectReply(context.Background(), personal.TurnInput{Event: event}, &personalTools{}, tc.raw, nil, nil)
			if err != nil || response.Text != "formatter result" || response.ReplyKind != personal.ReplyContent {
				t.Fatalf("response=%+v error=%v", response, err)
			}
			if len(fixture.Requests) != 1 {
				t.Fatalf("formatter requests=%d, want one", len(fixture.Requests))
			}
		})
	}
}

func TestSelectReplyReportsReasonAfterRepairFails(t *testing.T) {
	const raw = `{"text":"private original draft","reply_kind":"content","attachments":[],"private_unknown":"private value"}`
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseText(`{"text":"private repaired draft","reply_kind":"content"}`),
	}}
	response, err := newMainAgent(t, fixture, newToolRepo()).selectReply(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 52, Kind: personal.EventUser, Text: "请继续。"},
	}, &personalTools{}, raw, nil, nil)
	if err == nil || response.Text != "" {
		t.Fatalf("response=%+v error=%v, want failed format repair", response, err)
	}
	var formatErr *personal.ReplyFormatError
	if !errors.As(err, &formatErr) {
		t.Fatalf("error=%T %v, want ReplyFormatError", err, err)
	}
	if !strings.Contains(formatErr.Reason, "缺少") || !strings.Contains(formatErr.Reason, "attachments") {
		t.Fatalf("reason=%q, want the repair response's missing attachments field", formatErr.Reason)
	}
	message := personal.ReplyFormatMessage(err)
	if !strings.Contains(message, formatErr.Reason) || strings.Contains(message, "private original draft") || strings.Contains(message, "private_unknown") || strings.Contains(message, "private value") {
		t.Fatalf("user-facing message leaked model content or missed reason: %q", message)
	}
	if !strings.Contains(err.Error(), "unexpected field") || !strings.Contains(err.Error(), "missing or malformed fields") {
		t.Fatalf("format error lost validation diagnostics: %v", err)
	}
	if len(fixture.Requests) != 1 {
		t.Fatalf("formatter requests=%d, want one", len(fixture.Requests))
	}
}

func TestRefusedRepairRetainsTrailingEnvelopeValidationReason(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{testkit.ResponseRefusal()}}
	agent := newMainAgent(t, fixture, newToolRepo())
	raw := "Draft complete.\n\n" + `{"text":"ok","reply_kind":"content","attachments":[42]}`
	_, err := agent.decodeOrFormatTurn(context.Background(), raw, nil)
	if message := personal.ReplyFormatMessage(err); !strings.Contains(message, "附件列表（attachments）格式无效") {
		t.Fatalf("refused repair lost the invalid attachment list: message=%q error=%v", message, err)
	}
}

func TestSelectReplyAddsFormatFailureToRequiredTaskReply(t *testing.T) {
	const raw = `{"text":"已收到。","attachments":[]}`
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseText(`{"text":"修复失败。","attachments":[]}`),
	}}
	required := "请补充研究范围。"
	response, err := newMainAgent(t, fixture, newToolRepo()).selectReply(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 53, Kind: personal.EventUser, Text: "继续研究。"},
	}, &personalTools{questions: []string{required}}, raw, nil, nil)
	if err != nil {
		t.Fatalf("required reply returned error: %v", err)
	}
	if response.ReplyKind != personal.ReplyContent || !strings.HasPrefix(response.Text, required+"\n\n") || !strings.Contains(response.Text, "缺少回复类型（reply_kind）字段") || !response.SkipReplyPresentation {
		t.Fatalf("required reply did not carry the format explanation: %+v", response)
	}
}

func TestSelectReplyCarriesSupplementalFormatFailure(t *testing.T) {
	const raw = `{"text":"状态已确认。","attachments":[]}`
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseText(`{"text":"修复结果","reply_kind":"content"}`),
	}}
	tools := &personalTools{
		persistedControl:    true,
		supplementalWork:    true,
		supplementalPurpose: personal.DeliveryControl,
		controlDeliveries:   []personal.DeliveryIntent{{Draft: personal.DeliveryDraft{Text: "提醒状态已确认。", Attachments: []string{}}}},
	}
	response, err := newMainAgent(t, fixture, newToolRepo()).selectReply(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 54, Kind: personal.EventUser, Text: "检查提醒。"},
	}, tools, raw, nil, nil)
	if err != nil || response.Text != "提醒状态已确认。" || response.SupplementalReply != nil {
		t.Fatalf("control response=%+v error=%v", response, err)
	}
	if !strings.Contains(response.SupplementalReplyFailureMessage, "缺少附件列表（attachments）字段") {
		t.Fatalf("supplemental failure message=%q", response.SupplementalReplyFailureMessage)
	}
}

func TestSelectReplyDoesNotMarkInitialModelErrorAsFormatFailure(t *testing.T) {
	modelErr := errors.New("initial model request failed")
	fixture := &testkit.ResponsesFixture{T: t}
	_, err := newMainAgent(t, fixture, newToolRepo()).selectReply(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 55, Kind: personal.EventUser, Text: "你好。"},
	}, &personalTools{}, `not json`, nil, modelErr)
	if !errors.Is(err, modelErr) {
		t.Fatalf("error=%v, want initial model error", err)
	}
	var formatErr *personal.ReplyFormatError
	if errors.As(err, &formatErr) || len(fixture.Requests) != 0 {
		t.Fatalf("initial model error was treated as a format failure: error=%v requests=%d", err, len(fixture.Requests))
	}
}

type canDeferReplyErrorRepository struct {
	Repository
	err error
}

func (r canDeferReplyErrorRepository) CanDeferReply(context.Context, int64) (bool, error) {
	return false, r.err
}

func TestSelectReplyDoesNotMarkDatabaseErrorAsFormatFailure(t *testing.T) {
	databaseErr := errors.New("reply deferral query failed")
	fixture := &testkit.ResponsesFixture{T: t}
	_, err := newMainAgent(t, fixture, canDeferReplyErrorRepository{err: databaseErr}).selectReply(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 56, Kind: personal.EventUser, Text: "https://example.com"},
	}, &personalTools{}, `not json`, nil, nil)
	if !errors.Is(err, databaseErr) {
		t.Fatalf("error=%v, want database error", err)
	}
	var formatErr *personal.ReplyFormatError
	if errors.As(err, &formatErr) || len(fixture.Requests) != 0 {
		t.Fatalf("database error was treated as a format failure: error=%v requests=%d", err, len(fixture.Requests))
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
