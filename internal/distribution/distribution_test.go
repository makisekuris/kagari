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
			Prepare: func(text string) []string { return []string{text, "tail"} },
			Send:    func(context.Context, domain.DeliveryTarget, string) (string, error) { return "", nil },
		},
		"second": {
			Prepare: func(text string) []string { return []string{text} },
			Send: func(_ context.Context, target domain.DeliveryTarget, text string) (string, error) {
				sends = append(sends, target.Channel+":"+target.Address+":"+text)
				return "42", nil
			},
		},
	}
	targets := []domain.DeliveryTarget{
		{Channel: "first", Address: "me"},
		{Channel: "first", Address: "me"},
		{Channel: "second", Address: "channel"},
	}
	deliveries, err := d.Plan(targets, "body")
	if err != nil {
		t.Fatal(err)
	}
	if len(sends) != 0 {
		t.Fatal("planning called a remote sender")
	}
	want := []domain.Delivery{
		{Target: targets[0], Part: 1, Text: "body"},
		{Target: targets[0], Part: 2, Text: "tail"},
		{Target: targets[2], Part: 1, Text: "body"},
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
		"broken": {Prepare: func(string) []string { return nil }, Send: func(context.Context, domain.DeliveryTarget, string) (string, error) { return "", nil }},
		"empty":  {Prepare: func(string) []string { return []string{""} }, Send: func(context.Context, domain.DeliveryTarget, string) (string, error) { return "1", nil }},
		"space":  {Prepare: func(string) []string { return []string{" "} }, Send: func(context.Context, domain.DeliveryTarget, string) (string, error) { return "1", nil }},
	}
	for _, target := range []domain.DeliveryTarget{
		{Channel: "missing", Address: "x"},
		{Channel: "broken", Address: "x"},
		{Channel: "broken", Address: " "},
	} {
		_, err := d.Plan([]domain.DeliveryTarget{target}, "body")
		var sendErr *SendError
		if !errors.As(err, &sendErr) || !sendErr.Permanent {
			t.Fatalf("Plan(%#v) error = %v, want permanent SendError", target, err)
		}
	}
	_, err := d.Plan([]domain.DeliveryTarget{{Channel: "broken", Address: "x"}}, "body")
	var sendErr *SendError
	if !errors.As(err, &sendErr) || !sendErr.Permanent {
		t.Fatalf("empty chunks error = %v, want permanent SendError", err)
	}
	_, err = d.Plan([]domain.DeliveryTarget{{Channel: "empty", Address: "x"}}, "body")
	if !errors.As(err, &sendErr) || !sendErr.Permanent {
		t.Fatalf("empty message error = %v, want permanent SendError", err)
	}
	if got, err := d.Plan([]domain.DeliveryTarget{{Channel: "space", Address: "x"}}, "body"); err != nil || len(got) != 1 || got[0].Text != " " {
		t.Fatalf("whitespace chunk plan = %#v, %v", got, err)
	}
	if _, err := (Dispatcher{}).Send(context.Background(), domain.Delivery{Target: domain.DeliveryTarget{Channel: "missing", Address: "x"}}); !errors.As(err, &sendErr) || !sendErr.Permanent {
		t.Fatalf("unknown channel error = %v, want permanent SendError", err)
	}
	if _, err := d.Send(context.Background(), domain.Delivery{Target: domain.DeliveryTarget{Channel: "broken", Address: "x"}, Text: "body"}); !errors.As(err, &sendErr) || !sendErr.Uncertain {
		t.Fatalf("missing receipt error = %v, want uncertain SendError", err)
	}
}
