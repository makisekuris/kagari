package personal

import (
	"context"
	"encoding/json"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"kagari/internal/domain"
	"strings"
	"testing"
)

func TestRunMemoryVersionIsFrozenButForgetRemovesDirectBlocks(t *testing.T) {
	for _, forget := range []bool{false, true} {
		t.Run(map[bool]string{false: "corrected", true: "forgotten"}[forget], func(t *testing.T) {
			repo := newToolRepo()
			repo.memories["preference"] = Memory{Key: "preference", Version: 1, Value: "PRIVATE_OLD_VALUE"}
			fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseTool("read_url", map[string]string{"url": "https://example.org"}, "read"), responseText("Done")}}
			engine := readingEngine(fixture, func(context.Context, string) (domain.Source, error) {
				repo.memories["preference"] = Memory{Key: "preference", Version: 2, Value: "NEW_VALUE", Deleted: forget}
				return domain.Source{URL: "https://example.org", Status: "ok", Content: "Observed evidence"}, nil
			})
			_, err := runTaskExecutor(t, context.Background(), engine, repo, executorRun(TaskInput{Goal: "Explicit requirement has priority", MemoryRefs: []string{"preference"}}), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(fixture.requests) != 2 {
				t.Fatalf("requests=%d", len(fixture.requests))
			}
			if !strings.Contains(requestInput(fixture.requests[0]), "PRIVATE_OLD_VALUE") {
				t.Fatal("selected version absent initially")
			}
			second := requestInput(fixture.requests[1])
			if strings.Contains(second, "PRIVATE_OLD_VALUE") == forget {
				t.Fatalf("forget=%t old value retained=%t", forget, strings.Contains(second, "PRIVATE_OLD_VALUE"))
			}
			if strings.Contains(second, "NEW_VALUE") {
				t.Fatal("normal correction replaced a frozen run version")
			}
		})
	}
}

func TestTaskWithoutMemoryReferencesDoesNotReceiveRecentFacts(t *testing.T) {
	repo := newToolRepo()
	repo.memories["unrelated"] = Memory{Key: "unrelated", Version: 1, Value: "UNRELATED_FACT"}
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseText("Done")}}
	if _, err := runTaskExecutor(t, context.Background(), readingEngine(fixture, nil), repo, executorRun(TaskInput{Goal: "Do the specified work"}), nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(requestInput(fixture.requests[0]), "UNRELATED_FACT") {
		t.Fatal("unselected memory injected")
	}
}

func TestMemoryToolHistoryKeepsVersionUntilForgotten(t *testing.T) {
	for _, forgotten := range []bool{false, true} {
		repo := newToolRepo()
		repo.memories["style"] = Memory{Key: "style", Version: 2, Value: "current value", Deleted: forgotten}
		memory, err := newRunMemoryContext(context.Background(), repo, Run{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(personalToolReply{OK: true, Memory: &Memory{Key: "style", Version: 1, Value: "historical value"}})
		round := contextToolRound("memory_get", string(raw))
		state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{Messages: round.Messages}
		_, updated, err := memory.BeforeModelRewriteState(context.Background(), state, nil)
		if err != nil {
			t.Fatal(err)
		}
		body := updated.Messages[1].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text
		if strings.Contains(body, "historical value") == forgotten {
			t.Fatalf("forgotten=%v tool history=%s", forgotten, body)
		}
		if strings.Contains(body, "current value") {
			t.Fatal("historical tool result was rewritten to a newer version")
		}
	}
}
