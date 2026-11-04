package personal

import (
	"context"
	"errors"
	"time"

	"kagari/internal/distribution"
	"kagari/internal/logging"

	"go.uber.org/zap"
)

func (r *Runtime) deliver(ctx context.Context) (worked bool, deliveryErrResult error) {
	message, err := r.Store.ClaimDelivery(ctx)
	if err != nil || message == nil {
		return false, err
	}
	log := r.eventLog(Event{ID: message.EventID, TaskID: message.TaskID}).With(zap.Int64("delivery_id", message.ID), zap.Int("attempt", message.Attempts), zap.Int("max_attempts", 3))
	ctx = logging.WithLogger(ctx, log)
	log.Info("delivery.started", zap.Any("payload", message.Text))
	defer func() {
		if deliveryErrResult != nil {
			log.Warn("delivery.persistence_failed", logging.ErrorFields(deliveryErrResult)...)
		}
	}()
	receipt, sendErr := r.Send(ctx, message.Text)
	// Telegram acceptance is a separate fact from committing its receipt locally.
	// Keep that evidence even when cancellation or a database error follows it.
	if sendErr == nil && receipt > 0 {
		log.Info("delivery.remote_accepted", zap.Int64("receipt", receipt))
	}
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
	fields := []zap.Field{zap.String("status", status), zap.String("reason", reason), zap.Duration("retry_after", delay)}
	if deliveryErr != nil {
		fields = append(fields, zap.Int("api_code", deliveryErr.APICode), zap.Bool("uncertain", deliveryErr.Uncertain))
	}
	log.Warn("delivery.remote_incomplete", fields...)
	return true, r.Store.UpdateDelivery(ctx, message.ID, status, 0, reason, time.Now().Add(delay))
}
