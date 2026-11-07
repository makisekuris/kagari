package personal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"kagari/internal/inputbudget"
)

func TestExecutorInitialContextUsesActualToolsAndRunsOnce(t *testing.T) {
	limits, err := inputbudget.NewLimits(14_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &ContextManifest{FormatVersion: 1, Revision: 3}
	handler := newExecutorInitialContext(
		Run{ID: 7, TaskID: 8, Revision: 3},
		TaskInput{Previous: "omit-me " + strings.Repeat("x", 4_000)},
		TaskProgress{Version: 2, QueryKey: "progress-query", SnapshotMaxID: 13, Entries: []ProgressEntry{{Seq: 13, Version: 2, ProgressUpdate: ProgressUpdate{ID: "check", Description: "checkpoint completed", Status: "reported"}}}},
		[]ReadCheckpoint{{ReadCheckpointUpdate: ReadCheckpointUpdate{ResourceKind: "material", ResourceID: "m_example", Version: "v1", NextOffset: 20, Note: "read first section"}, TaskID: 8, Revision: 3, RunID: 7}},
		limits, manifest,
	)
	state := &adk.TypedChatModelAgentState[*schema.AgenticMessage]{
		Messages:  []*schema.AgenticMessage{schema.SystemAgenticMessage("system"), schema.UserAgenticMessage("required task input")},
		ToolInfos: []*schema.ToolInfo{{Name: "large_schema", Desc: strings.Repeat("schema", 500)}},
	}
	ctx, out, err := handler.BeforeModelRewriteState(context.Background(), state, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil {
		t.Fatal(err)
	}
	if ctx == nil {
		t.Fatal("middleware returned nil context")
	}
	if len(out.Messages) != 5 {
		t.Fatalf("messages=%d, want required pair plus progress cursor, entry, checkpoint; previous must be omitted", len(out.Messages))
	}
	encoded, err := json.Marshal(out.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "omit-me") || !strings.Contains(string(encoded), "progress-query") || !strings.Contains(string(encoded), "checkpoint completed") || !strings.Contains(string(encoded), "read first section") {
		t.Fatalf("optional selection was incorrect: %s", encoded)
	}
	var previousOmitted bool
	for _, selection := range manifest.Selections {
		if selection.ID == "7:previous" && selection.Reason == "input_budget" && selection.IncludedBytes == 0 {
			previousOmitted = true
		}
	}
	if len(manifest.Selections) != 4 || !previousOmitted {
		t.Fatalf("manifest selections=%+v", manifest.Selections)
	}

	_, out, err = handler.BeforeModelRewriteState(context.Background(), out, &adk.TypedModelContext[*schema.AgenticMessage]{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 5 || len(manifest.Selections) != 4 {
		t.Fatalf("second pass duplicated initial context: messages=%d manifest=%+v", len(out.Messages), manifest.Selections)
	}
}
