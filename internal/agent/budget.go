package agent

import (
	"context"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"kagari/internal/inputbudget"
	"kagari/internal/logging"
)

func (e *Engine) budgetModel(ctx context.Context) (model.AgenticModel, error) {
	if bounded, ok := e.Model.(*inputbudget.AgenticModel); ok {
		return bounded, nil
	}
	limits, err := inputbudget.NewLimits(e.Config.Model.ContextWindow, int64(e.Config.Model.MaxOutputTokens))
	if err != nil {
		return nil, err
	}
	return inputbudget.NewAgenticModel(e.Model, inputbudget.ModelConfig{Limits: limits, RequiredPrefix: func(messages []*schema.AgenticMessage) int { return min(2, len(messages)) }, Observe: func(r inputbudget.ModelRecord) { logging.FromContext(ctx).Info("context.limit", zap.Any("usage", r)) }})
}
