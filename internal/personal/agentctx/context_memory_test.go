package agentctx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRunMemoryVersionIsFrozenButForgetRemovesDirectBlocks(t *testing.T) {
	for _, operation := range []string{"put", "forget"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Minute)
			event := testkit.OpenUserEvent(t, s, 1, 1, "Remember PRIVATE_OLD_VALUE and run the task")
			if _, err := s.WriteMemory(ctx, event.ID, "initial", personal.MemoryChange{Operation: "put", Key: "preference", Value: "PRIVATE_OLD_VALUE", Evidence: "PRIVATE_OLD_VALUE"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.CreateTask(ctx, event.ID, "task", "Read evidence", personal.TaskInput{Goal: "Explicit requirement has priority", MemoryRefs: []string{"preference"}}); err != nil {
				t.Fatal(err)
			}
			testkit.FinishEvent(t, s, event, personal.TurnOutput{}, nil)
			run, err := s.ClaimRun(ctx, personal.RunTask)
			if err != nil || run == nil {
				t.Fatalf("claim task: %v, %v", run, err)
			}
			memory, err := NewMemoryContext(ctx, MemoryContextConfig{Store: s, Run: *run, InitialKeys: []string{"preference"}, CheckRun: func(ctx context.Context) error { return ctx.Err() }})
			if err != nil {
				t.Fatal(err)
			}
			state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{schema.UserAgenticMessage("Explicit requirement has priority")}}
			_, first, err := memory.BeforeModelRewriteState(ctx, state, nil)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(first.Messages)
			if !strings.Contains(string(encoded), "PRIVATE_OLD_VALUE") {
				t.Fatal("selected memory absent before update")
			}
			changeEvent := testkit.OpenUserEvent(t, s, 2, 2, "Change preference to NEW_VALUE or forget it")
			change := personal.MemoryChange{Operation: operation, Key: "preference", ExpectedVersion: 1, Evidence: "NEW_VALUE"}
			if operation == "put" {
				change.Value = "NEW_VALUE"
			}
			if _, err := s.WriteMemory(ctx, changeEvent.ID, "change", change); err != nil {
				t.Fatal(err)
			}
			_, second, err := memory.BeforeModelRewriteState(ctx, first, nil)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ = json.Marshal(second.Messages)
			body := string(encoded)
			if strings.Contains(body, "PRIVATE_OLD_VALUE") != (operation == "put") {
				t.Fatalf("operation %s violated frozen-version/forget behavior: %s", operation, body)
			}
			if strings.Contains(body, "NEW_VALUE") || !strings.Contains(body, "Explicit requirement has priority") {
				t.Fatal("memory update replaced the frozen version or explicit requirement")
			}
		})
	}
}

func TestTaskWithoutMemoryReferencesDoesNotReceiveRecentFacts(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "Remember UNRELATED_FACT")
	if _, err := s.WriteMemory(ctx, event.ID, "memory", personal.MemoryChange{Operation: "put", Key: "unrelated", Value: "UNRELATED_FACT", Evidence: "UNRELATED_FACT"}); err != nil {
		t.Fatal(err)
	}
	testkit.CreateTask(t, s, event, "task", "Specified work", "Do the specified work", "")
	testkit.FinishEvent(t, s, event, personal.TurnOutput{}, nil)
	run, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatalf("claim task: %v, %v", run, err)
	}
	memory, err := NewMemoryContext(ctx, MemoryContextConfig{Store: s, Run: *run, CheckRun: func(ctx context.Context) error { return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{schema.UserAgenticMessage("Do the specified work")}}
	_, updated, err := memory.BeforeModelRewriteState(ctx, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(updated.Messages)
	if strings.Contains(string(encoded), "UNRELATED_FACT") {
		t.Fatal("unselected memory injected")
	}
}

func TestMemoryToolHistoryKeepsVersionUntilForgotten(t *testing.T) {
	for _, operation := range []string{"put", "forget"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Minute)
			event := testkit.OpenUserEvent(t, s, 1, 1, "historical value; current value")
			old, err := s.WriteMemory(ctx, event.ID, "old", personal.MemoryChange{Operation: "put", Key: "style", Value: "historical value", Evidence: "historical value"})
			if err != nil {
				t.Fatal(err)
			}
			change := personal.MemoryChange{Operation: operation, Key: "style", ExpectedVersion: old.Version, Evidence: "current value"}
			if operation == "put" {
				change.Value = "current value"
			}
			if _, err := s.WriteMemory(ctx, event.ID, "update", change); err != nil {
				t.Fatal(err)
			}
			memory, err := NewMemoryContext(ctx, MemoryContextConfig{Store: s})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(memoryToolReply{OK: true, Memory: &old})
			round := contextToolRound("memory_get", string(raw))
			state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: round.Messages}
			_, updated, err := memory.BeforeModelRewriteState(ctx, state, nil)
			if err != nil {
				t.Fatal(err)
			}
			body := updated.Messages[1].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text
			if strings.Contains(body, "historical value") != (operation == "put") {
				t.Fatalf("operation %s rewrote history incorrectly: %s", operation, body)
			}
			if strings.Contains(body, "current value") {
				t.Fatal("historical tool result rewritten to a newer version")
			}
		})
	}
}
