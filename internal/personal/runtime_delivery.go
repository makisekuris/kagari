package personal

import (
	"context"
	"errors"
	"time"

	"kagari/internal/distribution"

	"go.uber.org/zap"
)

func (r *Runtime) deliver(ctx context.Context) (bool, error) {
	message, err := r.Store.ClaimDelivery(ctx)
	if err != nil || message == nil {
		return false, err
	}
	receipt, sendErr := r.Send(ctx, message.Text)
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if sendErr == nil && receipt > 0 {
		return true, r.Store.UpdateDelivery(ctx, message.ID, "sent", receipt, "", time.Time{})
	}
	status, reason, delay := "uncertain", "send outcome unknown", time.Second
	var deliveryErr *distribution.SendError
	if errors.As(sendErr, &deliveryErr) {
		reason = deliveryErr.Reason
		if deliveryErr.Permanent {
			status = "failed"
		} else if !deliveryErr.Uncertain {
			// Three total attempts per outbox row, including the first; a new row starts at zero.
			status = "failed"
			if message.Attempts < 3 {
				status = "pending"
			}
			if deliveryErr.RetryAfter > 0 {
				delay = deliveryErr.RetryAfter
			}
		}
	}
	r.Log.Warn("personal delivery incomplete", zap.Int64("delivery_id", message.ID), zap.String("status", status), zap.String("reason", reason))
	return true, r.Store.UpdateDelivery(ctx, message.ID, status, 0, reason, time.Now().Add(delay))
}
