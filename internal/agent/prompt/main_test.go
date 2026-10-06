package prompt_test

import (
	"strings"
	"testing"

	"kagari/internal/agent/prompt"
)

func TestPromptComposeUsesInjectedPersona(t *testing.T) {
	role := "Use concise English as a neutral editor."
	analysis, digest := prompt.GetPromptTemplate(role), prompt.GetDigestPrompt(role)
	for _, text := range []string{analysis, digest} {
		if strings.Count(text, role) != 1 || strings.Contains(text, "永雏塔菲") || strings.Contains(text, "taffy") {
			t.Fatalf("injected persona was duplicated or replaced: %s", text)
		}
		if !strings.Contains(text, "不可信资料") {
			t.Fatal("replacing persona removed common task rules")
		}
	}
	for _, rule := range []string{
		"没有链接时也可以直接回答",
		"HTTP(S) URL",
		"追读相关链接",
		"read_url 通过直接 HTTP GET 获取页面正文和页面链接",
		"browse_url（可用时）通过 Playwright MCP 获取渲染后的可见内容和链接",
		"同一 URL 可以用两种方式读取",
		"即使 HTTP 请求成功也可改用或补用 browse_url",
		"HTTP 请求失败时也可尝试浏览器",
		"标题和概述",
		"关键要点",
		"讨论者观点",
		"Agent 评价",
		"限制或不确定性时说明",
		"实际读取的来源",
		"没有内容的栏目省略",
		"人格设定、profile 与用户偏好",
		"按问题调整顺序",
	} {
		if !strings.Contains(analysis, rule) {
			t.Errorf("analysis prompt missing rule %q", rule)
		}
	}
	if strings.Contains(analysis, "JSON Schema") || !strings.Contains(analysis, "不输出 JSON、category、claim ID 或 source ID") {
		t.Fatal("analysis prompt retained a final schema contract")
	}
}
