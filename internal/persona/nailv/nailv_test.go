package persona_test

import (
	"fmt"
	"strings"
	"testing"

	persona "kagari/internal/persona/nailv"
)

func TestNailvPromptContainsPersonaOnly(t *testing.T) {
	prompt := persona.Nailv{}.Prompt()
	if !strings.HasPrefix(prompt, "## 人格设定") || strings.TrimSpace(prompt) == "" {
		t.Fatalf("unexpected persona prompt: %q", prompt)
	}
	for _, detail := range []string{"明前奶绿", "LAPLACE 花店", "百合", "藤椅", "共感"} {
		if !strings.Contains(prompt, detail) {
			t.Errorf("persona prompt is missing %q: %q", detail, prompt)
		}
	}
	for _, rule := range []string{"profile", "输出格式", "输出协议", "必须准确", "用户资料"} {
		if strings.Contains(prompt, rule) {
			t.Errorf("generic guidance %q leaked into persona prompt: %q", rule, prompt)
		}
	}
}

func TestNailvAskChatID(t *testing.T) {
	nailv := persona.Nailv{}
	seen := map[string]bool{}
	for jobID := int64(1); jobID <= 10; jobID++ {
		output := nailv.AskChatID(jobID)
		id := fmt.Sprintf("#%d", jobID)
		if !strings.Contains(output, id) {
			t.Fatalf("output %q missing job ID %d", output, jobID)
		}
		wording := strings.Replace(output, id, "#JOB", 1)
		if seen[wording] {
			t.Fatalf("duplicate acknowledgement wording: %q", wording)
		}
		seen[wording] = true
	}

	for _, jobID := range []int64{0, int64(1<<63 - 1)} {
		got := nailv.AskChatID(jobID)
		if !strings.Contains(got, fmt.Sprintf("#%d", jobID)) {
			t.Errorf("output %q missing full job ID %d", got, jobID)
		}
		if again := nailv.AskChatID(jobID); got != again {
			t.Errorf("AskChatID(%d) is not deterministic: %q != %q", jobID, got, again)
		}
	}
}
