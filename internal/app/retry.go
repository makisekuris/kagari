package app

import (
	"context"
	"fmt"
	"time"

	"kagari/internal/domain"
)

// ProcessJob runs only the selected CLI job, honoring the persisted retry schedule.
func (w *Worker) ProcessJob(ctx context.Context, id int64) (*domain.Job, error) {
	return w.processJob(ctx, id, pause)
}

func (w *Worker) processJob(ctx context.Context, id int64, wait func(context.Context, time.Duration) bool) (*domain.Job, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		job, err := w.Store.Job(ctx, id)
		if err != nil {
			return nil, err
		}
		if job == nil {
			return nil, fmt.Errorf("task #%d does not exist", id)
		}
		switch job.Status {
		case "completed":
			return job, nil
		case "failed":
			return job, fmt.Errorf("task #%d failed after %d attempts; inspect local logs or retry", id, job.Attempts)
		case "pending":
			if delay := time.Until(job.NextAttemptAt); delay > 0 && !wait(ctx, delay) {
				return job, ctx.Err()
			}
			job, err = w.Store.StartJob(ctx, id)
			if err != nil {
				return nil, err
			}
			if err := w.Process(ctx, job); err != nil {
				persisted, stateErr := w.Store.Job(ctx, id)
				if stateErr != nil {
					return nil, stateErr
				}
				if persisted == nil || persisted.Status == "processing" {
					return persisted, err
				}
			}
		default:
			return job, fmt.Errorf("task #%d cannot be processed in state %q", id, job.Status)
		}
	}
}
