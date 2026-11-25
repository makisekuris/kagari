package agentctx

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"kagari/internal/documentarchive"
)

func TestArchiveSummarizerSendsExtractedText(t *testing.T) {
	const original = "工程报价包括防水和五年质保。"
	const expected = "报价包含防水，质保五年。"
	model := &imageModelStub{generate: func(_ context.Context, input []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
		if len(input) != 2 || input[0].Role != schema.AgenticRoleTypeSystem || input[1].Role != schema.AgenticRoleTypeUser {
			t.Fatalf("archive model received unexpected messages: %#v", input)
		}
		blocks := input[1].ContentBlocks
		if len(blocks) != 1 || blocks[0].UserInputText == nil || blocks[0].UserInputText.Text != original {
			t.Fatalf("archive model did not receive the extracted text")
		}
		return imageTextResponse(expected), nil
	}}
	got, err := ArchiveSummarizer(model)(t.Context(), documentarchive.Sample{Text: original})
	if err != nil || got != expected {
		t.Fatalf("summary %q: %v", got, err)
	}
}
