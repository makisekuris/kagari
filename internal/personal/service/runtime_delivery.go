package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/logging"
	"kagari/internal/personal"
)

const deliveryPersistenceTimeout = 5 * time.Second

type deliveryPersistenceError struct {
	deliveryID int64
	err        error
}

func (e *deliveryPersistenceError) Error() string {
	return fmt.Sprintf("personal: persist delivery %d outcome: %v", e.deliveryID, e.err)
}

func (e *deliveryPersistenceError) Unwrap() error { return e.err }

func (r *Runtime) deliver(ctx context.Context) (worked bool, deliveryErrResult error) {
	if r.Ready != nil {
		if err := r.Ready(ctx); err != nil {
			var sendErr *channel.SendError
			if errors.As(err, &sendErr) && sendErr.Waiting {
				return false, nil
			}
			return false, err
		}
	}
	message, err := r.Store.ClaimDelivery(ctx)
	if err != nil || message == nil {
		return false, err
	}
	log := r.eventLog(personal.Event{ID: message.EventID, TaskID: message.TaskID}).With(zap.Int64("delivery_id", message.ID), zap.Int("attempt", message.Attempts), zap.Int("max_attempts", 3))
	ctx = logging.WithLogger(ctx, log)
	log.Info("delivery.started", zap.Any("payload", message.Text), zap.String("attachment_id", message.AttachmentID))
	defer func() {
		if deliveryErrResult != nil {
			log.Warn("delivery.persistence_failed", logging.ErrorFields(deliveryErrResult)...)
		}
	}()
	receipt, sendErr := r.Send(ctx, channel.Content{Text: message.Text, AttachmentID: message.AttachmentID})
	if sendErr == nil {
		log.Info("delivery.remote_accepted", zap.Any("receipt", receipt))
	}
	if sendErr == nil {
		return true, r.persistDeliveryOutcome(ctx, message.ID, "sent", receipt, "", time.Time{})
	}
	status, reason, delay := "uncertain", "send outcome unknown", time.Second
	var deliveryErr *channel.SendError
	if errors.As(sendErr, &deliveryErr) {
		reason = deliveryErr.Reason
		if deliveryErr.Waiting {
			status = "waiting"
		} else if deliveryErr.Permanent {
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
		fields = append(fields, zap.Bool("uncertain", deliveryErr.Uncertain), zap.Bool("waiting", deliveryErr.Waiting))
	}
	log.Warn("delivery.remote_incomplete", fields...)
	return true, r.persistDeliveryOutcome(ctx, message.ID, status, receipt, reason, time.Now().Add(delay))
}

func (r *Runtime) persistDeliveryOutcome(ctx context.Context, deliveryID int64, status string, receipt channel.Receipt, reason string, nextAttempt time.Time) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryPersistenceTimeout)
	defer cancel()
	if err := r.Store.UpdateDelivery(persistCtx, deliveryID, status, receipt, reason, nextAttempt); err != nil {
		return &deliveryPersistenceError{deliveryID: deliveryID, err: err}
	}
	return nil
}
