package agent

import (
	"net/url"
	"strings"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// 只记录可读内容，不序列化 AgenticMessage 的签名、Extra 或上游错误正文。
func (e *Engine) logMessage(msg *schema.AgenticMessage, streaming bool, turn int) {
	if e.Log == nil || msg == nil {
		return
	}
	base := []zap.Field{zap.Int("turn", turn), zap.String("role", string(msg.Role)), zap.Bool("streaming", streaming)}
	for _, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}
		fields := append([]zap.Field{}, base...)
		if block.StreamingMeta != nil {
			fields = append(fields, zap.Int("block_index", block.StreamingMeta.Index))
		}
		switch {
		case block.Reasoning != nil:
			text := block.Reasoning.Text
			if ext := block.Reasoning.OpenAIExtension; ext != nil {
				for _, content := range ext.Content {
					if content != nil {
						text += content.Text
					}
				}
			}
			if text != "" {
				e.Log.Info("agent reason", append(fields, zap.String("text", e.logContent(text)))...)
			}
		case block.FunctionToolCall != nil:
			if streaming {
				call := block.FunctionToolCall
				e.Log.Info("agent sse output", append(fields, zap.String("event", "toolcall"), zap.String("call_id", call.CallID), zap.String("name", call.Name), zap.String("arguments_delta", e.logContent(call.Arguments)))...)
			} else {
				e.logToolCall(block.FunctionToolCall, turn)
			}
		case block.FunctionToolResult != nil:
			result := block.FunctionToolResult
			var b strings.Builder
			for _, content := range result.Content {
				if content != nil && content.Text != nil {
					b.WriteString(content.Text.Text)
				}
			}
			text := e.logContent(b.String())
			preview := logText(text)
			e.Log.Info("agent toolcall result", append(fields, zap.String("call_id", result.CallID), zap.String("name", result.Name), zap.String("result", preview), zap.Bool("truncated", preview != text))...)
		case block.AssistantGenText != nil && block.AssistantGenText.Text != "":
			if streaming {
				e.Log.Info("agent sse output", append(fields, zap.String("event", "message"), zap.String("delta", e.logContent(block.AssistantGenText.Text)))...)
			} else {
				e.Log.Info("agent message", append(fields, zap.String("text", e.logContent(block.AssistantGenText.Text)))...)
			}
		}
	}
	if msg.ResponseMeta != nil && msg.ResponseMeta.OpenAIExtension != nil {
		ext := msg.ResponseMeta.OpenAIExtension
		fields := append(base, zap.String("response_id", ext.ID), zap.String("status", string(ext.Status)))
		if ext.IncompleteDetails != nil {
			fields = append(fields, zap.String("incomplete_reason", logText(ext.IncompleteDetails.Reason)))
		}
		name := "agent model response"
		if streaming {
			name = "agent sse event"
		}
		e.Log.Info(name, fields...)
	}
}

func (e *Engine) logToolCall(call *schema.FunctionToolCall, turn int) {
	if e.Log != nil {
		e.Log.Info("agent toolcall", zap.Int("turn", turn), zap.String("call_id", call.CallID), zap.String("name", call.Name), zap.String("arguments", e.logContent(call.Arguments)))
	}
}

func (e *Engine) logUsage(u *schema.TokenUsage, turn int) {
	if e.Log != nil {
		e.Log.Info("agent token usage", zap.Int("turn", turn), zap.Int("input_tokens", u.PromptTokens), zap.Int("output_tokens", u.CompletionTokens), zap.Int("total_tokens", u.TotalTokens), zap.Int("cached_input_tokens", u.PromptTokenDetails.CachedTokens), zap.Int("reasoning_tokens", u.CompletionTokensDetails.ReasoningTokens))
	}
}

func (e *Engine) logContent(text string) string {
	for _, secret := range []string{e.Config.Model.APIKey, e.Config.Telegram.Token} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

// 工具结果最多四行和 200 个 Unicode 字符，省略号也计入上限。
func logText(text string) string {
	lines := strings.SplitN(strings.ReplaceAll(text, "\r\n", "\n"), "\n", 5)
	truncated := len(lines) > 4
	if truncated {
		text = strings.Join(lines[:4], "\n")
	}
	runes := []rune(text)
	if len(runes) > 200 || truncated {
		if len(runes) > 199 {
			runes = runes[:199]
		}
		return string(runes) + "…"
	}
	return text
}

func logURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}
