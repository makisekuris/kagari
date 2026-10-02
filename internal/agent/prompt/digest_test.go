package prompt_test

import (
	"strings"
	"testing"

	"kagari/internal/agent/prompt"
	"kagari/internal/persona"
)

func TestDigestPromptReusesPersonaWithoutAnalysisHeadingRules(t *testing.T) {
	rolePrompt := persona.Default().Prompt()
	analysis, digest := prompt.GetPromptTemplate(rolePrompt), prompt.GetDigestPrompt(rolePrompt)
	for _, promptText := range []string{analysis, digest} {
		if !strings.Contains(promptText, rolePrompt) {
			t.Fatalf("persona missing from prompt: %s", promptText)
		}
	}
	for _, phrase := range []string{"headings 是报告的栏目名称", "profile 提供了栏目改名示例", "few-shot 生成"} {
		if !strings.Contains(analysis, phrase) {
			t.Errorf("analysis prompt lost its output rules: %q", phrase)
		}
	}
	if strings.Contains(digest, "headings 是报告的栏目名称") || !strings.Contains(digest, "主题相近不等于重复") || !strings.Contains(digest, "每个输入 job_id 必须且只能") {
		t.Fatal("digest prompt does not separate digest requirements from analysis heading rules")
	}
}
