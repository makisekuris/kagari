package agent

import (
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"

	"kagari/internal/domain"
)

func TestDecodeAnalysisOutputStrictly(t *testing.T) {
	kind, body, err := decodeAnalysisOutput(`{"kind":"chat","body":"  exact body\n"}`)
	if err != nil || kind != domain.ResultKindChat || body != "  exact body\n" {
		t.Fatalf("decoded=(%q, %q, %v)", kind, body, err)
	}

	for _, raw := range []string{
		``, `null`, `[]`, `{"body":"missing kind"}`, `{"kind":"analysis"}`,
		`{"kind":"other","body":"text"}`, `{"kind":"analysis","body":" \n "}`,
		`{"kind":"analysis","body":"text","extra":true}`,
		`{"kind":"chat","kind":"analysis","body":"text"}`,
		`{"Kind":"analysis","body":"text"}`, `{"kind":42,"body":"text"}`, `{"kind":"analysis","body":true}`,
		`{"kind":"analysis","body":"text"}{"kind":"chat","body":"more"}`,
		`{"kind":"analysis","body":"text"} trailing`,
		`{"kind":"analysis","body":null}`, `{"kind":null,"body":"text"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := decodeAnalysisOutput(raw); !errors.Is(err, errInvalidAnalysisOutput) {
				t.Fatalf("error=%v, want safe invalid-output error", err)
			}
		})
	}
}

func TestConsumeEventsRejectsParseableStreamingOutputWithNoncompletedStatus(t *testing.T) {
	const raw = `{"kind":"analysis","body":"complete JSON, incomplete response"}`
	msg := &schema.AgenticMessage{
		Role:          schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: raw})},
		ResponseMeta:  &schema.AgenticResponseMeta{OpenAIExtension: &openai.ResponseMetaExtension{Status: openai.ResponseStatusInProgress}},
	}
	iter, gen := adk.NewAsyncIteratorPair[*adk.TypedAgentEvent[*schema.AgenticMessage]]()
	gen.Send(adk.EventFromAgenticMessage(nil, schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), schema.AgenticRoleTypeAssistant))
	gen.Close()
	output, _, _, err := (&Engine{}).consumeEvents(iter, 1)
	if _, _, parseErr := decodeAnalysisOutput(output); parseErr != nil {
		t.Fatalf("fixture should be valid structured output: %v", parseErr)
	}
	if err == nil || err.Error() != "model response incomplete" {
		t.Fatalf("error=%v, want incomplete response", err)
	}
}
