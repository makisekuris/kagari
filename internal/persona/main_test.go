package persona_test

import (
	"strings"
	"testing"

	"kagari/internal/agent/prompt"
	"kagari/internal/persona"
	nailv "kagari/internal/persona/nailv"
)

func TestPersonasProvidePrompt(t *testing.T) {
	for name, role := range map[string]persona.PersonaRole{
		"default": persona.Default(),
		"nailv":   nailv.Nailv{},
	} {
		t.Run(name, func(t *testing.T) {
			if role == nil || strings.TrimSpace(role.Prompt()) == "" {
				t.Fatal("persona must provide a prompt")
			}
			rolePrompt := role.Prompt()
			if got := prompt.GetTaskPromptTemplate(rolePrompt); strings.Count(got, rolePrompt) != 1 {
				t.Fatal("task prompt did not include the persona exactly once")
			}
		})
	}
}
