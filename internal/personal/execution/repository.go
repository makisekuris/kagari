package execution

import (
	"context"

	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/personal/agentctx"
)

type Repository interface {
	agentctx.BudgetRepository
	agentctx.MemoryRepository
	agentctx.ReadRepository
	agentctx.ProgressRepository
	personal.TaskInputProcessingRepository
	SaveSource(context.Context, int64, string, domain.Source) (domain.SourceRef, error)
	Task(context.Context, int64) (*personal.Task, error)
}
