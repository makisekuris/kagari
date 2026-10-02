package prompt_test

import (
	"strings"
	"testing"

	"kagari/internal/agent/prompt"
)

func TestPromptComposeUsesInjectedPersona(t *testing.T) {
	role := "Use concise English as a neutral editor."
	for _, text := range []string{prompt.GetPromptTemplate(role), prompt.GetDigestPrompt(role)} {
		if strings.Count(text, role) != 1 || strings.Contains(text, "永雏塔菲") || strings.Contains(text, "taffy") {
			t.Fatalf("injected persona was duplicated or replaced: %s", text)
		}
		if !strings.Contains(text, "不可信资料") || !strings.Contains(text, "不当作事实证据") || !strings.Contains(text, "JSON Schema") {
			t.Fatal("replacing persona removed common task rules")
		}
	}
}
