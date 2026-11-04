package personal

import (
	"context"
	"testing"
	"time"
)

func TestRuntimeCompletesLegacyMemoryCallbacksSilently(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, ":memory:", time.Minute)
			event := openUserEvent(t, s, 1, 1, "I prefer concise answers.")
			finishEvent(t, s, event, TurnOutput{}, nil)
			run, err := s.ClaimRun(ctx, RunMemory)
			if err != nil || run == nil {
				t.Fatalf("claim memory run: %+v, %v", run, err)
			}
			if err := s.FinishMemoryReview(ctx, run.ID, MemoryReview{}, "review failed"); err != nil {
				t.Fatal(err)
			}
			// Reproduce a callback persisted by an older service version.
			res, err := s.db.ExecContext(ctx, `INSERT INTO personal_events(kind,text,run_id,status,created_at) VALUES('memory','review failed',?,?,?)`, run.ID, status, timestamp(time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			callbackID, err := res.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if status == "running" {
				if err := s.Recover(ctx); err != nil {
					t.Fatal(err)
				}
			}
			r := testRuntime(s)
			r.Think = func(context.Context, TurnInput) (TurnOutput, error) {
				t.Fatal("memory callback invoked the conversation model")
				return TurnOutput{}, nil
			}
			r.Prepare = func(string) ([]string, error) {
				t.Fatal("memory callback prepared a notification")
				return nil, nil
			}
			if worked, err := r.turn(ctx); err != nil || !worked {
				t.Fatalf("memory callback: worked=%v, %v", worked, err)
			}
			var storedStatus EventStatus
			if err := s.db.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, callbackID).Scan(&storedStatus); err != nil || storedStatus != EventCompleted {
				t.Fatalf("callback status: %s, %v", storedStatus, err)
			}
			storedRun, err := s.Run(ctx, run.ID)
			if err != nil || storedRun == nil || storedRun.Status != RunFailed || storedRun.Error != "review failed" {
				t.Fatalf("memory failure lost: %+v, %v", storedRun, err)
			}
			var messages int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_history WHERE event_id=?`, callbackID).Scan(&messages); err != nil || messages != 0 {
				t.Fatalf("memory callback entered conversation history: %d, %v", messages, err)
			}
			if delivery, err := s.ClaimDelivery(ctx); err != nil || delivery != nil {
				t.Fatalf("memory callback notified: %+v, %v", delivery, err)
			}
		})
	}
}
