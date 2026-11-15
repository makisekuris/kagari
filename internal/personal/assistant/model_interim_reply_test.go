package assistant

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestThinkDeliversVisibleToolTextAndContinuesWithoutExtraModelCall(t *testing.T) {
	const initial = "你给出的两个条件分别影响部署方式和维护成本，需要分开比较。"
	const final = "已有任务的目标同时包含部署限制和维护成本。"
	first := testkit.ResponseText(initial)
	first.Output = append(first.Output, testkit.ResponseTool("get_task", map[string]any{"task_id": 7}, "inspect").Output...)
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		first,
		testkit.ResponseText(`{"text":"` + final + `","reply_kind":"content","attachments":[]}`),
	}}
	repo := newToolRepo()
	repo.tasks[7] = personal.Task{ID: 7, Revision: 1, Title: "方案比较", Goal: "比较部署限制和维护成本", Status: personal.TaskRunning}
	var visible []string
	output, err := newMainAgent(t, fixture, repo).Think(context.Background(), personal.TurnInput{
		Event: personal.Event{ID: 81, Kind: personal.EventUser, Text: "检查比较任务是否考虑了部署限制和维护成本。"},
		OnInterimReply: func(_ context.Context, text string) error {
			visible = append(visible, text)
			return nil
		},
	})
	if err != nil || output.Silent || output.Text != final {
		t.Fatalf("final output=%+v error=%v", output, err)
	}
	if !reflect.DeepEqual(visible, []string{initial}) || len(fixture.Requests) != 2 {
		t.Fatalf("visible=%q model calls=%d, want the original text and two execution calls", visible, len(fixture.Requests))
	}
	if !strings.Contains(testkit.RequestInput(fixture.Requests[1]), "比较部署限制和维护成本") {
		t.Fatal("the next model call did not receive the tool result")
	}
}

func TestConsumePersonalEventsInterimReplySelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{name: "natural text", text: "离线要求会影响方案选择。", want: []string{"离线要求会影响方案选择。"}},
		{name: "tool only"},
		{name: "receipt envelope", text: `{"text":"已收到，稍后回复。","reply_kind":"receipt","attachments":[]}`},
		{name: "silent envelope", text: `{"text":"内部进度","reply_kind":"silent","attachments":[]}`},
		{name: "content envelope", text: `{"text":"离线要求会影响方案选择。","reply_kind":"content","attachments":[]}`, want: []string{"离线要求会影响方案选择。"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it, gen := adk.NewAsyncIteratorPair[*adk.TypedAgentEvent[*schema.AgenticMessage]]()
			gen.Send(adk.EventFromAgenticMessage(&schema.AgenticMessage{
				Role: schema.AgenticRoleTypeAssistant,
				ContentBlocks: []*schema.ContentBlock{
					{Reasoning: &schema.Reasoning{Text: "PRIVATE_REASONING"}},
					{AssistantGenText: &schema.AssistantGenText{Text: tc.text}},
					{FunctionToolCall: &schema.FunctionToolCall{Name: "get_task", Arguments: `{}`, CallID: "first"}},
				},
			}, nil, schema.AgenticRoleTypeAssistant))
			if len(tc.want) > 0 {
				gen.Send(adk.EventFromAgenticMessage(&schema.AgenticMessage{
					Role: schema.AgenticRoleTypeAssistant,
					ContentBlocks: []*schema.ContentBlock{
						{AssistantGenText: &schema.AssistantGenText{Text: "第二次工具调用的正文"}},
						{FunctionToolCall: &schema.FunctionToolCall{Name: "get_task", Arguments: `{}`, CallID: "second"}},
					},
				}, nil, schema.AgenticRoleTypeAssistant))
			}
			gen.Send(adk.EventFromAgenticMessage(&schema.AgenticMessage{
				Role:          schema.AgenticRoleTypeAssistant,
				ContentBlocks: []*schema.ContentBlock{{AssistantGenText: &schema.AssistantGenText{Text: "完整答案"}}},
			}, nil, schema.AgenticRoleTypeAssistant))
			gen.Close()
			var visible []string
			output, err := consumePersonalEvents(it, func(text string) error {
				visible = append(visible, text)
				return nil
			})
			if err != nil || output != "完整答案" || !reflect.DeepEqual(visible, tc.want) {
				t.Fatalf("output=%q visible=%q error=%v", output, visible, err)
			}
		})
	}
}

func TestConsumePersonalEventsStopsOnInterimPersistenceFailure(t *testing.T) {
	it, gen := adk.NewAsyncIteratorPair[*adk.TypedAgentEvent[*schema.AgenticMessage]]()
	gen.Send(adk.EventFromAgenticMessage(&schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			{AssistantGenText: &schema.AssistantGenText{Text: "部署条件需要分别核对。"}},
			{FunctionToolCall: &schema.FunctionToolCall{Name: "get_task", Arguments: `{}`, CallID: "first"}},
		},
	}, nil, schema.AgenticRoleTypeAssistant))
	gen.Close()
	want := errors.New("outbox unavailable")
	if _, err := consumePersonalEvents(it, func(string) error { return want }); !errors.Is(err, want) {
		t.Fatalf("error=%v, want persistence failure", err)
	}
}
