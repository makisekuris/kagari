package persona_test

import (
	"strings"
	"testing"

	persona "kagari/internal/persona/taffy"
)

func TestTaffyPromptContainsPersonaWithoutProfileRules(t *testing.T) {
	prompt := persona.Taffy{}.Prompt()
	if !strings.HasPrefix(prompt, "## 人格设定") || !strings.Contains(prompt, "永雏塔菲") {
		t.Fatalf("persona prompt missing role: %q", prompt)
	}
	if strings.Contains(prompt, "profile") || strings.Contains(prompt, "用户的提问") {
		t.Fatalf("generic profile guidance leaked into persona prompt: %q", prompt)
	}
}
