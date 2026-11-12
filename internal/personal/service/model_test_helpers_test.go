package service

import (
	"testing"
	"time"

	"kagari/internal/personal/assistant"
	"kagari/internal/personal/internal/testkit"
)

func testMainAgent(t *testing.T, fixture *testkit.ResponsesFixture, store assistant.Repository) *assistant.MainAgent {
	t.Helper()
	return &assistant.MainAgent{
		Store: store, Model: fixture.Model(t), Persona: "Persona style token.", Profile: "Profile facts token.",
		MaxIterations: 6, Timeout: 4 * time.Second,
	}
}
