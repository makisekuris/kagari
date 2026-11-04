package personal

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/distribution"
)

func TestRuntimeDeliveryClassifiesUnknownAndRetriesOnlyKnownFailures(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		err          error
		receipt      int64
	}{
		{"success", "sent", nil, 321},
		{"permanent", "failed", &distribution.SendError{Reason: "bad recipient", Permanent: true}, 0},
		{"unknown", "uncertain", errors.New("connection lost"), 0},
		{"missing receipt", "uncertain", nil, 0},
		{"transient", "pending", &distribution.SendError{Reason: "rate limited", RetryAfter: time.Minute}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, filepath.Join(t.TempDir(), "delivery.db"), time.Minute)
			event := openUserEvent(t, s, 1, 1, "hello")
			finishEvent(t, s, event, TurnOutput{Text: "reply"}, []string{"reply"})
			r := testRuntime(s)
			attempts := 0
			r.Send = func(context.Context, string) (int64, error) { attempts++; return tc.receipt, tc.err }
			if worked, err := r.deliver(ctx); err != nil || !worked {
				t.Fatalf("send: %v, %v", worked, err)
			}
			var status, next string
			if err := s.db.QueryRow(`SELECT status,next_attempt_at FROM personal_outbox`).Scan(&status, &next); err != nil || status != tc.status {
				t.Fatalf("state %s, %v", status, err)
			}
			if tc.status == "pending" {
				at, err := parseTimestamp(next)
				if err != nil || time.Until(at) < 50*time.Second {
					t.Fatalf("retry-after ignored: %s, %v", next, err)
				}
				for i := 0; i < 2; i++ {
					if _, err := s.db.Exec(`UPDATE personal_outbox SET next_attempt_at=?`, timestamp(time.Now().Add(-time.Minute))); err != nil {
						t.Fatal(err)
					}
					if _, err := r.deliver(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.db.QueryRow(`SELECT status FROM personal_outbox`).Scan(&status); err != nil || status != "failed" || attempts != 3 {
					t.Fatalf("retry cap: %s attempts=%d, %v", status, attempts, err)
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
