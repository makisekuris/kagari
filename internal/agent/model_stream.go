package agent

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"
)

type normalizedResponsesModel struct {
	*agenticopenai.ResponsesModel
}

func (m *normalizedResponsesModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	source, err := m.ResponsesModel.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return normalizeToolCallStream(source), nil
}

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
