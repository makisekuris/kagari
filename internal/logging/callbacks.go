package logging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"kagari/internal/domain"
)

var modelCallSequence atomic.Uint64

type modelCallKey struct{}
type toolCallKey struct{}

type callState struct {
	id      string
	started time.Time
	log     *zap.Logger
	input   any
}

// CallbackObserver logs model and tool calls and joins its async stream readers before return.
type CallbackObserver struct {
	phase   string
	streams sync.WaitGroup
}

func NewCallbackObserver(phase string) *CallbackObserver {
	return &CallbackObserver{phase: phase}
}

func (o *CallbackObserver) Needed(_ context.Context, info *callbacks.RunInfo, timing callbacks.CallbackTiming) bool {
	switch timing {
	case callbacks.TimingOnStart, callbacks.TimingOnEnd, callbacks.TimingOnError:
		return true
	case callbacks.TimingOnEndWithStreamOutput:
		return isComponent(info, components.ComponentOfAgenticModel)
	default:
		return false
	}
}

func (o *CallbackObserver) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	if _, ok := input.(*model.AgenticCallbackInput); ok {
		// Graph callbacks omit provider config and duplicate the component callback lifecycle.
		modelInput := model.ConvAgenticCallbackInput(input)
		if modelInput == nil {
			return ctx
		}
		state := callState{id: fmt.Sprintf("model_call_%d", modelCallSequence.Add(1)), started: time.Now(), log: o.logger(ctx)}
		state.log.Info("model.started", zap.String("model_call_id", state.id), zap.Any("payload", map[string]any{
			"messages": modelInput.Messages, "tools": modelInput.Tools, "config": modelInput.Config,
		}))
		return context.WithValue(ctx, modelCallKey{}, state)
	}
	if toolInput := tool.ConvCallbackInput(input); toolInput != nil && isComponent(info, components.ComponentOfTool) {
		state := callState{started: time.Now(), log: o.logger(ctx), input: toolInput}
		state.log.Info("tool.started", append(toolFields(info, compose.GetToolCallID(ctx), state, ""), zap.Any("payload", map[string]any{"input": toolInput}))...)
		return context.WithValue(ctx, toolCallKey{}, state)
	}
	return ctx
}

func (o *CallbackObserver) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	if _, ok := output.(*model.AgenticCallbackOutput); ok {
		modelOutput := model.ConvAgenticCallbackOutput(output)
		if modelOutput == nil {
			return ctx
		}
		state := o.modelState(ctx)
		message := modelOutput.Message
		var usage any
		if modelOutput.TokenUsage != nil {
			usage = modelOutput.TokenUsage
		}
		if usage == nil && message != nil && message.ResponseMeta != nil && message.ResponseMeta.TokenUsage != nil {
			usage = message.ResponseMeta.TokenUsage
		}
		fields := modelFields(state, usage)
		state.log.Info("model.finished", append(fields, zap.Any("payload", map[string]any{"message": message, "usage": usage}))...)
		return ctx
	}
	if toolOutput := tool.ConvCallbackOutput(output); toolOutput != nil && isComponent(info, components.ComponentOfTool) {
		state := o.toolState(ctx)
		callID := compose.GetToolCallID(ctx)
		outcome := "success"
		if toolBusinessFailure(info.Name, toolOutput) {
			outcome = "business_failure"
		}
		state.log.Info("tool.finished", append(toolFields(info, callID, state, outcome), zap.Any("payload", map[string]any{"input": state.input, "output": toolOutput}))...)
	}
	return ctx
}

func (o *CallbackObserver) OnError(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
	if isComponent(info, components.ComponentOfAgenticModel) {
		state, ok := ctx.Value(modelCallKey{}).(callState)
		if !ok {
			return ctx
		}
		fields := append(modelFields(state, nil), zap.Any("payload", nil))
		state.log.Warn("model.failed", append(fields, ErrorFields(err)...)...)
	} else if isComponent(info, components.ComponentOfTool) {
		state := o.toolState(ctx)
		fields := append(toolFields(info, compose.GetToolCallID(ctx), state, "failed"), zap.Any("payload", map[string]any{"input": state.input}))
		state.log.Warn("tool.failed", append(fields, ErrorFields(err)...)...)
	}
	return ctx
}

func (o *CallbackObserver) OnStartWithStreamInput(ctx context.Context, _ *callbacks.RunInfo, _ *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	return ctx
}

