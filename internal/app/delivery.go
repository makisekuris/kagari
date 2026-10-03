package app

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"kagari/internal/distribution"
)

func deliver(ctx context.Context, w *Worker, dispatcher distribution.Dispatcher) error {
	for ctx.Err() == nil {
		d, err := w.Store.ClaimDelivery(ctx)
		if err != nil {
			return err
		}
		if d == nil {
			if !pause(ctx, time.Second) {
				break
			}
			continue
		}
		messageID, sendErr := dispatcher.Send(ctx, *d)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if sendErr == nil {
			err = w.Store.SentDeliveryReceipt(persistCtx, d.ID, messageID)
		} else {
			failure := &distribution.SendError{Reason: "delivery outcome is unknown", Uncertain: true}
			_ = errors.As(sendErr, &failure)
			attempts := w.Config.MaxAttempts
			if failure.Permanent {
				attempts = 1
			}
			delay := backoff(d.Attempts)
			if failure.RetryAfter > delay {
				delay = failure.RetryAfter
			}
			// 未知结果不自动重试；只有用户显式 retry_delivery 才承担重复发送风险。
			err = w.Store.FailDelivery(persistCtx, d.ID, failure.Reason, attempts, time.Now().Add(delay), failure.Uncertain)
			w.Log.Warn("delivery failed", zap.Int64("job_id", d.JobID), zap.Int64("delivery_id", d.ID), zap.Bool("uncertain", failure.Uncertain))
		}
		cancel()
		if err != nil {
			return err
		}
		// ponytail: one sender, one message per second; introduce per-chat rate
		// limits only if this personal bot needs higher throughput.
		if !pause(ctx, time.Second) {
			break
		}
	}
	return nil
}

func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
