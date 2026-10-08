package distribution

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"kagari/internal/domain"
)

func TestPlanAndSendRoutePerTarget(t *testing.T) {
	var sends []string
	d := Dispatcher{
		"first": {
			Prepare: func(content domain.Content) ([]string, error) {
				if content.Format != domain.ContentMarkdown {
					t.Fatalf("content format = %q, want Markdown", content.Format)
				}
				return []string{content.Text, "tail"}, nil
			},
			Send: func(context.Context, domain.DeliveryTarget, domain.Content) (string, error) { return "", nil },
		},
		"second": {
			Prepare: func(content domain.Content) ([]string, error) {
				return []string{content.Text}, nil
			},
			Send: func(_ context.Context, target domain.DeliveryTarget, content domain.Content) (string, error) {
				if content.Format != domain.ContentMarkdown {
					t.Fatalf("send content format = %q, want Markdown", content.Format)
				}
				sends = append(sends, target.Channel+":"+target.Address+":"+content.Text)
				return "42", nil
			},
		},
	}
	targets := []domain.DeliveryTarget{
		{Channel: "first", Address: "me"},
		{Channel: "first", Address: "me"},
		{Channel: "second", Address: "channel"},
	}
	deliveries, err := d.Plan(targets, domain.Content{Text: "body", Format: domain.ContentMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	if len(sends) != 0 {
		t.Fatal("planning called a remote sender")
	}
	want := []domain.Delivery{
		{Target: targets[0], Part: 1, Text: "body", Format: domain.ContentMarkdown},
		{Target: targets[0], Part: 2, Text: "tail", Format: domain.ContentMarkdown},
		{Target: targets[2], Part: 1, Text: "body", Format: domain.ContentMarkdown},
	}
	if !reflect.DeepEqual(deliveries, want) {
		t.Fatalf("deliveries = %#v, want %#v", deliveries, want)
	}
	if id, err := d.Send(context.Background(), deliveries[2]); err != nil || id != "42" {
		t.Fatalf("Send() = %q, %v; want 42, nil", id, err)
	}
	if !reflect.DeepEqual(sends, []string{"second:channel:body"}) {
		t.Fatalf("sends = %#v", sends)
	}
}

func TestPlanAndSendRejectInvalidTargets(t *testing.T) {
	d := Dispatcher{
		"broken": {Prepare: func(domain.Content) ([]string, error) { return nil, nil }, Send: func(context.Context, domain.DeliveryTarget, domain.Content) (string, error) { return "", nil }},
		"empty": {Prepare: func(domain.Content) ([]string, error) {
			return []string{""}, nil
		}, Send: func(context.Context, domain.DeliveryTarget, domain.Content) (string, error) { return "1", nil }},
		"space": {Prepare: func(domain.Content) ([]string, error) {
			return []string{" "}, nil
		}, Send: func(context.Context, domain.DeliveryTarget, domain.Content) (string, error) { return "1", nil }},
		"failure": {Prepare: func(domain.Content) ([]string, error) {
			return nil, &SendError{Reason: "could not prepare", Permanent: true}
		}, Send: func(context.Context, domain.DeliveryTarget, domain.Content) (string, error) { return "1", nil }},
	}
	for _, target := range []domain.DeliveryTarget{
		{Channel: "missing", Address: "x"},
		{Channel: "broken", Address: "x"},
		{Channel: "broken", Address: " "},
		{Channel: "failure", Address: "x"},
	} {
		_, err := d.Plan([]domain.DeliveryTarget{target}, domain.Content{Text: "body"})
		var sendErr *SendError
		if !errors.As(err, &sendErr) || !sendErr.Permanent {
			t.Fatalf("Plan(%#v) error = %v, want permanent SendError", target, err)
		}
	}
	_, err := d.Plan([]domain.DeliveryTarget{{Channel: "broken", Address: "x"}}, domain.Content{Text: "body"})
	var sendErr *SendError
	if !errors.As(err, &sendErr) || !sendErr.Permanent {
		t.Fatalf("empty chunks error = %v, want permanent SendError", err)
	}
	_, err = d.Plan([]domain.DeliveryTarget{{Channel: "empty", Address: "x"}}, domain.Content{Text: "body"})
	if !errors.As(err, &sendErr) || !sendErr.Permanent {
		t.Fatalf("empty message error = %v, want permanent SendError", err)
	}
	if got, err := d.Plan([]domain.DeliveryTarget{{Channel: "space", Address: "x"}}, domain.Content{Text: "body"}); err != nil || len(got) != 1 || got[0].Text != " " {
		t.Fatalf("whitespace chunk plan = %#v, %v", got, err)
	}
	if _, err := (Dispatcher{}).Send(context.Background(), domain.Delivery{Target: domain.DeliveryTarget{Channel: "missing", Address: "x"}}); !errors.As(err, &sendErr) || !sendErr.Permanent {
		t.Fatalf("unknown channel error = %v, want permanent SendError", err)
	}
	if _, err := d.Send(context.Background(), domain.Delivery{Target: domain.DeliveryTarget{Channel: "broken", Address: "x"}, Text: "body"}); !errors.As(err, &sendErr) || !sendErr.Uncertain {
		t.Fatalf("missing receipt error = %v, want uncertain SendError", err)
	}
}
