package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"go.uber.org/zap"

	"kagari/internal/logging"
	"kagari/internal/textpage"
)

const (
	maxMemories    = 40
	maxTaskRunes   = 2_000
	maxMemoryRunes = 1_500
)

// MainAgent handles the owner's conversation and manages tasks, schedules and memory.
type MainAgent struct {
	WorkspaceDirectory string
	Timezone           string
	Store              Repository
	Log                *zap.Logger
	Model              model.AgenticModel
	Persona            string
	Profile            string
	MaxIterations      int
	Timeout            time.Duration
}

type personalReplyKind string

const (
	replySilent  personalReplyKind = "silent"
	replyReceipt personalReplyKind = "receipt"
	replyContent personalReplyKind = "content"
)

type personalTurnResponse struct {
	Text        string            `json:"text"`
	ReplyKind   personalReplyKind `json:"reply_kind"`
	Attachments []string          `json:"attachments"`
}

func (a *MainAgent) Think(ctx context.Context, input TurnInput) (TurnOutput, error) {
	if a == nil || a.Store == nil || a.Model == nil {
		return TurnOutput{}, errors.New("personal agent is not configured")
	}
	if a.MaxIterations <= 0 {
		return TurnOutput{}, errors.New("personal agent max iterations must be positive")
	}
	if a.Timeout <= 0 {
		return TurnOutput{}, errors.New("personal agent timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	tools := &personalTools{timezone: a.Timezone, store: a.Store, log: a.Log, input: input, event: input.Event, taskIDs: make(map[int64]struct{})}
	registeredTools, err := tools.build()
	if err != nil {
		return TurnOutput{}, fmt.Errorf("build personal tools: %w", err)
	}
	replyTools, err := tools.replyTools()
	if err != nil {
		return TurnOutput{}, err
	}
	registeredTools = append(registeredTools, replyTools...)
	fileTools, err := attachmentTools(a.Store, nil, nil, nil)
	if err != nil {
		return TurnOutput{}, err
	}
	registeredTools = append(registeredTools, fileTools...)
	contextTools, err := (&contextTools{store: a.Store, scope: ReadScope{EventID: input.Event.ID}}).build()
	if err != nil {
		return TurnOutput{}, err
	}
	registeredTools = append(registeredTools, contextTools...)
	progressTools, err := progressTools(a.Store, ReadScope{EventID: input.Event.ID}, nil)
	if err != nil {
		return TurnOutput{}, err
	}
	registeredTools = append(registeredTools, progressTools...)
	workspaceTools, err := a.workspaceTools()
	if err != nil {
		return TurnOutput{}, err
	}
	registeredTools = append(registeredTools, workspaceTools...)
	budgetModel, err := modelWithSchema(a.Model, nil)
	if err != nil {
		return TurnOutput{}, err
	}
	ctx = textpage.WithMaxBytes(ctx, int(min(int64(textpage.MaxJSONBytes), budgetModel.Limits().Hard/4)))
	registeredTools = boundContextTools(registeredTools)
	budgetHandler, err := contextBudgetHandler(ctx, budgetModel, a.Store, ReadScope{EventID: input.Event.ID})
	if err != nil {
		return TurnOutput{}, err
	}
	memoryKeys := make([]string, 0, len(input.Memories))
	for _, m := range input.Memories {
		if !m.Deleted {
			memoryKeys = append(memoryKeys, m.Key)
		}
	}
	memoryContext, err := newRunMemoryContext(ctx, a.Store, Run{}, memoryKeys)
	if err != nil {
		return TurnOutput{}, err
	}
	memoryContext.limits = budgetModel.Limits()
	instruction := personalInstruction(a.Persona, a.Profile, input.Event.Kind == EventTask && input.Schedule != nil)
	view, err := assembleTurn(ctx, input, instruction, registeredTools, budgetModel.Limits())
	if err != nil {
		return TurnOutput{}, err
	}
	agent, err := adk.NewTypedChatModelAgent(ctx, &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: "personal_assistant", Description: "Answer the owner's message and manage personal tasks, schedules and memory.",
		Instruction: instruction, Model: budgetModel, MaxIterations: a.MaxIterations,
		Handlers:    []adk.TypedChatModelAgentMiddleware[*schema.AgenticMessage]{memoryContext, budgetHandler},
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: registeredTools, ExecuteSequentially: true}},
	})
	if err != nil {
		return TurnOutput{}, fmt.Errorf("create personal agent: %w", err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		return TurnOutput{}, fmt.Errorf("encode personal input: %w", err)
	}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: agent})
	observer := logging.NewCallbackObserver("think")
	modelCtx, cancelModel := context.WithCancel(ctx)
	// Some Responses endpoints suppress function calls when text.format also requires JSON Schema.
	// Validate the final response locally; the memory-only reviewer can still use strict wire output.
	output, err := consumePersonalEvents(runner.Query(modelCtx, string(encoded), adk.WithCallbacks(observer)))
	if err != nil {
		cancelModel()
	}
	observer.Wait()
	cancelModel()
	response, err := a.selectReply(ctx, input, tools, output, err)
	if err != nil {
		return TurnOutput{}, err
	}
	files, err := resolveAttachments(ctx, a.Store, response.Attachments)
	if err != nil {
		return TurnOutput{}, fmt.Errorf("reply attachment is unavailable: %w", err)
	}
	response.Attachments = response.Attachments[:0]
	for _, file := range files {
		response.Attachments = append(response.Attachments, file.ID)
	}
	notify := response.ReplyKind != replySilent
	if tools.mustNotify || taskOutcomeRequiresNotification(input) {
		notify = true
	}
	taskID, scheduleID := tools.taskID(), tools.scheduleID()
	if input.Event.Kind == EventTask {
		taskID, scheduleID = input.Event.TaskID, input.Event.ScheduleID
	} else if len(tools.scheduleIDs) > 1 {
		taskID = 0
	} else if taskID > 0 && scheduleID > 0 {
		info, err := a.Store.ScheduleForTask(ctx, taskID)
		if err != nil {
			return TurnOutput{}, err
		}
		if info == nil || info.Schedule.ID != scheduleID {
			taskID, scheduleID = 0, 0
		}
	}
	return TurnOutput{ScheduleID: scheduleID, Text: response.Text, TaskID: taskID, Silent: !notify, Attachments: response.Attachments}, nil
}

