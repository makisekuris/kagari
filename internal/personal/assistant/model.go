package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/logging"
	"kagari/internal/personal"
	"kagari/internal/personal/agentctx"
	"kagari/internal/textpage"
)

const (
	maxMemories          = 40
	maxMemoryRunes       = 1_500
	maxInterimReplyRunes = 200
)

// MainAgent handles the owner's conversation and manages tasks, schedules and memory.
type MainAgent struct {
	WorkspaceDirectory string
	Timezone           string
	Store              Repository
	Log                *zap.Logger
	Model              model.AgenticModel
	MultimodalModel    model.AgenticModel
	Persona            string
	Profile            string
	MaxIterations      int
	Timeout            time.Duration
}

type personalTurnResponse struct {
	Text                            string                      `json:"text"`
	ReplyKind                       personal.ReplyKind          `json:"reply_kind"`
	Attachments                     []string                    `json:"attachments"`
	SkipReplyPresentation           bool                        `json:"-"`
	SupplementalReply               *personal.SupplementalReply `json:"-"`
	SupplementalReplyFailureMessage string                      `json:"-"`
}

const incompleteSupplementalReply = "本轮其他查询或回复未能完成。"

func (a *MainAgent) Think(ctx context.Context, input personal.TurnInput) (personal.TurnOutput, error) {
	if a == nil || a.Store == nil || a.Model == nil {
		return personal.TurnOutput{}, errors.New("personal agent is not configured")
	}
	if a.MaxIterations <= 0 {
		return personal.TurnOutput{}, errors.New("personal agent max iterations must be positive")
	}
	if a.Timeout <= 0 {
		return personal.TurnOutput{}, errors.New("personal agent timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	tools := &personalTools{timezone: a.Timezone, store: a.Store, log: a.Log, input: input, event: input.Event, taskIDs: make(map[int64]struct{})}
	registeredTools, err := tools.build()
	if err != nil {
		return personal.TurnOutput{}, fmt.Errorf("build personal tools: %w", err)
	}
	replyTools, err := tools.replyTools()
	if err != nil {
		return personal.TurnOutput{}, err
	}
	registeredTools = append(registeredTools, replyTools...)
	var createdAttachments []channel.Attachment
	fileTools, err := agentctx.AttachmentTools(a.Store, nil, func(file channel.Attachment) {
		createdAttachments = append(createdAttachments, file)
	}, nil, nil)
	if err != nil {
		return personal.TurnOutput{}, err
	}
	registeredTools = append(registeredTools, fileTools...)
	if a.MultimodalModel != nil && input.Event.Kind == personal.EventUser {
		if allowed := turnImageAttachments(input); len(allowed) > 0 {
			imageTool, err := agentctx.ImageAnalysisTool(a.Store, a.MultimodalModel, allowed, nil)
			if err != nil {
				return personal.TurnOutput{}, err
			}
			registeredTools = append(registeredTools, imageTool)
		}
	}
	readTools, err := agentctx.ReadTools(agentctx.ReadToolsConfig{Store: a.Store, Scope: personal.ReadScope{EventID: input.Event.ID}})
	if err != nil {
		return personal.TurnOutput{}, err
	}
	registeredTools = append(registeredTools, readTools...)
	progressTools, err := agentctx.ProgressTools(agentctx.ProgressToolsConfig{Store: a.Store, Scope: personal.ReadScope{EventID: input.Event.ID}})
	if err != nil {
		return personal.TurnOutput{}, err
	}
	registeredTools = append(registeredTools, progressTools...)
	workspaceTools, err := a.workspaceTools()
	if err != nil {
		return personal.TurnOutput{}, err
	}
	registeredTools = append(registeredTools, workspaceTools...)
	budgetModel, err := agentctx.ModelWithSchema(a.Model, nil)
	if err != nil {
		return personal.TurnOutput{}, err
	}
	ctx = textpage.WithMaxBytes(ctx, int(min(int64(textpage.MaxJSONBytes), budgetModel.Limits().Hard/4)))
	registeredTools = agentctx.BoundTools(registeredTools)
	budgetHandler, err := agentctx.BudgetHandler(ctx, budgetModel, a.Store, personal.ReadScope{EventID: input.Event.ID})
	if err != nil {
		return personal.TurnOutput{}, err
	}
	memoryKeys := make([]string, 0, len(input.Memories))
	for _, m := range input.Memories {
		if !m.Deleted {
			memoryKeys = append(memoryKeys, m.Key)
		}
	}
	memoryContext, err := agentctx.NewMemoryContext(ctx, agentctx.MemoryContextConfig{Store: a.Store, Run: personal.Run{}, InitialKeys: memoryKeys, Limits: budgetModel.Limits()})
	if err != nil {
		return personal.TurnOutput{}, err
	}
	instruction := personalInstruction(a.Persona, a.Profile, input.Event.Kind == personal.EventTask && input.Schedule != nil)
	view, err := assembleTurn(ctx, input, instruction, registeredTools, budgetModel.Limits())
	if err != nil {
		return personal.TurnOutput{}, err
	}
	agent, err := adk.NewTypedChatModelAgent(ctx, &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: "personal_assistant", Description: "Answer the owner's message and manage personal tasks, schedules and memory.",
		Instruction: instruction, Model: budgetModel, MaxIterations: a.MaxIterations,
		Handlers:    []adk.TypedChatModelAgentMiddleware[*schema.AgenticMessage]{memoryContext, budgetHandler},
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: registeredTools, ExecuteSequentially: true}},
	})
	if err != nil {
		return personal.TurnOutput{}, fmt.Errorf("create personal agent: %w", err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		return personal.TurnOutput{}, fmt.Errorf("encode personal input: %w", err)
	}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: agent})
	observer := logging.NewCallbackObserver("think")
	modelCtx, cancelModel := context.WithCancel(ctx)
	// Some Responses endpoints suppress function calls when text.format also requires JSON Schema.
	// Validate the final response locally; the memory-only reviewer can still use strict wire output.
	var interim func(string) error
	if input.Event.Kind == personal.EventUser && input.OnInterimReply != nil {
		interim = func(text string) error { return input.OnInterimReply(ctx, text) }
	}
	output, err := consumePersonalEventsWithToolObserver(runner.Query(modelCtx, string(encoded), adk.WithCallbacks(observer)), func(name string) {
		if name != memoryCurrentAnswerToolName {
			tools.otherToolCalled = true
		}
	}, interim)
	if err != nil {
		cancelModel()
	}
	observer.Wait()
	cancelModel()
	response, err := a.selectReply(ctx, input, tools, output, createdAttachments, err)
	if err != nil {
		return personal.TurnOutput{}, err
	}
	if tools.persistedControl && response.SupplementalReply != nil {
		files, err := agentctx.ResolveAttachments(ctx, a.Store, response.SupplementalReply.Draft.Attachments)
		if err != nil {
			logging.FromContext(ctx).Warn("conversation.reply.supplemental_attachment_failed", append(logging.ErrorFields(err), zap.Int64("event_id", input.Event.ID))...)
			response.SupplementalReply = nil
			response.SupplementalReplyFailureMessage = incompleteSupplementalReply
		} else {
			attachments := make([]string, 0, len(files))
			for _, file := range files {
				attachments = append(attachments, file.ID)
			}
			response.SupplementalReply.Draft.Attachments = attachments
		}
	} else if tools.settledDelivery == nil && !tools.persistedControl {
		files, err := agentctx.ResolveAttachments(ctx, a.Store, response.Attachments)
		if err != nil {
			return personal.TurnOutput{}, fmt.Errorf("reply attachment is unavailable: %w", err)
		}
		response.Attachments = response.Attachments[:0]
		for _, file := range files {
			response.Attachments = append(response.Attachments, file.ID)
		}
	}
	notify := response.ReplyKind != personal.ReplySilent
	if tools.mustNotify || taskOutcomeRequiresNotification(input) {
		notify = true
	}
	taskID, scheduleID := tools.taskID(), tools.scheduleID()
	if tools.persistedControl {
		// Saved controls own their own scopes. Supplemental task/schedule scopes
		// are carried separately so they cannot be paired with a control target.
		taskID, scheduleID = 0, 0
	} else if input.Event.Kind == personal.EventTask {
		taskID, scheduleID = input.Event.TaskID, input.Event.ScheduleID
	} else if len(tools.scheduleIDs) > 1 {
		taskID = 0
	} else if taskID > 0 && scheduleID > 0 {
		info, err := a.Store.ScheduleForTask(ctx, taskID)
		if err != nil {
			return personal.TurnOutput{}, err
		}
		if info == nil || info.Schedule.ID != scheduleID {
			taskID, scheduleID = 0, 0
		}
	}
	requiredFeedback := ""
	if tools.persistedControl && tools.settledDelivery == nil {
		requiredFeedback = tools.requiredTaskReply()
		if response.SupplementalReplyFailureMessage != "" {
			if requiredFeedback != "" {
				requiredFeedback += "\n\n"
			}
			requiredFeedback += response.SupplementalReplyFailureMessage
		}
	}
	purpose := personal.DeliveryPurpose("")
	if tools.deliveryQueried && !tools.taskResultCommitted && !tools.persistedControl && tools.settledDelivery == nil {
		purpose = personal.DeliveryControl
	}
	taskIDs := tools.taskIDsSorted()
	if len(taskIDs) > 1 {
		taskID = 0
	}
	return personal.TurnOutput{ScheduleID: scheduleID, Text: response.Text, TaskID: taskID, TaskIDs: taskIDs, RequiredFeedback: requiredFeedback, SupplementalReply: response.SupplementalReply,
		CommittedActionsNeedReceipt: tools.persistedControl && response.SupplementalReplyFailureMessage != "",
		SkipReplyPresentation:       response.SkipReplyPresentation,
		Purpose:                     purpose, Silent: !notify, Attachments: response.Attachments, ReplyKind: response.ReplyKind}, nil
}

