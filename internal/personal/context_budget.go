package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"kagari/internal/domain"
	"kagari/internal/inputbudget"
	"kagari/internal/logging"
	"kagari/internal/textpage"
	"regexp"
)

func modelWithSchema(m model.AgenticModel, outputSchema map[string]any) (*inputbudget.AgenticModel, error) {
	bounded, ok := m.(*inputbudget.AgenticModel)
	if !ok {
		return nil, errors.New("personal: model must be configured with an explicit context_window budget")
	}
	var raw []byte
	var err error
	if outputSchema != nil {
		raw, err = json.Marshal(outputSchema)
		if err != nil {
			return nil, err
		}
	}
	return bounded.WithOutputSchema(raw)
}
func contextBudgetHandler(ctx context.Context, m model.AgenticModel, store Repository, scope ReadScope) (adk.TypedChatModelAgentMiddleware[*schema.AgenticMessage], error) {
	bounded, err := modelWithSchema(m, nil)
	if err != nil {
		return nil, err
	}
	return inputbudget.NewAgenticHandler(ctx, inputbudget.HandlerConfig{
		Limits: bounded.Limits(), SummaryModel: bounded, RequiredPrefix: func(messages []*schema.AgenticMessage) int { return min(2, len(messages)) },
		ValidateSummary: func(ctx context.Context, text string) error {
			for _, id := range summaryMaterialRef.FindAllString(text, -1) {
				if _, err := store.ReadMaterial(ctx, scope, id, "metadata", textpage.Request{Limit: 1}); err != nil {
					return err
				}
			}
			return nil
		},
		KeepUncompressedTools: []string{"memory_get", "memory_search", "memory_history", "memory_put", "memory_forget", "read_attachment", "write_attachment"},
		PersistAndReplace: func(ctx context.Context, round inputbudget.ToolRound) ([]*schema.AgenticMessage, error) {
			return reduceToolRound(ctx, store, scope, round)
		},
		Observe: func(r inputbudget.HandlerRecord) {
			logging.FromContext(ctx).Info("context.reduced", zap.String("phase", r.Phase), zap.Any("estimate", r.Estimate), zap.Any("model_usage", r.ModelRecord), zap.String("error", r.Error))
		},
	})
}

var summaryMaterialRef = regexp.MustCompile(`\bm_[A-Za-z0-9_]+`)

func reduceToolRound(ctx context.Context, store Repository, scope ReadScope, round inputbudget.ToolRound) ([]*schema.AgenticMessage, error) {
	// Existing immutable sources already have an exact read path; archive only other results.
	references := map[string]string{}
	needsArchive := false
	for _, message := range round.Messages {
		for _, block := range message.ContentBlocks {
			if result := block.FunctionToolResult; result != nil {
				ref := persistentToolReference(result)
				if ref == "" {
					needsArchive = true
				} else {
					references[result.CallID] = ref
				}
			}
		}
	}
	var archived string
	if needsArchive {
		raw, err := json.Marshal(round.Messages)
		if err != nil {
			return nil, err
		}
		id, err := store.SaveToolRecord(ctx, scope, round.Key, string(raw), round.ToolNames)
		if err != nil {
			return nil, err
		}
		archived = "Earlier tool output saved as material " + id + "; use read_material to inspect it."
	}
	messages := make([]*schema.AgenticMessage, len(round.Messages))
	for i, message := range round.Messages {
		copyMessage := *message
		copyMessage.ContentBlocks = make([]*schema.ContentBlock, len(message.ContentBlocks))
		for j, block := range message.ContentBlocks {
			copyBlock := *block
			if block.FunctionToolResult != nil {
				result := *block.FunctionToolResult
				ref := references[result.CallID]
				if ref == "" {
					ref = archived
				}
				result.Content = []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: ref}}}
				copyBlock.FunctionToolResult = &result
			}
			copyMessage.ContentBlocks[j] = &copyBlock
		}
		messages[i] = &copyMessage
	}
	return messages, nil
}

func persistentToolReference(result *schema.FunctionToolResult) string {
	if result.Name != "read_url" && result.Name != "browse_url" && result.Name != "read_material" && result.Name != "search_material" {
		return ""
	}
	if len(result.Content) != 1 || result.Content[0].Text == nil {
		return ""
	}
	var value struct {
		Source   *domain.SourceRef `json:"source"`
		Material *Material         `json:"material"`
	}
	if json.Unmarshal([]byte(result.Content[0].Text.Text), &value) != nil {
		return ""
	}
	var id string
	if value.Source != nil {
		id = value.Source.ID
	}
	if value.Material != nil {
		id = value.Material.ID
	}
	if id == "" {
		return ""
	}
	return fmt.Sprintf("Saved material %s; use read_material with part=body or metadata to reopen the original observation.", id)
}
