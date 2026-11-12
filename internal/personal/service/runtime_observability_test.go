package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

type receiptFailureStore struct{ Repository }

func (s receiptFailureStore) UpdateDelivery(context.Context, int64, string, channel.Receipt, string, time.Time) error {
	return errors.New("receipt storage unavailable")
}

// A lost local receipt must never erase the evidence that Telegram accepted the
// message, or make an operator mistake a possible duplicate for a safe retry.
func TestDeliveryTraceSeparatesRemoteAcceptanceAndPersistence(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 10, "hello")
	testkit.FinishEvent(t, s, event, personal.TurnOutput{Text: "reply"}, []string{"reply"})
	core, logs := observer.New(zap.InfoLevel)
	r := testRuntime(receiptFailureStore{s})
	r.Log = zap.New(core)
	sends := 0
	r.Send = func(context.Context, channel.Content) (channel.Receipt, error) {
		sends++
		return channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner"}, nil
	}
	if worked, err := r.deliver(ctx); !worked || err == nil {
		t.Fatalf("lost receipt = %v, %v", worked, err)
	}
	accepted := logs.FilterMessage("delivery.remote_accepted").All()
	if len(accepted) != 1 || accepted[0].ContextMap()["receipt"] == nil || accepted[0].ContextMap()["event_id"] != event.ID {
		t.Fatalf("remote evidence: %+v", accepted)
	}
	if logs.FilterMessage("delivery.persistence_failed").Len() != 1 {
		t.Fatal("missing local persistence failure")
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if worked, err := r.deliver(ctx); worked || err != nil || sends != 1 {
		t.Fatalf("uncertain delivery retried: %v, %v, sends=%d", worked, err, sends)
	}
	delivery := onlyDelivery(t, s, event.ID)
	if delivery.Status != "uncertain" || delivery.Attempts != 1 {
		t.Fatalf("uncertain delivery state: %+v", delivery)
	}
}

// A normal chat creates no Task; its event ID must still connect the model turn
// to the queued reply and delivery so the common path is diagnosable.
func TestConversationTraceConnectsAnOrdinaryChat(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	id := testkit.AcceptInput(t, s, "1", testkit.SourceInput("10", "hello"))
	core, logs := observer.New(zap.InfoLevel)
	r := testRuntime(s)
	r.Log = zap.New(core)
	r.Think = func(context.Context, personal.TurnInput) (personal.TurnOutput, error) {
		return personal.TurnOutput{Text: "reply"}, nil
	}
	if _, err := r.turn(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.deliver(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"conversation.started", "conversation.reply_queued", "conversation.finished", "delivery.started", "delivery.remote_accepted"} {
		entries := logs.FilterMessage(name).All()
		if len(entries) != 1 || entries[0].ContextMap()["event_id"] != id {
			t.Fatalf("%s lost chat identity: %+v", name, entries)
		}
	}
	snapshot, err := s.Diagnostic(ctx, id, 0)
	if err != nil || len(snapshot.Deliveries) != 1 || snapshot.Deliveries[0].Status != "sent" {
		t.Fatalf("ordinary chat delivery state: %+v, %v", snapshot.Deliveries, err)
	}
}
