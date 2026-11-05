package persona_test

import (
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
