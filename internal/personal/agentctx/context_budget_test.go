package agentctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"kagari/internal/domain"
	"kagari/internal/inputbudget"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/textpage"
)

func contextToolRound(name string, values ...string) inputbudget.ToolRound {
	calls := make([]*schema.ContentBlock, 0, len(values))
	results := make([]*schema.ContentBlock, 0, len(values))
	for i, value := range values {
		callID := fmt.Sprintf("call-%d", i+1)
		calls = append(calls, schema.NewContentBlock(&schema.FunctionToolCall{CallID: callID, Name: name, Arguments: `{}`}))
		results = append(results, schema.NewContentBlock(&schema.FunctionToolResult{CallID: callID, Name: name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: value}}}}))
	}
	return inputbudget.ToolRound{Key: "round", Complete: true, ToolNames: []string{name}, Messages: []*schema.AgenticMessage{
		{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: calls},
		{Role: schema.AgenticRoleTypeUser, ContentBlocks: results},
	}}
}

func TestContextReductionReusesSourceAndPersistsOtherResults(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	run := testkit.CreateRunningTask(t, s, "reduce", personal.TaskInput{Goal: "Read evidence"})
	ref, err := s.SaveSource(ctx, run.ID, "source", domain.Source{Content: "original source", Status: "ok", FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"source": ref, "page": map[string]any{"text": "original source"}})
	round := contextToolRound("read_url", string(raw))
	reduced, err := reduceToolRound(ctx, s, personal.ReadScope{RunID: run.ID}, round)
	if err != nil {
		t.Fatal(err)
	}
	if reduced[0].ContentBlocks[0].FunctionToolCall.CallID != reduced[1].ContentBlocks[0].FunctionToolResult.CallID {
		t.Fatal("tool pair changed")
	}
	text := reduced[1].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text
	if !strings.Contains(text, ref.ID) || strings.Contains(text, "original source") {
		t.Fatal(text)
	}
	page, err := s.ListMaterials(ctx, personal.ReadScope{RunID: run.ID}, run.ID, personal.ListPage{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("source was duplicated: %+v %v", page, err)
	}
	round = contextToolRound("custom_tool", `{"observation":"unique result"}`, `{"observation":"second result"}`)
	reduced, err = reduceToolRound(ctx, s, personal.ReadScope{RunID: run.ID}, round)
	if err != nil {
		t.Fatal(err)
	}
	text = reduced[1].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text
	ids := personal.MaterialReferences(text)
	if len(ids) != 1 {
		t.Fatal(text)
	}
	saved, err := s.ReadMaterial(ctx, personal.ReadScope{RunID: run.ID}, ids[0], "body", textpage.Request{})
	if err != nil {
		t.Fatal(err)
	}
	expectedBody, err := json.Marshal(round.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Page.Text != string(expectedBody) {
		t.Fatalf("archive changed the original tool round: %+v", saved)
	}
	metadata, err := s.ReadMaterial(ctx, personal.ReadScope{RunID: run.ID}, ids[0], "metadata", textpage.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var gotMetadata struct {
		FormatVersion int      `json:"format_version"`
		RecordType    string   `json:"record_type"`
		Tools         []string `json:"tools"`
		ToolCallIDs   []string `json:"tool_call_ids"`
	}
	if err := json.Unmarshal([]byte(metadata.Page.Text), &gotMetadata); err != nil {
		t.Fatal(err)
	}
	if gotMetadata.FormatVersion != 1 || gotMetadata.RecordType != "tool_round" || !reflect.DeepEqual(gotMetadata.Tools, []string{"custom_tool"}) || !reflect.DeepEqual(gotMetadata.ToolCallIDs, []string{"call-1", "call-2"}) {
		t.Fatalf("archived metadata = %+v", gotMetadata)
	}
	privateRound := contextToolRound("memory_get", `{"memory":{"key":"secret","value":"private"}}`)
	privateBody, err := json.Marshal(privateRound.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveToolRecord(ctx, personal.ReadScope{RunID: run.ID}, "memory", personal.ToolRecord{Body: string(privateBody), Tools: privateRound.ToolNames}); err == nil {
		t.Fatal("memory body archived")
	}
}

func TestContextReductionPersistenceFailureKeepsOriginal(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	run := testkit.CreateRunningTask(t, s, "failure", personal.TaskInput{Goal: "Observe"})
	failure := errors.New("disk unavailable")
	store := failingArchive{BudgetRepository: s, failure: failure}
	round := contextToolRound("custom_tool", `{"observation":"retained"}`)
	if _, err := reduceToolRound(ctx, store, personal.ReadScope{RunID: run.ID}, round); !errors.Is(err, failure) {
		t.Fatal("persistence failure ignored")
	}
	if got := round.Messages[1].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text; !strings.Contains(got, "retained") {
		t.Fatal("original mutated")
	}
}

type failingArchive struct {
	BudgetRepository
	failure error
}

func (s failingArchive) SaveToolRecord(context.Context, personal.ReadScope, string, personal.ToolRecord) (string, error) {
	return "", s.failure
}