func taskOutcomeRequiresNotification(input personal.TurnInput) bool {
	if input.Event.Kind != personal.EventTask {
		return false
	}
	for _, task := range input.Tasks {
		if task.ID == input.Event.TaskID && (task.Status == personal.TaskCompleted || task.Status == personal.TaskFailed || task.Status == personal.TaskWaitingInput || task.Status == personal.TaskWaitingBudget) {
			return true
		}
	}
	return false
}

func consumePersonalEvents(it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]], interim ...func(string) error) (string, error) {
	return consumePersonalEventsWithToolObserver(it, nil, interim...)
}

func consumePersonalEventsWithToolObserver(it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]], observeTool func(string), interim ...func(string) error) (string, error) {
	var output string
	interimSent := false
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
				if observeTool != nil {
					observeTool(block.FunctionToolCall.Name)
				}
			}
			if block.AssistantGenText != nil {
				if block.AssistantGenText.OpenAIExtension != nil && block.AssistantGenText.OpenAIExtension.Refusal != nil {
					return "", errors.New("model response refused")
				}
				text.WriteString(block.AssistantGenText.Text)
			}
		}
		if toolCall && !interimSent && len(interim) > 0 && interim[0] != nil {
			body := personalInterimReply(text.String())
			if body != "" {
				if err := interim[0](body); err != nil {
					return "", fmt.Errorf("publish interim reply: %w", err)
				}
				interimSent = true
			}
		}
		if !toolCall {
			output = text.String()
		}
	}
}

func personalInterimReply(raw string) string {
	response, err := decodePersonalTurnResponse(strings.TrimSpace(raw))
	if err != nil || response.ReplyKind != personal.ReplyContent || len(response.Attachments) > 0 {
		return ""
	}
	text := strings.TrimSpace(response.Text)
	if text == "" || utf8.RuneCountInString(text) > maxInterimReplyRunes {
		return ""
	}
	return text
}