func personalJSONOutputOption(name string, schema map[string]any) model.Option {
	return agenticopenai.WithResponsesText(&responses.ResponseTextConfigParam{
		Format: responses.ResponseFormatTextConfigUnionParam{OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name: name, Strict: param.NewOpt(true), Schema: schema,
		}},
	})
}

func decodePersonalTurnResponse(raw string) (personalTurnResponse, error) {
	fields, err := strictJSONObject(raw, "text", "reply_kind", "attachments")
	if err != nil {
		return personalTurnResponse{}, fmt.Errorf("invalid personal response: %w", err)
	}
	var text *string
	var kind personalReplyKind
	if err := json.Unmarshal(fields["text"], &text); err != nil || text == nil {
		return personalTurnResponse{}, errors.New("invalid personal response text")
	}
	if err := json.Unmarshal(fields["reply_kind"], &kind); err != nil || (kind != replySilent && kind != replyReceipt && kind != replyContent) {
		return personalTurnResponse{}, errors.New("invalid personal response reply kind")
	}
	var attachments []string
	if err := json.Unmarshal(fields["attachments"], &attachments); err != nil || attachments == nil {
		return personalTurnResponse{}, errors.New("invalid personal response attachments")
	}
	for _, id := range attachments {
		if strings.TrimSpace(id) == "" {
			return personalTurnResponse{}, errors.New("blank attachment ID")
		}
	}
	if kind != replySilent && strings.TrimSpace(*text) == "" && len(attachments) == 0 {
		return personalTurnResponse{}, errors.New("personal response text is blank")
	}
	if kind != replyContent && len(attachments) > 0 {
		return personalTurnResponse{}, errors.New("only a content response can include attachments")
	}
	return personalTurnResponse{Text: *text, ReplyKind: kind, Attachments: attachments}, nil
}

func taskOutcomeRequiresNotification(input TurnInput) bool {
	if input.Event.Kind != EventTask {
		return false
	}
	for _, task := range input.Tasks {
		if task.ID == input.Event.TaskID && (task.Status == TaskCompleted || task.Status == TaskFailed || task.Status == TaskWaitingInput || task.Status == TaskWaitingBudget) {
			return true
		}
	}
	return false
}

func limitRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return s
}

func consumePersonalEvents(it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) (string, error) {
	var output string
	statusSeen := false
	responseStatus := ""
	for {
		event, ok := it.Next()
		if !ok {
			if statusSeen && responseStatus != "completed" {
				return "", errors.New("model response incomplete")
			}
			if strings.TrimSpace(output) == "" {
				return "", errors.New("model returned blank output")
			}
			return output, nil
		}
		if event.Err != nil {
			return "", fmt.Errorf("personal agent run: %w", event.Err)
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		variant := event.Output.MessageOutput
		if variant.AgenticRole == schema.AgenticRoleTypeAssistant {
			statusSeen, responseStatus = false, ""
		}
		msg, err := variant.GetMessage()
		if err != nil {
			return "", fmt.Errorf("personal agent message: %w", err)
		}
		if msg == nil || msg.Role != schema.AgenticRoleTypeAssistant {
			continue
		}
		if msg.ResponseMeta != nil && msg.ResponseMeta.OpenAIExtension != nil {
			status := string(msg.ResponseMeta.OpenAIExtension.Status)
			if status != "" {
				statusSeen, responseStatus = true, status
			}
			switch status {
			case "failed", "incomplete", "cancelled", "refused":
				return "", fmt.Errorf("model response %s", status)
			}
		}
		var text strings.Builder
		toolCall := false
		for _, block := range msg.ContentBlocks {
			if block == nil {
				continue
			}
			if block.FunctionToolCall != nil {
				toolCall = true
			}
			if block.AssistantGenText != nil {
				if block.AssistantGenText.OpenAIExtension != nil && block.AssistantGenText.OpenAIExtension.Refusal != nil {
					return "", errors.New("model response refused")
				}
				text.WriteString(block.AssistantGenText.Text)
			}
		}
		if !toolCall {
			output = text.String()
		}
	}
}

func strictJSONObject(raw string, expected ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	token, err := dec.Token()
	delim, ok := token.(json.Delim)
	if err != nil || !ok || delim != '{' {
		return nil, errors.New("expected object")
	}
	allowed := make(map[string]struct{}, len(expected))
	for _, key := range expected {
		allowed[key] = struct{}{}
	}
	fields := make(map[string]json.RawMessage, len(expected))
	for dec.More() {
		token, err = dec.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid object key")
		}
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("unexpected field %q", key)
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, errors.New("invalid field value")
		}
		fields[key] = value
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim('}') || len(fields) != len(expected) {
		return nil, errors.New("missing or malformed fields")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON data")
	}
	return fields, nil
}
