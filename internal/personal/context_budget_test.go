package personal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"kagari/internal/domain"
	"kagari/internal/inputbudget"
	"kagari/internal/textpage"
)

func contextToolRound(name, value string) inputbudget.ToolRound {
	call := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "call", Name: name, Arguments: `{}`})}}
	result := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "call", Name: name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: value}}}})}}
	return inputbudget.ToolRound{Key: "round", Complete: true, ToolNames: []string{name}, Messages: []*schema.AgenticMessage{call, result}}
}

func TestContextReductionReusesSourceAndPersistsOtherResults(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "reduce", TaskInput{Goal: "Read evidence"})
	ref, err := s.SaveSource(ctx, run.ID, "source", domain.Source{Content: "original source", Status: "ok", FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"source": ref, "page": map[string]any{"text": "original source"}})
	round := contextToolRound("read_url", string(raw))
	reduced, err := reduceToolRound(ctx, s, ReadScope{RunID: run.ID}, round)
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
	page, err := s.ListMaterials(ctx, ReadScope{RunID: run.ID}, run.ID, ListPage{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("source was duplicated: %+v %v", page, err)
	}
	round = contextToolRound("custom_tool", `{"observation":"unique result"}`)
	reduced, err = reduceToolRound(ctx, s, ReadScope{RunID: run.ID}, round)
	if err != nil {
		t.Fatal(err)
	}
	text = reduced[1].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text
	ids := summaryMaterialRef.FindAllString(text, -1)
	if len(ids) != 1 {
		t.Fatal(text)
	}
	saved, err := s.ReadMaterial(ctx, ReadScope{RunID: run.ID}, ids[0], "body", textpage.Request{})
	if err != nil || !strings.Contains(saved.Page.Text, "unique result") {
		t.Fatalf("archive unreadable: %+v %v", saved, err)
	}
	if _, err = s.SaveToolRecord(ctx, ReadScope{RunID: run.ID}, "memory", "private", []string{"memory_get"}); err == nil {
		t.Fatal("memory body archived")
	}
}

func TestContextReductionPersistenceFailureKeepsOriginal(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "failure", TaskInput{Goal: "Observe"})
	if _, err := s.db.Exec(`CREATE TRIGGER reject_tool_record BEFORE INSERT ON personal_materials BEGIN SELECT RAISE(ABORT, 'disk unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	round := contextToolRound("custom_tool", `{"observation":"retained"}`)
	if _, err := reduceToolRound(ctx, s, ReadScope{RunID: run.ID}, round); err == nil {
		t.Fatal("persistence failure ignored")
	}
	if got := round.Messages[1].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text; !strings.Contains(got, "retained") {
		t.Fatal("original mutated")
	}
}
