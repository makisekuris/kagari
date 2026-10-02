package agent

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"kagari/internal/domain"
)

// consumeEvents is shared by single-shot digest generation and tool-using analysis.
func (e *Engine) consumeEvents(it *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]], iterations int) (output string, usage domain.Usage, usageReported bool, err error) {
	turn := 0
	for {
		event, ok := it.Next()
		if !ok {
			return output, usage, usageReported, nil
		}
		if event.Err != nil {
			return output, usage, usageReported, fmt.Errorf("agent run: %w", event.Err)
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		variant := event.Output.MessageOutput
		if variant.AgenticRole == schema.AgenticRoleTypeAssistant {
			turn++
			if e.Log != nil {
				e.Log.Info("agent model turn", zap.Int("turn", turn), zap.Int("iterations_limit", iterations), zap.Bool("streaming", variant.IsStreaming))
			}
		}
		if variant.IsStreaming {
			variant.MessageStream = schema.StreamReaderWithConvert(variant.MessageStream, func(part *schema.AgenticMessage) (*schema.AgenticMessage, error) {
				e.logMessage(part, true, turn)
				return part, nil
			})
		}
		msg, msgErr := variant.GetMessage()
		if msgErr != nil {
			return output, usage, usageReported, fmt.Errorf("agent message: %w", msgErr)
		}
		if !variant.IsStreaming {
			e.logMessage(msg, false, turn)
		}
		if msg == nil || msg.Role != schema.AgenticRoleTypeAssistant {
			continue
		}
		if msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil {
			usageReported = true
			u := msg.ResponseMeta.TokenUsage
			usage.InputTokens += u.PromptTokens
			usage.OutputTokens += u.CompletionTokens
			usage.TotalTokens += u.TotalTokens
			e.logUsage(u, turn)
		}
		if msg.ResponseMeta != nil && msg.ResponseMeta.OpenAIExtension != nil {
			switch msg.ResponseMeta.OpenAIExtension.Status {
			case "failed":
				return output, usage, usageReported, errors.New("model response failed")
			case "incomplete":
				return output, usage, usageReported, errors.New("model response incomplete")
			}
		}
		var b strings.Builder
		toolCall := false
		for _, block := range msg.ContentBlocks {
			if block == nil {
				continue
			}
			if block.FunctionToolCall != nil {
				toolCall = true
				if variant.IsStreaming {
					e.logToolCall(block.FunctionToolCall, turn)
				}
			}
			if block.AssistantGenText != nil {
				b.WriteString(block.AssistantGenText.Text)
			}
		}
		if !toolCall && b.Len() > 0 {
			output = b.String()
		}
	}
}
