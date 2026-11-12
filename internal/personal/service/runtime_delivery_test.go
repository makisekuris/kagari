package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

func TestRuntimeDeliveryClassifiesUnknownAndRetriesOnlyKnownFailures(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		err          error
		receipt      channel.Receipt
	}{
		{"success without remote ID", "sent", nil, channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner"}},
		{"permanent", "failed", &channel.SendError{Reason: "bad recipient", Permanent: true}, channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner"}},
		{"unknown", "uncertain", errors.New("connection lost"), channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner"}},
		{"zero receipt success", "sent", nil, channel.Receipt{}},
		{"transient", "pending", &channel.SendError{Reason: "rate limited", RetryAfter: 20 * time.Millisecond}, channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testkit.Store(t, ":memory:", time.Minute)
			event := testkit.OpenUserEvent(t, s, 1, 1, "hello")
			testkit.FinishEvent(t, s, event, personal.TurnOutput{Text: "reply"}, []string{"reply"})
			r := testRuntime(s)
			attempts := 0
			r.Send = func(context.Context, channel.Content) (channel.Receipt, error) { attempts++; return tc.receipt, tc.err }
			if worked, err := r.deliver(ctx); err != nil || !worked {
				t.Fatalf("send: %v, %v", worked, err)
			}
			delivery := onlyDelivery(t, s, event.ID)
			if delivery.Status != tc.status || delivery.Attempts != 1 {
				t.Fatalf("state %+v, want status %s after first send", delivery, tc.status)
			}
			if tc.status == "pending" {
				if worked, err := r.deliver(ctx); err != nil || worked || attempts != 1 {
					t.Fatalf("retry ran before RetryAfter: worked=%v attempts=%d err=%v", worked, attempts, err)
				}
				for attempts < 3 {
					time.Sleep(25 * time.Millisecond)
					if _, err := r.deliver(ctx); err != nil {
						t.Fatal(err)
					}
				}
				delivery = onlyDelivery(t, s, event.ID)
				if delivery.Status != "failed" || delivery.Attempts != 3 || attempts != 3 {
					t.Fatalf("retry cap: %+v sends=%d", delivery, attempts)
				}
			}
			if err := s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if worked, err := r.deliver(ctx); err != nil || worked {
				t.Fatalf("terminal delivery retried: %v, %v", worked, err)
			}
		})
	}
}

func TestRuntimeReadyWaitingKeepsDeliveryPendingWithoutClaiming(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "hello")
	testkit.FinishEvent(t, s, event, personal.TurnOutput{Text: "reply"}, []string{"reply"})
	r := testRuntime(s)
	sends := 0
	r.Ready = func(context.Context) error { return &channel.SendError{Reason: "waiting for login", Waiting: true} }
	r.Send = func(context.Context, channel.Content) (channel.Receipt, error) {
		sends++
		return channel.Receipt{}, nil
	}
	if worked, err := r.deliver(ctx); err != nil || worked {
		t.Fatalf("waiting Ready() = (%v,%v)", worked, err)
	}
	delivery := onlyDelivery(t, s, event.ID)
	if delivery.Status != "pending" || delivery.Attempts != 0 || sends != 0 {
		t.Fatalf("waiting changed delivery: %+v sends=%d", delivery, sends)
	}
}

func onlyDelivery(t *testing.T, s *sqlite.Store, eventID int64) sqlite.DeliveryRecord {
	t.Helper()
	snapshot, err := s.Diagnostic(context.Background(), eventID, 0)
	if err != nil || len(snapshot.Deliveries) != 1 {
		t.Fatalf("Diagnostic() deliveries=(%+v, %v)", snapshot.Deliveries, err)
	}
	return snapshot.Deliveries[0]
}
