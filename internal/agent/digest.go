package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"kagari/internal/digest"
	"kagari/internal/domain"
	"kagari/internal/logging"
)

// GenerateDigest renders the frozen digest snapshot in one tool-free model call.
func (e *Engine) GenerateDigest(ctx context.Context, input domain.DigestInput) (review domain.DigestReview, usage domain.Usage, err error) {
	started := time.Now()
	stage := "agent_setup"
	modelCalled, usageReported := false, false
	if e.Log != nil {
		e.Log.Info("agent digest started", zap.Int64("user_id", input.UserID), zap.Int("entries", len(input.Entries)), zap.Bool("streaming", e.Config.Agent.Streaming))
	}
	ctx, cancel := context.WithTimeout(ctx, e.Config.Agent.Timeout)
	defer cancel()
	defer func() {
		if e.Log == nil {
			return
		}
		fields := []zap.Field{zap.String("stage", stage), zap.Duration("elapsed", time.Since(started)),
			zap.Bool("model_called", modelCalled), zap.Bool("usage_reported", usageReported),
			zap.Int("input_tokens", usage.InputTokens), zap.Int("output_tokens", usage.OutputTokens), zap.Int("total_tokens", usage.TotalTokens)}
		if err != nil {
			e.Log.Warn("agent digest failed", append(fields, logging.ErrorFields(err)...)...)
		} else {
			e.Log.Info("agent digest finished", fields...)
		}
	}()

	instruction := input.Instruction
	if instruction == "" {
		instruction = e.DigestPrompt()
	}
	a, err := adk.NewTypedChatModelAgent(ctx, &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: "weekly_digest", Description: "按输入分析生成周报", Instruction: instruction,
		Model: e.Model, MaxIterations: 1,
	})
	if err != nil {
		return review, usage, err
	}
	userInput := input
	userInput.Instruction = ""
	userJSON, err := json.Marshal(userInput)
	if err != nil {
		return review, usage, fmt.Errorf("marshal digest input: %w", err)
	}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: a, EnableStreaming: e.Config.Agent.Streaming})
	stage = "model"
	modelCalled = true
	it := runner.Query(ctx, string(userJSON), adk.WithChatModelOptions([]model.Option{agenticopenai.WithResponsesText(digestFormat())}))
	output, gotUsage, reported, runErr := e.consumeEvents(it, 1)
	usage, usageReported = gotUsage, reported
	if runErr != nil {
		return review, usage, runErr
	}
	stage = "decode_digest"
	if err := decode(output, &review); err != nil {
		return review, usage, fmt.Errorf("invalid structured digest: %w", err)
	}
	stage = "validate_digest"
	if err := digest.ValidateReview(input, review); err != nil {
		return review, usage, err
	}
	return review, usage, nil
}
