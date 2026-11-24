package modelprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	aclopenai "github.com/cloudwego/eino-ext/libs/acl/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"kagari/internal/config"
)

type jsonSchemaOption struct {
	name   string
	schema map[string]any
	set    bool
}

// WithJSONSchema requests a strict JSON Schema response from either supported API.
func WithJSONSchema(name string, schema map[string]any) model.Option {
	return model.WrapImplSpecificOptFn(func(option *jsonSchemaOption) {
		option.name = name
		option.schema = schema
		option.set = true
	})
}

// New constructs the configured provider while preserving the shared AgenticModel contract.
func New(ctx context.Context, cfg config.Model) (model.AgenticModel, error) {
	switch cfg.EffectiveAPIType() {
	case config.ModelAPIResponses:
		zero, no := 0, false
		m, err := agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{
			BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Model: cfg.Name,
			Timeout: &cfg.Timeout, MaxTokens: &cfg.MaxOutputTokens, MaxRetries: &zero,
			Store: &no, ParallelToolCalls: &no,
		})
		if err != nil {
			return nil, err
		}
		return &responsesModel{ResponsesModel: m}, nil
	case config.ModelAPIChatCompletions:
		maxTokens := cfg.MaxOutputTokens
		m, err := agenticopenai.NewChatModel(ctx, &agenticopenai.ChatConfig{
			BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Model: cfg.Name,
			Timeout: cfg.Timeout, MaxCompletionTokens: &maxTokens,
		})
		if err != nil {
			return nil, err
		}
		return &chatModel{ChatModel: m}, nil
	default:
		return nil, fmt.Errorf("unsupported model API type %q", cfg.EffectiveAPIType())
	}
}

type responsesModel struct {
	*agenticopenai.ResponsesModel
}

func (m *responsesModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	remaining, jsonSchema := splitOptions(opts)
	if jsonSchema != nil {
		remaining = append(remaining, responsesJSONSchemaOption(*jsonSchema))
	}
	return m.ResponsesModel.Generate(ctx, input, remaining...)
}

func (m *responsesModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	remaining, jsonSchema := splitOptions(opts)
	if jsonSchema != nil {
		remaining = append(remaining, responsesJSONSchemaOption(*jsonSchema))
	}
	stream, err := m.ResponsesModel.Stream(ctx, input, remaining...)
	if err != nil {
		return nil, err
	}
	return normalizeToolCallStream(stream), nil
}

type chatModel struct {
	*agenticopenai.ChatModel
}

func (m *chatModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	remaining, jsonSchema := chatOptions(opts)
	if jsonSchema != nil {
		remaining = append(remaining, agenticopenai.WithExtraFields(chatJSONSchemaFields(*jsonSchema)))
	}
	message, err := m.ChatModel.Generate(ctx, input, remaining...)
	if err != nil {
		return nil, err
	}
	if err := validateChatResponse(message); err != nil {
		return nil, err
	}
	return message, nil
}

func (m *chatModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	remaining, jsonSchema := chatOptions(opts)
	if jsonSchema != nil {
		remaining = append(remaining, agenticopenai.WithExtraFields(chatJSONSchemaFields(*jsonSchema)))
	}
	stream, err := m.ChatModel.Stream(ctx, input, remaining...)
	if err != nil {
		return nil, err
	}
	return normalizeChatStream(stream), nil
}

func splitOptions(opts []model.Option) ([]model.Option, *jsonSchemaOption) {
	remaining := make([]model.Option, 0, len(opts))
	var jsonSchema *jsonSchemaOption
	for _, opt := range opts {
		var schemaOpt jsonSchemaOption
		model.GetImplSpecificOptions(&schemaOpt, opt)
		if schemaOpt.set {
			copy := schemaOpt
			jsonSchema = &copy
			continue
		}
		remaining = append(remaining, opt)
	}
	return remaining, jsonSchema
}

func chatOptions(opts []model.Option) ([]model.Option, *jsonSchemaOption) {
	remaining, jsonSchema := splitOptions(opts)
	for i, opt := range remaining {
		if maxTokens := model.GetCommonOptions(nil, opt).MaxTokens; maxTokens != nil {
			remaining[i] = aclopenai.WithMaxCompletionTokens(*maxTokens)
		}
	}
	return remaining, jsonSchema
}

func responsesJSONSchemaOption(option jsonSchemaOption) model.Option {
	return agenticopenai.WithResponsesText(&responses.ResponseTextConfigParam{
		Format: responses.ResponseFormatTextConfigUnionParam{OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name: option.name, Strict: param.NewOpt(true), Schema: option.schema,
		}},
	})
}

func chatJSONSchemaFields(option jsonSchemaOption) map[string]any {
	return map[string]any{
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name": option.name, "strict": true, "schema": option.schema,
			},
		},
	}
}

func validateChatResponse(message *schema.AgenticMessage) error {
	if message == nil {
		return errors.New("chat completion returned an empty response")
	}
	finishReason := ""
	if message.ResponseMeta != nil {
		if ext, ok := message.ResponseMeta.Extension.(*agenticopenai.ChatResponseMetaExtension); ok {
			finishReason = ext.FinishReason
		}
	}
	toolCall := false
	for _, block := range message.ContentBlocks {
		if block != nil && block.FunctionToolCall != nil {
			toolCall = true
			break
		}
	}
	return chatFinishReasonError(finishReason, toolCall)
}

func chatFinishReasonError(finishReason string, toolCall bool) error {
	if finishReason == "length" || finishReason == "content_filter" {
		return fmt.Errorf("chat completion ended with finish_reason %q", finishReason)
	}
	if toolCall && finishReason != "tool_calls" {
		return fmt.Errorf("function tool call ended with finish_reason %q", finishReason)
	}
	if finishReason != "" && finishReason != "stop" && finishReason != "tool_calls" {
		return fmt.Errorf("chat completion ended with unsupported finish_reason %q", finishReason)
	}
	return nil
}

var _ model.AgenticModel = (*responsesModel)(nil)
var _ model.AgenticModel = (*chatModel)(nil)
