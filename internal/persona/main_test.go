package persona_test

import (
	"strings"
	"testing"

	"kagari/internal/persona"
)

func TestDefaultProvidesPromptAndAcknowledgement(t *testing.T) {
	var role persona.PersonaRole = persona.Default()
	if role == nil || strings.TrimSpace(role.Prompt()) == "" {
		t.Fatal("default persona must provide a prompt")
	}
	if reply := role.AskChatID(42); !strings.Contains(reply, "#42") {
		t.Fatalf("acknowledgement lost the job ID: %q", reply)
	}
}
