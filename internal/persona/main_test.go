package persona_test

import (
	"strings"
	"testing"

	"kagari/internal/agent/prompt"
	"kagari/internal/persona"
	nailv "kagari/internal/persona/nailv"
)

func TestPersonasProvidePromptAndAcknowledgement(t *testing.T) {
	for name, role := range map[string]persona.PersonaRole{
		"default": persona.Default(),
		"nailv":   nailv.Nailv{},
	} {
		t.Run(name, func(t *testing.T) {
			if role == nil || strings.TrimSpace(role.Prompt()) == "" {
				t.Fatal("persona must provide a prompt")
			}
			if reply := role.AskChatID(42); !strings.Contains(reply, "#42") {
				t.Fatalf("acknowledgement lost the job ID: %q", reply)
			}
			rolePrompt := role.Prompt()
			for _, instruction := range []string{prompt.GetPromptTemplate(rolePrompt), prompt.GetDigestPrompt(rolePrompt)} {
				if strings.Count(instruction, rolePrompt) != 1 {
					t.Fatal("analysis or digest did not include the persona exactly once")
				}
			}
		})
	}
}
