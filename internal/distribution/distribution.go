package distribution

import (
	"context"
	"strings"
	"time"

	"kagari/internal/domain"
)

type SendError struct {
	Reason     string
	RetryAfter time.Duration
	Permanent  bool
	Uncertain  bool
}

func (e *SendError) Error() string {
	if e == nil {
		return ""
	}
	return e.Reason
}

type Adapter struct {
	Prepare func(domain.Content) ([]string, error)
	Send    func(context.Context, domain.DeliveryTarget, domain.Content) (string, error)
}

type Dispatcher map[string]Adapter

func (d Dispatcher) Plan(targets []domain.DeliveryTarget, content domain.Content) ([]domain.Delivery, error) {
	var deliveries []domain.Delivery
	seen := make(map[domain.DeliveryTarget]struct{}, len(targets))
	for _, target := range targets {
		if strings.TrimSpace(target.Channel) == "" || strings.TrimSpace(target.Address) == "" {
			return nil, &SendError{Reason: "distribution target is incomplete", Permanent: true}
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		adapter, ok := d[target.Channel]
		if !ok || adapter.Prepare == nil || adapter.Send == nil {
			return nil, &SendError{Reason: "distribution channel is unavailable", Permanent: true}
		}
		chunks, err := adapter.Prepare(content)
		if err != nil {
			return nil, err
		}
		if content.Text != "" && len(chunks) == 0 {
			return nil, &SendError{Reason: "distribution produced no message", Permanent: true}
		}
		for i, chunk := range chunks {
			if chunk == "" {
				return nil, &SendError{Reason: "distribution produced an empty message", Permanent: true}
			}
			deliveries = append(deliveries, domain.Delivery{Target: target, Part: i + 1, Text: chunk, Format: content.Format})
		}
	}
	return deliveries, nil
}

func (d Dispatcher) Send(ctx context.Context, delivery domain.Delivery) (string, error) {
	target := delivery.Target
	if strings.TrimSpace(target.Channel) == "" || strings.TrimSpace(target.Address) == "" {
		return "", &SendError{Reason: "distribution target is incomplete", Permanent: true}
	}
	adapter, ok := d[target.Channel]
	if !ok || adapter.Prepare == nil || adapter.Send == nil {
		return "", &SendError{Reason: "distribution channel is unavailable", Permanent: true}
	}
	messageID, err := adapter.Send(ctx, target, domain.Content{Text: delivery.Text, Format: delivery.Format})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(messageID) == "" {
		return "", &SendError{Reason: "distribution send receipt is unavailable", Uncertain: true}
	}
	return messageID, nil
}
