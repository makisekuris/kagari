package modelprovider

import (
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"
)

func normalizeToolCallStream(source *schema.StreamReader[*schema.AgenticMessage]) *schema.StreamReader[*schema.AgenticMessage] {
	reader, writer := schema.Pipe[*schema.AgenticMessage](1)
	go func() {
		defer source.Close()
		defer writer.Close()

		earlyArgs := map[int]string{}
		gotDelta := map[int]bool{}
		sawFunctionCall := false
		var status openai.ResponseStatus

		for {
			msg, err := source.Recv()
			if errors.Is(err, io.EOF) {
				// Do not execute or restore arguments unless the response completed.
				if sawFunctionCall && status != openai.ResponseStatusCompleted {
					writer.Send(nil, fmt.Errorf("function tool call stream ended before response completed (status %q)", status))
					return
				}
				if status == "" {
					writer.Send(nil, errors.New("model response incomplete"))
					return
				}
				for index, args := range earlyArgs {
					if gotDelta[index] {
						continue
					}
					chunk := schema.NewContentBlockChunk(&schema.FunctionToolCall{Arguments: args}, &schema.StreamingMeta{Index: index})
					if writer.Send(&schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{chunk}}, nil) {
						return
					}
				}
				return
			}
			if err != nil {
				writer.Send(nil, err)
				return
			}
			if msg == nil {
				continue
			}

			if msg.ResponseMeta != nil && msg.ResponseMeta.OpenAIExtension != nil && msg.ResponseMeta.OpenAIExtension.Status != "" {
				status = msg.ResponseMeta.OpenAIExtension.Status
			}

			copy := *msg
			copy.ContentBlocks = append([]*schema.ContentBlock(nil), msg.ContentBlocks...)
			for i, block := range copy.ContentBlocks {
				if block == nil || block.FunctionToolCall == nil {
					continue
				}
				sawFunctionCall = true
				if block.StreamingMeta == nil || block.FunctionToolCall.Arguments == "" {
					continue
				}
				index := block.StreamingMeta.Index
				if block.FunctionToolCall.Name == "" {
					// agenticopenai v0.2.4 omits Name on argument-delta chunks.
					// A delta supersedes the full arguments sent with output_item.added.
					gotDelta[index] = true
					continue
				}

				earlyArgs[index] = block.FunctionToolCall.Arguments
				blockCopy := *block
				callCopy := *block.FunctionToolCall
				callCopy.Arguments = ""
				blockCopy.FunctionToolCall = &callCopy
				copy.ContentBlocks[i] = &blockCopy
			}
			if writer.Send(&copy, nil) {
				return
			}
		}
	}()
	return reader
}

// normalizeChatStream keeps streamed tool calls pending until the Chat
// Completions API confirms a complete tool-call response. Text chunks remain
// available as they arrive, including when the response later fails.
func normalizeChatStream(source *schema.StreamReader[*schema.AgenticMessage]) *schema.StreamReader[*schema.AgenticMessage] {
	reader, writer := schema.Pipe[*schema.AgenticMessage](1)
	go func() {
		defer source.Close()
		defer writer.Close()

		var pendingTools []*schema.AgenticMessage
		sawToolCall := false
		finishReason := ""

		for {
			msg, err := source.Recv()
			if errors.Is(err, io.EOF) {
				if finishReason == "" {
					writer.Send(nil, errors.New("model response incomplete"))
					return
				}
				if err := chatFinishReasonError(finishReason, sawToolCall); err != nil {
					writer.Send(nil, err)
					return
				}
				for _, toolChunk := range pendingTools {
					if writer.Send(toolChunk, nil) {
						return
					}
				}
				return
			}
			if err != nil {
				writer.Send(nil, err)
				return
			}
			if msg == nil {
				continue
			}

			if msg.ResponseMeta != nil {
				if ext, ok := msg.ResponseMeta.Extension.(*agenticopenai.ChatResponseMetaExtension); ok && ext.FinishReason != "" {
					finishReason = ext.FinishReason
				}
			}

			var textBlocks, toolBlocks []*schema.ContentBlock
			for _, block := range msg.ContentBlocks {
				if block != nil && block.FunctionToolCall != nil {
					toolBlocks = append(toolBlocks, block)
				} else {
					textBlocks = append(textBlocks, block)
				}
			}
			if len(toolBlocks) == 0 {
				if writer.Send(msg, nil) {
					return
				}
				continue
			}

			sawToolCall = true
			toolChunk := *msg
			toolChunk.ContentBlocks = toolBlocks
			pendingTools = append(pendingTools, &toolChunk)
			if len(textBlocks) == 0 {
				continue
			}
			textChunk := *msg
			textChunk.ContentBlocks = textBlocks
			if writer.Send(&textChunk, nil) {
				return
			}
		}
	}()
	return reader
}
