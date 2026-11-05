package prompt_test

import (
	"strings"
	"testing"

	"kagari/internal/agent/prompt"
)

func TestPromptComposeUsesInjectedPersona(t *testing.T) {
	role := "Use concise English as a neutral editor."
	instruction := prompt.GetTaskPromptTemplate(role)
	if strings.Count(instruction, role) != 1 || strings.Contains(instruction, "永雏塔菲") || strings.Contains(instruction, "taffy") {
		t.Fatalf("injected persona was duplicated or replaced: %s", instruction)
	}
	if !strings.Contains(instruction, "untrusted") {
		t.Fatal("replacing persona removed common task rules")
	}
	for _, rule := range []string{
		"frozen task goal",
		"reference data",
		"read_url",
		"browse_url",
		"brief, direct plain-text summary",
	} {
		if !strings.Contains(instruction, rule) {
			t.Errorf("task prompt missing rule %q", rule)
		}
	}
}
