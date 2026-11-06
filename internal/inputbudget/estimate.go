package inputbudget

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type Estimate struct {
	TotalBytes        int64  `json:"estimated_input_tokens"`
	MessagesBytes     int64  `json:"messages_bytes"`
	RequiredBytes     int64  `json:"required_input_bytes"`
	HistoryBytes      int64  `json:"history_bytes"`
	ToolsBytes        int64  `json:"tools_schema_bytes"`
	OutputSchemaBytes int64  `json:"output_schema_bytes"`
	CountBasis        string `json:"count_basis"`
}

type UnsupportedContentError struct {
	MessageIndex int
	ContentType  schema.ContentBlockType
}

func (e *UnsupportedContentError) Error() string {
	return fmt.Sprintf("inputbudget: unsupported content block %q in message %d", e.ContentType, e.MessageIndex)
}

var ErrInvalidJSON = errors.New("inputbudget: invalid schema JSON")

// EstimateAgenticInput counts canonical UTF-8 JSON for messages, tool schemas, and an explicitly supplied output schema.
func EstimateAgenticInput(messages []*schema.AgenticMessage, tools, deferred []*schema.ToolInfo, toolSearch *schema.ToolInfo, outputSchema json.RawMessage, requiredPrefix int) (Estimate, error) {
	if requiredPrefix < 0 || requiredPrefix > len(messages) {
		return Estimate{}, fmt.Errorf("inputbudget: required prefix %d outside message count %d", requiredPrefix, len(messages))
	}
	for i, msg := range messages {
		if msg == nil {
			return Estimate{}, fmt.Errorf("inputbudget: nil message at index %d", i)
		}
		for _, block := range msg.ContentBlocks {
			if !supportedBlock(block) {
				var typ schema.ContentBlockType
				if block != nil {
					typ = block.Type
				}
				return Estimate{}, &UnsupportedContentError{MessageIndex: i, ContentType: typ}
			}
		}
	}
	messageJSON, err := json.Marshal(messages)
	if err != nil {
		return Estimate{}, fmt.Errorf("inputbudget: encode messages: %w", err)
	}
	var requiredBytes int64
	if requiredPrefix > 0 {
		prefixJSON, err := json.Marshal(messages[:requiredPrefix])
		if err != nil {
			return Estimate{}, fmt.Errorf("inputbudget: encode required input: %w", err)
		}
		requiredBytes = int64(len(prefixJSON))
	}
	var toolJSON []byte
	if len(tools)+len(deferred) > 0 || toolSearch != nil {
		toolJSON, err = json.Marshal(struct {
			Tools      []*schema.ToolInfo `json:"tools,omitempty"`
			Deferred   []*schema.ToolInfo `json:"deferred_tools,omitempty"`
			ToolSearch *schema.ToolInfo   `json:"tool_search_tool,omitempty"`
		}{Tools: tools, Deferred: deferred, ToolSearch: toolSearch})
		if err != nil {
			return Estimate{}, fmt.Errorf("inputbudget: encode tool schemas: %w", err)
		}
	}
	canonicalSchema, err := canonicalJSON(outputSchema)
	if err != nil {
		return Estimate{}, err
	}
	messageBytes, toolBytes, schemaBytes := int64(len(messageJSON)), int64(len(toolJSON)), int64(len(canonicalSchema))
	if messageBytes > int64(^uint64(0)>>1)-toolBytes || messageBytes+toolBytes > int64(^uint64(0)>>1)-schemaBytes {
		return Estimate{}, errors.New("inputbudget: estimated input size overflows int64")
	}
	return Estimate{
		TotalBytes:        messageBytes + toolBytes + schemaBytes,
		MessagesBytes:     messageBytes,
		RequiredBytes:     requiredBytes,
		HistoryBytes:      messageBytes - requiredBytes,
		ToolsBytes:        toolBytes,
		OutputSchemaBytes: schemaBytes,
		CountBasis:        CountBasis,
	}, nil
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, fmt.Errorf("%w: trailing value", ErrInvalidJSON)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	return encoded, nil
}

func supportedBlock(block *schema.ContentBlock) bool {
	if block == nil || contentVariantCount(block) != 1 {
		return false
	}
	switch block.Type {
	case schema.ContentBlockTypeReasoning:
		return block.Reasoning != nil
	case schema.ContentBlockTypeUserInputText:
		return block.UserInputText != nil
	case schema.ContentBlockTypeAssistantGenText:
		return block.AssistantGenText != nil
	case schema.ContentBlockTypeFunctionToolCall:
		return block.FunctionToolCall != nil
	case schema.ContentBlockTypeFunctionToolResult:
		if block.FunctionToolResult == nil {
			return false
		}
		for _, part := range block.FunctionToolResult.Content {
			if part == nil || part.Type != schema.FunctionToolResultContentBlockTypeText || part.Text == nil {
				return false
			}
		}
		return true
	case schema.ContentBlockTypeToolSearchResult:
		return block.ToolSearchFunctionToolResult != nil
	default:
		return false
	}
}

func contentVariantCount(block *schema.ContentBlock) int {
	count := 0
	for _, present := range []bool{
		block.Reasoning != nil, block.UserInputText != nil, block.UserInputImage != nil, block.UserInputAudio != nil,
		block.UserInputVideo != nil, block.UserInputFile != nil, block.AssistantGenText != nil, block.AssistantGenImage != nil,
		block.AssistantGenAudio != nil, block.AssistantGenVideo != nil, block.FunctionToolCall != nil, block.FunctionToolResult != nil,
		block.ToolSearchFunctionToolResult != nil, block.ServerToolCall != nil, block.ServerToolResult != nil, block.MCPToolCall != nil,
		block.MCPToolResult != nil, block.MCPListToolsResult != nil, block.MCPToolApprovalRequest != nil, block.MCPToolApprovalResponse != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

func estimateWithOptions(messages []*schema.AgenticMessage, options *model.Options, outputSchema json.RawMessage, requiredPrefix int) (Estimate, error) {
	if options == nil {
		options = &model.Options{}
	}
	return EstimateAgenticInput(messages, options.Tools, options.DeferredTools, options.ToolSearchTool, outputSchema, requiredPrefix)
}
