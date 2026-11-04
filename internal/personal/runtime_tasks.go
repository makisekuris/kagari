package personal

import (
	"context"
	"time"

	"go.uber.org/zap"
	"kagari/internal/logging"
)

func (r *Runtime) task(ctx context.Context) (worked bool, runErr error) {
	run, err := r.Store.ClaimRun(ctx, RunTask)
	if err != nil || run == nil {
		return false, err
	}
	log := r.runLog(*run)
	ctx = logging.WithLogger(ctx, log)
	log.Info("run.started", zap.Int64("remaining_ms", run.RemainingMS), zap.Any("payload", run.Input))
	defer func() {
		if runErr != nil {
			log.Warn("run.execution.failed", logging.ErrorFields(runErr)...)
		}
	}()
	remaining := time.Duration(run.RemainingMS) * time.Millisecond
	if remaining <= 0 {
		remaining = time.Millisecond
	}
	runCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	started := time.Now()
	elapsed := func() time.Duration { return time.Duration(run.ElapsedMS)*time.Millisecond + time.Since(started) }
	watchDone := make(chan error, 1)
	go func() {
		for wait(runCtx, r.idleDelay) {
			left, err := r.Store.AccountRun(runCtx, run.ID, elapsed())
			if err != nil {
				if runCtx.Err() == nil {
					cancel()
					watchDone <- err
					return
				}
				break
			}
			current, err := r.Store.Run(runCtx, run.ID)
			if err != nil {
				if runCtx.Err() == nil {
					cancel()
					watchDone <- err
					return
				}
				break
			}
			if left <= 0 || current == nil || current.Status != RunRunning {
				cancel()
				break
			}
		}
		watchDone <- nil
	}()
	result, executeErr := r.Execute(runCtx, *run, func(progress string) error {
		return r.Store.Progress(runCtx, run.ID, progress)
	})
	cancel()
	if err := <-watchDone; err != nil {
		return true, err
	}
	persistCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	_, err = r.Store.AccountRun(persistCtx, run.ID, elapsed())
	stop()
	if err != nil {
		return true, err
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	reason := ""
	if executeErr != nil {
		reason = logging.ErrorReason(executeErr)
	}
	return true, r.Store.FinishStage(ctx, run.ID, result, reason)
}
