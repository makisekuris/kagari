package personal

import (
	"context"
	"time"
)

// Wait for cancelled stages to release the workspace before starting the run's budget.
// A nil release means the waiting run was cancelled and needs no execution.
func (r *Runtime) acquireTask(ctx context.Context, run Run) (release func(), err error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.taskMu.Lock()
		done, busy := r.activeTasks[run.TaskID]
		if !busy {
			if r.activeTasks == nil {
				r.activeTasks = make(map[int64]chan struct{})
			}
			done = make(chan struct{})
			r.activeTasks[run.TaskID] = done
			r.taskMu.Unlock()
			return func() {
				r.taskMu.Lock()
				close(done)
				delete(r.activeTasks, run.TaskID)
				r.taskMu.Unlock()
			}, nil
		}
		r.taskMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.idleDelay):
			current, err := r.Store.Run(ctx, run.ID)
			if err != nil {
				return nil, err
			}
			if current == nil || current.Status != RunRunning {
				return nil, nil
			}
		}
	}
}
