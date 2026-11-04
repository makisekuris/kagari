package personal

import (
	"context"

	"kagari/internal/logging"
)

func (r *Runtime) memory(ctx context.Context) (bool, error) {
	run, err := r.Store.ClaimRun(ctx, RunMemoryReview)
	if err != nil || run == nil {
		return false, err
	}
	input, err := r.Store.MemoryReviewInput(ctx, run.ID)
	if err != nil {
		return true, err
	}
	review, reviewErr := r.ReviewMemory(ctx, input)
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	return true, r.Store.FinishMemoryReview(ctx, run.ID, review, logging.ErrorReason(reviewErr))
}
