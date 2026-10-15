package persona_test

import (
	"fmt"
	"strings"
	"testing"

	persona "kagari/internal/persona/taffy"
)

func TestTaffyAskChatID(t *testing.T) {
	taffy := persona.Taffy{}
	seen := map[string]bool{}
	for jobID := int64(1); jobID <= 10; jobID++ {
		output := taffy.AskChatID(jobID)
		if !strings.Contains(output, fmt.Sprintf("#%d", jobID)) {
			t.Fatalf("output %q missing job ID %d", output, jobID)
		}
		wording := strings.Replace(output, fmt.Sprintf("#%d", jobID), "#JOB", 1)
		if seen[wording] {
			t.Fatalf("duplicate acknowledgement wording: %q", wording)
		}
		seen[wording] = true
	}
}

func TestTaffyPromptContainsPersonaWithoutProfileRules(t *testing.T) {
	prompt := persona.Taffy{}.Prompt()
	if !strings.HasPrefix(prompt, "## 人格设定") || !strings.Contains(prompt, "永雏塔菲") {
		t.Fatalf("persona prompt missing role: %q", prompt)
	}
	if strings.Contains(prompt, "profile") || strings.Contains(prompt, "用户的提问") {
		t.Fatalf("generic profile guidance leaked into persona prompt: %q", prompt)
	}
}
