package service

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"kagari/internal/logging"
	"kagari/internal/personal"
)

func (r *Runtime) task(ctx context.Context) (worked bool, runErr error) {
	run, err := r.Store.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		return false, err
	}
	release, err := r.acquireTask(ctx, *run)
	if err != nil || release == nil {
		return true, err
	}
	defer release()
	run, err = r.Store.BeginScheduledRun(ctx, run.ID, time.Now().UTC())
	if err != nil || run == nil {
		return true, err
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
			if left <= 0 || current == nil || current.Status != personal.RunRunning {
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
	finishErr := r.Store.FinishStage(ctx, run.ID, result, reason)
	if !errors.Is(finishErr, personal.ErrAttachmentUnavailable) {
		return true, finishErr
	}
	// Deletion can race stage saving; the rejected transaction applied none of these reports.
	result.Attachments = nil
	result.Progress = nil
	result.AppliedInputIDs = nil
	const unavailableAttachmentFailure = "an attachment became unavailable before stage results were saved; this stage's progress and task input reports were not applied"
	if reason != "" {
		reason += "; "
	}
	reason += unavailableAttachmentFailure
	return true, r.Store.FinishStage(ctx, run.ID, result, reason)
}
