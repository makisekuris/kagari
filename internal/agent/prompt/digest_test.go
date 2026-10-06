package prompt_test

import (
	"strings"
	"testing"

	"kagari/internal/agent/prompt"
	"kagari/internal/persona"
)

func TestDigestPromptUsesMarkdownStructureFromArchivedText(t *testing.T) {
	rolePrompt := persona.Default().Prompt()
	digest := prompt.GetDigestPrompt(rolePrompt)
	if !strings.Contains(digest, rolePrompt) {
		t.Fatalf("persona missing from prompt: %s", digest)
	}
	for _, rule := range []string{
		"所有输入 body",
		"合并重复内容，保留不同观点",
		"周期总览",
		"按实际主题分组",
		"关键要点",
		"观点差异或评价",
		"限制与来源",
		"简短结尾",
		"没有内容的栏目省略",
		"阅读偏好调整",
		"不联网读取",
		"归档正文和 sources",
		"真实 URL",
		"中文 Markdown",
	} {
		if !strings.Contains(digest, rule) {
			t.Errorf("digest prompt missing rule %q", rule)
		}
	}
	if strings.Contains(digest, "JSON Schema") || strings.Contains(digest, "claim ID") || strings.Contains(digest, "source ID") {
		t.Fatal("digest prompt retained a schema or source ID contract")
	}
}
