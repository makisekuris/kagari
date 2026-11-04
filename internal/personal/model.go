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
)

const (
	maxHistoryMessages = 24
	maxTasks           = 30
	maxMemories        = 40
	maxEventRunes      = 16_000
	maxHistoryRunes    = 2_000
	maxTaskRunes       = 2_000
	maxMemoryRunes     = 1_500
)

// MainAgent handles the owner's conversation and manages tasks and memory.
type MainAgent struct {
	Store         Repository
	Log           *zap.Logger
	Model         model.AgenticModel
	Persona       string
	Profile       string
	MaxIterations int
	Timeout       time.Duration
}

type personalTurnResponse struct {
	Text   string `json:"text"`
	Notify bool   `json:"notify"`
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

	tools := &personalTools{store: a.Store, log: a.Log, input: input, event: input.Event, taskIDs: make(map[int64]struct{})}
	registeredTools, err := tools.build()
	if err != nil {
		return TurnOutput{}, fmt.Errorf("build personal tools: %w", err)
	}
	agent, err := adk.NewTypedChatModelAgent(ctx, &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: "personal_assistant", Description: "Answer the owner's message and manage personal tasks and memory.",
		Instruction: personalInstruction(a.Persona, a.Profile), Model: a.Model, MaxIterations: a.MaxIterations,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: registeredTools, ExecuteSequentially: true}},
	})
	if err != nil {
		return TurnOutput{}, fmt.Errorf("create personal agent: %w", err)
	}
	encoded, err := json.Marshal(boundedTurnInput(input))
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
	if err != nil {
		return TurnOutput{}, err
	}
	response, err := decodePersonalTurnResponse(output)
	if err != nil {
		// Formatting gets one tool-free attempt; committed actions are never replayed.
		response, err = a.formatTurn(ctx, output)
	}
	if err != nil {
		return TurnOutput{}, err
	}
	notify := response.Notify
	if input.Event.Kind == EventUser || tools.mustNotify || taskOutcomeRequiresNotification(input) {
		notify = true
	}
	return TurnOutput{Text: response.Text, TaskID: tools.taskID(), Silent: !notify}, nil
}

func personalJSONOutputOption(name string, schema map[string]any) model.Option {
	return agenticopenai.WithResponsesText(&responses.ResponseTextConfigParam{
		Format: responses.ResponseFormatTextConfigUnionParam{OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name: name, Strict: param.NewOpt(true), Schema: schema,
		}},
	})
}

func decodePersonalTurnResponse(raw string) (personalTurnResponse, error) {
	fields, err := strictJSONObject(raw, "text", "notify")
	if err != nil {
		return personalTurnResponse{}, fmt.Errorf("invalid personal response: %w", err)
	}
	var text *string
	var notify *bool
	if err := json.Unmarshal(fields["text"], &text); err != nil || text == nil {
		return personalTurnResponse{}, errors.New("invalid personal response text")
	}
	if err := json.Unmarshal(fields["notify"], &notify); err != nil || notify == nil {
		return personalTurnResponse{}, errors.New("invalid personal response notify flag")
	}
	if strings.TrimSpace(*text) == "" {
		return personalTurnResponse{}, errors.New("personal response text is blank")
	}
	return personalTurnResponse{Text: *text, Notify: *notify}, nil
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
	if input.CurrentRun != nil && input.CurrentRun.TaskID == input.Event.TaskID {
		var result StageResult
		if json.Unmarshal([]byte(input.CurrentRun.Result), &result) == nil {
			if len(result.Sources) > 0 {
				return true
			}
		}
	}
	return false
}

func boundedTurnInput(input TurnInput) TurnInput {
	input.Event.Text = limitRunes(input.Event.Text, maxEventRunes)
	input.History = append([]Message(nil), input.History...)
	input.Tasks = append([]Task(nil), input.Tasks...)
	input.Memories = append([]Memory(nil), input.Memories...)
	if input.CurrentRun != nil {
		current := *input.CurrentRun
		input.CurrentRun = &current
		input.CurrentRun.Input = limitRunes(input.CurrentRun.Input, maxTaskRunes)
		input.CurrentRun.Result = limitRunes(input.CurrentRun.Result, maxTaskRunes)
		input.CurrentRun.Error = limitRunes(input.CurrentRun.Error, 800)
	}
	if len(input.History) > maxHistoryMessages {
		input.History = input.History[len(input.History)-maxHistoryMessages:]
	}
	for i := range input.History {
		input.History[i].Text = limitRunes(input.History[i].Text, maxHistoryRunes)
	}
	if len(input.Tasks) > maxTasks {
		input.Tasks = input.Tasks[:maxTasks]
	}
	for i := range input.Tasks {
		input.Tasks[i].Title = limitRunes(input.Tasks[i].Title, 300)
		input.Tasks[i].Goal = limitRunes(input.Tasks[i].Goal, maxTaskRunes)
		input.Tasks[i].Context = limitRunes(input.Tasks[i].Context, maxTaskRunes)
		input.Tasks[i].Summary = limitRunes(input.Tasks[i].Summary, maxTaskRunes)
	}
	if len(input.Memories) > maxMemories {
		input.Memories = input.Memories[:maxMemories]
	}
	for i := range input.Memories {
		input.Memories[i].Key = limitRunes(input.Memories[i].Key, 300)
		input.Memories[i].Value = limitRunes(input.Memories[i].Value, maxMemoryRunes)
		if input.Memories[i].Deleted {
			input.Memories[i].Value = ""
		}
	}
	return input
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
