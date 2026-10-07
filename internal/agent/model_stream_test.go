package agent

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"
)

func TestNormalizeToolCallStream(t *testing.T) {
	t.Run("delta replaces repeated early arguments", func(t *testing.T) {
		const args = `{"url":"https://example.org"}`
		added := functionCallChunk(2, "read_url", "call_1", args)
		got := collectNormalized(t, []*schema.AgenticMessage{
			added,
			functionCallChunk(2, "", "", args),
			responseStatusChunk("completed"),
		})

		if gotArgs := argumentsAt(got, 2); gotArgs != args {
			t.Fatalf("arguments=%q, want one copy %q", gotArgs, args)
		}
		if added.ContentBlocks[0].FunctionToolCall.Arguments != args {
			t.Fatal("normalizer mutated the source chunk")
		}
	})

	t.Run("empty added block keeps incremental deltas", func(t *testing.T) {
		const first, second = `{"url":`, `"https://example.org"}`
		got := collectNormalized(t, []*schema.AgenticMessage{
			functionCallChunk(4, "read_url", "call_2", ""),
			functionCallChunk(4, "", "", first),
			functionCallChunk(4, "", "", second),
			responseStatusChunk("completed"),
		})
		if gotArgs := argumentsAt(got, 4); gotArgs != first+second {
			t.Fatalf("arguments=%q, want %q", gotArgs, first+second)
		}
	})

	t.Run("restores early arguments when no delta arrives", func(t *testing.T) {
		const partialJSON = `{"url":`
		added := functionCallChunk(7, "read_url", "call_3", partialJSON)
		got := collectNormalized(t, []*schema.AgenticMessage{added, responseStatusChunk("completed")})
		if gotArgs := argumentsAt(got, 7); gotArgs != partialJSON {
			t.Fatalf("arguments=%q, want raw partial JSON %q", gotArgs, partialJSON)
		}
		if added.ContentBlocks[0].FunctionToolCall.Arguments != partialJSON {
			t.Fatal("normalizer mutated the source chunk")
		}
	})

	t.Run("interleaved calls retain their own arguments", func(t *testing.T) {
		const argsA, argsB = `{"url":"a"}`, `{"url":"b"}`
		got := collectNormalized(t, []*schema.AgenticMessage{
			functionCallChunk(10, "read_url", "call_a", argsA),
			functionCallChunk(11, "read_url", "call_b", argsB),
			functionCallChunk(10, "", "", argsA),
			responseStatusChunk("completed"),
		})
		if gotA, gotB := argumentsAt(got, 10), argumentsAt(got, 11); gotA != argsA || gotB != argsB {
			t.Fatalf("arguments at indices 10 and 11 = %q, %q; want %q, %q", gotA, gotB, argsA, argsB)
		}
	})
}

func TestNormalizeToolCallStreamRequiresCompletedResponse(t *testing.T) {
	for _, status := range []string{"", "failed", "incomplete", "cancelled"} {
		t.Run(statusName(status), func(t *testing.T) {
			source := []*schema.AgenticMessage{functionCallChunk(1, "read_url", "call", `{"url":"x"}`)}
			if status != "" {
				source = append(source, responseStatusChunk(status))
			}
			chunks, err := drainNormalized(normalizeToolCallStream(schema.StreamReaderFromArray(source)))
			if err == nil || !strings.Contains(err.Error(), "before response completed") {
				t.Fatalf("stream error=%v, want incomplete response error", err)
			}
			if got := argumentsAt(chunks, 1); got != "" {
				t.Fatalf("arguments=%q; incomplete stream must not restore early arguments", got)
			}
		})
	}
}

func TestNormalizeToolCallStreamForwardsSourceErrorWithoutFallback(t *testing.T) {
	source, writer := schema.Pipe[*schema.AgenticMessage](2)
	sourceErr := errors.New("upstream read failed")
	writer.Send(functionCallChunk(3, "read_url", "call", `{"url":"x"}`), nil)
	writer.Send(nil, sourceErr)
	writer.Close()

	chunks, err := drainNormalized(normalizeToolCallStream(source))
	if !errors.Is(err, sourceErr) {
		t.Fatalf("stream error=%v, want source error %v", err, sourceErr)
	}
	if got := argumentsAt(chunks, 3); got != "" {
		t.Fatalf("arguments=%q; source error must prevent fallback", got)
	}
}

func TestNormalizeToolCallStreamPreservesNonToolResponseMetadata(t *testing.T) {
	terminal := responseStatusChunk("completed")
	terminal.ResponseMeta.TokenUsage = &schema.TokenUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}
	got := collectNormalized(t, []*schema.AgenticMessage{terminal})
	if len(got) != 1 || got[0].ResponseMeta != terminal.ResponseMeta {
		t.Fatal("response metadata was not passed through unchanged")
	}
	if got[0].ResponseMeta.TokenUsage.TotalTokens != 5 || got[0].ResponseMeta.OpenAIExtension.Status != openai.ResponseStatusCompleted {
		t.Fatalf("response metadata=%+v", got[0].ResponseMeta)
	}
}

func functionCallChunk(index int, name, callID, args string) *schema.AgenticMessage {
	block := schema.NewContentBlockChunk(&schema.FunctionToolCall{Name: name, CallID: callID, Arguments: args}, &schema.StreamingMeta{Index: index})
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{block}}
}

func responseStatusChunk(status string) *schema.AgenticMessage {
	return &schema.AgenticMessage{ResponseMeta: &schema.AgenticResponseMeta{OpenAIExtension: &openai.ResponseMetaExtension{Status: openai.ResponseStatus(status)}}}
}

func collectNormalized(t *testing.T, source []*schema.AgenticMessage) []*schema.AgenticMessage {
	t.Helper()
	chunks, err := drainNormalized(normalizeToolCallStream(schema.StreamReaderFromArray(source)))
	if err != nil {
		t.Fatal(err)
	}
	return chunks
}

func drainNormalized(reader *schema.StreamReader[*schema.AgenticMessage]) ([]*schema.AgenticMessage, error) {
	defer reader.Close()
	var chunks []*schema.AgenticMessage
	for {
		chunk, err := reader.Recv()
		if errors.Is(err, io.EOF) {
			return chunks, nil
		}
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, chunk)
	}
}

func argumentsAt(chunks []*schema.AgenticMessage, index int) string {
	var args strings.Builder
	for _, chunk := range chunks {
		if chunk == nil {
			continue
		}
		for _, block := range chunk.ContentBlocks {
			if block != nil && block.FunctionToolCall != nil && block.StreamingMeta != nil && block.StreamingMeta.Index == index {
				args.WriteString(block.FunctionToolCall.Arguments)
			}
		}
	}
	return args.String()
}

func statusName(status string) string {
	if status == "" {
		return "missing"
	}
	return status
}
