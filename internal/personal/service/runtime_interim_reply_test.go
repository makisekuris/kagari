package service

import (
	"context"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRuntimeDeliversInterimTextWhileThinkContinues(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	r := testRuntime(s)
	const initial = "离线使用要求会影响部署方案。"
	const final = "完整比较：方案甲支持离线部署。"
	queued := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	defer close(release)
	r.Think = func(ctx context.Context, in personal.TurnInput) (personal.TurnOutput, error) {
		if err := in.OnInterimReply(ctx, initial); err != nil {
			return personal.TurnOutput{}, err
		}
		close(queued)
		<-release
		return personal.TurnOutput{Text: final, ReplyKind: personal.ReplyContent}, nil
	}
	presenterCalls := 0
	r.PresentReply = func(_ context.Context, _ personal.Event, text string) (string, error) {
		presenterCalls++
		return text, nil
	}
	eventID := testkit.AcceptInput(t, s, "1", testkit.SourceInput("1", "比较支持离线使用的部署方案。"))
	go func() {
		_, err := r.turn(ctx)
		done <- err
	}()
	receive(t, queued)
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil || message.Text != initial {
		t.Fatalf("interim delivery=%+v error=%v", message, err)
	}
	if err := s.UpdateDelivery(ctx, message.ID, "sent", channel.Receipt{MessageID: "initial"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if intents, err := s.EventDeliveries(ctx, eventID); err != nil || len(intents) != 0 {
		t.Fatalf("interim text became a completion cursor: %+v, %v", intents, err)
	}
	if presenterCalls != 0 {
		t.Fatalf("interim text invoked an extra model presenter %d times", presenterCalls)
	}
	release <- struct{}{}
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	message, err = s.ClaimDelivery(ctx)
	if err != nil || message == nil || message.Text != final || presenterCalls != 1 {
		t.Fatalf("final delivery=%+v error=%v presenter calls=%d", message, err, presenterCalls)
	}
	if err := s.UpdateDelivery(ctx, message.ID, "sent", channel.Receipt{MessageID: "final"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if extra, err := s.ClaimDelivery(ctx); err != nil || extra != nil {
		t.Fatalf("unexpected receipt=%+v error=%v", extra, err)
	}
}
