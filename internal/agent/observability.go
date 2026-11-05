package agent

import (
	"net/url"
	"strings"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

func (e *Engine) logUsage(u *schema.TokenUsage, turn int) {
	if e.Log != nil {
		e.Log.Info("agent token usage", zap.Int("turn", turn), zap.Int("input_tokens", u.PromptTokens), zap.Int("output_tokens", u.CompletionTokens), zap.Int("total_tokens", u.TotalTokens), zap.Int("cached_input_tokens", u.PromptTokenDetails.CachedTokens), zap.Int("reasoning_tokens", u.CompletionTokensDetails.ReasoningTokens))
	}
}

// 阶段日志中的来源原因最多四行和 200 个 Unicode 字符，省略号也计入上限。
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
