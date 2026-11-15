package sqlite

import (
	"context"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func TestQueueInterimReplyPreservesExecutionAndFinalDelivery(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	event := openUserEvent(t, s, 1, 1, "比较离线部署方案")
	const initial = "离线要求会影响部署方式。"
	if err := s.QueueInterimReply(ctx, event.ID, initial, []string{initial}); err != nil {
		t.Fatal(err)
	}
	var status personal.EventStatus
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, event.ID).Scan(&status); err != nil || status != personal.EventRunning {
		t.Fatalf("event status=%q error=%v", status, err)
	}
	if intents, err := s.EventDeliveries(ctx, event.ID); err != nil || len(intents) != 0 {
		t.Fatalf("interim text became an unfinished settlement: %+v, %v", intents, err)
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil || message.Text != initial {
		t.Fatalf("interim delivery=%+v error=%v", message, err)
	}
	if err := s.UpdateDelivery(ctx, message.ID, "sent", channel.Receipt{MessageID: "initial"}, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{Text: "最终结果"}, []string{"最终结果"})
	message, err = s.ClaimDelivery(ctx)
	if err != nil || message == nil || message.Text != "最终结果" {
		t.Fatalf("final delivery=%+v error=%v", message, err)
	}
	var histories int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=? AND role='assistant'`, event.ID).Scan(&histories); err != nil || histories != 2 {
		t.Fatalf("history count=%d error=%v", histories, err)
	}
	if err := s.QueueInterimReply(ctx, event.ID, "late", []string{"late"}); err == nil {
		t.Fatal("completed event accepted another interim reply")
	}
}
