package service

import (
	"context"

	"kagari/internal/logging"
	"kagari/internal/personal"
)

func (r *Runtime) memory(ctx context.Context) (worked bool, runErr error) {
	run, err := r.Store.ClaimRun(ctx, personal.RunMemoryReview)
	if err != nil || run == nil {
		return false, err
	}
	log := r.runLog(*run)
	ctx = logging.WithLogger(ctx, log)
	log.Info("memory.review.started")
	defer func() {
		if runErr != nil {
			log.Warn("memory.review.persistence_failed", logging.ErrorFields(runErr)...)
		}
	}()
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