func (o *CallbackObserver) OnEndWithStreamOutput(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	if !isComponent(info, components.ComponentOfAgenticModel) {
		output.Close()
		return ctx
	}
	state, ok := ctx.Value(modelCallKey{}).(callState)
	if !ok {
		output.Close()
		return ctx
	}
	// Each callback receives its own stream copy; draining it asynchronously preserves the caller's streaming path.
	o.streams.Add(1)
	go func() {
		defer o.streams.Done()
		defer output.Close()
		var chunks []*schema.AgenticMessage
		var usage any
		var streamErr error
		for {
			part, err := output.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				streamErr = err
				break
			}
			if _, ok := part.(*model.AgenticCallbackOutput); !ok {
				continue
			}
			callbackOutput := model.ConvAgenticCallbackOutput(part)
			if callbackOutput == nil {
				continue
			}
			if callbackOutput.Message != nil {
				chunks = append(chunks, callbackOutput.Message)
			}
			if callbackOutput.TokenUsage != nil {
				usage = callbackOutput.TokenUsage
			}
		}
		var message *schema.AgenticMessage
		if len(chunks) > 0 {
			var concatErr error
			message, concatErr = schema.ConcatAgenticMessages(chunks)
			if streamErr == nil {
				streamErr = concatErr
			}
		}
		if usage == nil && message != nil && message.ResponseMeta != nil && message.ResponseMeta.TokenUsage != nil {
			usage = message.ResponseMeta.TokenUsage
		}
		fields := modelFields(state, usage)
		payload := map[string]any{"message": message, "usage": usage}
		if streamErr != nil {
			payload["chunks"] = chunks
			fields = append(fields, zap.Any("payload", payload))
			state.log.Warn("model.failed", append(fields, ErrorFields(streamErr)...)...)
			return
		}
		state.log.Info("model.finished", append(fields, zap.Any("payload", payload))...)
	}()
	return ctx
}

func (o *CallbackObserver) Wait() { o.streams.Wait() }

func (o *CallbackObserver) logger(ctx context.Context) *zap.Logger {
	log := FromContext(ctx)
	if o.phase != "" {
		log = log.With(zap.String("phase", o.phase))
	}
	return log
}

func (o *CallbackObserver) modelState(ctx context.Context) callState {
	if state, ok := ctx.Value(modelCallKey{}).(callState); ok {
		return state
	}
	return callState{id: fmt.Sprintf("model_call_%d", modelCallSequence.Add(1)), started: time.Now(), log: o.logger(ctx)}
}

func (o *CallbackObserver) toolState(ctx context.Context) callState {
	if state, ok := ctx.Value(toolCallKey{}).(callState); ok {
		return state
	}
	return callState{started: time.Now(), log: o.logger(ctx)}
}

func modelFields(state callState, usage any) []zap.Field {
	fields := []zap.Field{zap.String("model_call_id", state.id), zap.Int64("elapsed_ms", time.Since(state.started).Milliseconds()), zap.Bool("usage_reported", usage != nil)}
	switch u := usage.(type) {
	case *model.TokenUsage:
		if u != nil {
			fields = append(fields, zap.Int("input_tokens", u.PromptTokens), zap.Int("output_tokens", u.CompletionTokens), zap.Int("total_tokens", u.TotalTokens))
		}
	case *schema.TokenUsage:
		if u != nil {
			fields = append(fields, zap.Int("input_tokens", u.PromptTokens), zap.Int("output_tokens", u.CompletionTokens), zap.Int("total_tokens", u.TotalTokens))
		}
	}
	return fields
}

func toolFields(info *callbacks.RunInfo, callID string, state callState, outcome string) []zap.Field {
	name := ""
	if info != nil {
		name = info.Name
	}
	fields := []zap.Field{zap.String("tool_name", name), zap.String("tool_call_id", callID), zap.Int64("elapsed_ms", time.Since(state.started).Milliseconds())}
	if outcome != "" {
		fields = append(fields, zap.String("outcome", outcome))
	}
	return fields
}

func isComponent(info *callbacks.RunInfo, component components.Component) bool {
	return info != nil && info.Component == component
}

func toolBusinessFailure(toolName string, output *tool.CallbackOutput) bool {
	if output == nil {
		return false
	}
	text := output.Response
	if text == "" && output.ToolOutput != nil {
		for _, part := range output.ToolOutput.Parts {
			if part.Type == schema.ToolPartTypeText {
				text += part.Text
			}
		}
	}
	var result map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &result) != nil {
		return false
	}
	for key, raw := range result {
		switch strings.ToLower(key) {
		case "ok":
			var ok bool
			if json.Unmarshal(raw, &ok) == nil && !ok {
				return true
			}
		case "error":
			var message string
			if json.Unmarshal(raw, &message) == nil && strings.TrimSpace(message) != "" {
				return true
			}
		}
	}
	if toolName == "read_url" || toolName == "browse_url" {
		// Only the read tools return source references and text pages; other tools may use an unrelated source field.
		var source *domain.Source
		if raw, ok := result["source"]; ok {
			if json.Unmarshal(raw, &source) == nil && source != nil && source.Content != "" {
				return !source.Usable()
			}
			var ref domain.SourceRef
			if json.Unmarshal(raw, &ref) != nil || ref.Status == "" {
				return false
			}
			var page struct {
				Text string `json:"text"`
			}
			if rawPage, ok := result["page"]; ok {
				_ = json.Unmarshal(rawPage, &page)
			}
			return !(domain.Source{Status: ref.Status, Truncated: ref.SourceTruncated, Content: page.Text}).Usable()
		}
	}
	return false
}
