package personal

import (
	"context"
	"database/sql"

	"go.uber.org/zap"
	"kagari/internal/logging"
)

// Log an outcome only after commit so rolled-back writes are never reported as saved.
func commitStoreOutcome(ctx context.Context, tx interface{ Commit() error }, operation, outcome string, payload any, fields ...zap.Field) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	fields = append([]zap.Field{zap.String("operation", operation), zap.String("outcome", outcome)}, fields...)
	if payload != nil {
		fields = append(fields, zap.Any("payload", payload))
	}
	logging.FromContext(ctx).Info("store.outcome", fields...)
	return nil
}

type taskOutcomePayload struct {
	Task Task `json:"task"`
	Run  *Run `json:"run,omitempty"`
}

type stageOutcomePayload struct {
	Result  StageResult `json:"result"`
	Failure string      `json:"failure,omitempty"`
}

type memoryReviewOutcomePayload struct {
	Review  MemoryReview `json:"review"`
	Applied []Memory     `json:"applied"`
	Failure string       `json:"failure,omitempty"`
}

type recoveryOutcomePayload struct {
	RequeuedEvents int64 `json:"requeued_events"`
	RequeuedRuns   int64 `json:"requeued_runs"`
	BudgetEvents   int64 `json:"budget_events"`
	FailedRuns     int64 `json:"failed_runs"`
	WaitingTasks   int64 `json:"waiting_tasks"`
	QueuedTasks    int64 `json:"queued_tasks"`
	UncertainSends int64 `json:"uncertain_sends"`
}

type deliveryOutcomePayload struct {
	Status        string `json:"status"`
	Receipt       int64  `json:"receipt,omitempty"`
	Detail        string `json:"detail,omitempty"`
	NextAttemptAt string `json:"next_attempt_at"`
}

func changedRows(result sql.Result) (int64, error) {
	return result.RowsAffected()
}
