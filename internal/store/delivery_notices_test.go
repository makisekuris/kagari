package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
)

func createSendingDelivery(t *testing.T, s *Store, kind, key string, payload []byte, target domain.DeliveryTarget, messages ...domain.Delivery) (int64, *domain.Delivery) {
	t.Helper()
	ctx := context.Background()
	jobID, _, err := s.Enqueue(ctx, kind, key, payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJob(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	for i := range messages {
		messages[i].Target = target
		messages[i].Part = i + 1
	}
	if len(messages) == 0 {
		messages = []domain.Delivery{{Target: target, Part: 1, Text: "PRIVATE_ORIGINAL_MESSAGE"}}
	}
	if err := s.CompletePublication(ctx, jobID, []byte(`{"saved":"PRIVATE_RESULT"}`), messages, nil); err != nil {
		t.Fatal(err)
	}
	delivery, err := s.ClaimDelivery(ctx)
	if err != nil || delivery == nil || delivery.JobID != jobID {
		t.Fatalf("ClaimDelivery() = (%+v, %v), want job %d", delivery, err, jobID)
	}
	return jobID, delivery
}

func deliveryNoticeCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs WHERE kind='notice' AND key LIKE 'delivery-notice:%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestFailDeliveryNotifiesOnlyTerminalAnalysisAndDigestPrivately(t *testing.T) {
	tests := []struct {
		name        string
		kind        string
		payload     any
		maxAttempts int
		wantNotice  bool
		wantChatID  int64
		wantStatus  string
	}{
		{name: "analyze", kind: "analyze", payload: domain.Submission{UserID: 7, ChatID: 7}, maxAttempts: 1, wantNotice: true, wantChatID: 7, wantStatus: "failed"},
		{name: "digest owner fallback", kind: "digest", payload: domain.DigestRequest{UserID: 9}, maxAttempts: 1, wantNotice: true, wantChatID: 9, wantStatus: "failed"},
		{name: "command does not recurse", kind: "command", payload: domain.Command{UserID: 7, ChatID: 7}, maxAttempts: 1, wantStatus: "failed"},
		{name: "notice does not recurse", kind: "notice", payload: domain.Command{UserID: 7, ChatID: 7}, maxAttempts: 1, wantStatus: "failed"},
		{name: "missing owner", kind: "analyze", payload: domain.Submission{ChatID: 7}, maxAttempts: 1, wantStatus: "failed"},
		{name: "invalid private chat", kind: "analyze", payload: domain.Submission{UserID: 7, ChatID: -100}, maxAttempts: 1, wantStatus: "failed"},
		{name: "ordinary retry", kind: "analyze", payload: domain.Submission{UserID: 7, ChatID: 7}, maxAttempts: 2, wantStatus: "pending"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t, ":memory:")
			payload, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			jobID, delivery := createSendingDelivery(t, s, tc.kind, "delivery-notice-"+tc.name, payload, domain.DeliveryTarget{Channel: "telegram", Address: "-100123"})
			reason := "fixture_upstream_reason"
			if err := s.FailDelivery(context.Background(), delivery.ID, reason, tc.maxAttempts, time.Now(), false); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := s.db.QueryRow(`SELECT status FROM deliveries WHERE id=?`, delivery.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if tc.wantNotice {
				if state != "failed" || deliveryNoticeCount(t, s) != 1 {
					t.Fatalf("terminal delivery = %q with %d notices", state, deliveryNoticeCount(t, s))
				}
				notice, err := s.ClaimJob(context.Background())
				if err != nil || notice == nil || notice.Kind != "notice" || len(notice.Targets) != 1 || notice.Targets[0] != telegramTargets(tc.wantChatID)[0] {
					t.Fatalf("private notice = (%+v, %v)", notice, err)
				}
				var command domain.Command
				if err := json.Unmarshal(notice.Payload, &command); err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"#" + strconv.FormatInt(jobID, 10), "telegram/-100123", "投递失败", "原任务结果已保留", "/status " + strconv.FormatInt(jobID, 10), "/retry_delivery " + strconv.FormatInt(jobID, 10)} {
					if !strings.Contains(command.Text, want) {
						t.Errorf("notice omitted %q: %q", want, command.Text)
					}
				}
				if strings.Contains(command.Text, reason) || strings.Contains(command.Text, "PRIVATE_ORIGINAL_MESSAGE") || strings.Contains(command.Text, "PRIVATE_RESULT") {
					t.Fatalf("notice exposed failure details or message body: %q", command.Text)
				}
			} else if state != tc.wantStatus || deliveryNoticeCount(t, s) != 0 {
				t.Fatalf("nonterminal or ineligible delivery = %q with %d notices", state, deliveryNoticeCount(t, s))
			}
		})
	}
}

func TestFailDeliveryNotifiesAfterAttemptExhaustion(t *testing.T) {
	s := testStore(t, ":memory:")
	payload := mustJSON(t, domain.Submission{UserID: 7, ChatID: 7})
	jobID, delivery := createSendingDelivery(t, s, "analyze", "delivery-exhausted", payload, domain.DeliveryTarget{Channel: "telegram", Address: "-100123"})
	if err := s.FailDelivery(context.Background(), delivery.ID, "temporary", 2, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if deliveryNoticeCount(t, s) != 0 {
		t.Fatal("retryable delivery generated a notice")
	}
	delivery, err := s.ClaimDelivery(context.Background())
	if err != nil || delivery == nil || delivery.JobID != jobID || delivery.Attempts != 2 {
		t.Fatalf("second delivery attempt = (%+v, %v)", delivery, err)
	}
	if err := s.FailDelivery(context.Background(), delivery.ID, "fixture_upstream_reason", 2, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if deliveryNoticeCount(t, s) != 1 {
		t.Fatal("exhausted delivery did not generate one notice")
	}
}

func TestUncertainDeliveryNoticeAndManualRetryAreIdempotent(t *testing.T) {
	s := testStore(t, ":memory:")
	payload := mustJSON(t, domain.Submission{UserID: 7, ChatID: 7})
	jobID, delivery := createSendingDelivery(t, s, "analyze", "delivery-uncertain", payload, domain.DeliveryTarget{Channel: "telegram", Address: "-100123"})
	if err := s.FailDelivery(context.Background(), delivery.ID, "fixture_upstream_reason", 1, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	notice, err := s.ClaimJob(context.Background())
	if err != nil || notice == nil || notice.Kind != "notice" || notice.Targets[0] != telegramTargets(7)[0] {
		t.Fatalf("uncertain notice = (%+v, %v)", notice, err)
	}
	var command domain.Command
	if err := json.Unmarshal(notice.Payload, &command); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"投递结果不确定", "远端可能已发送", "手动重发可能导致重复", "/status " + strconv.FormatInt(jobID, 10), "/retry_delivery " + strconv.FormatInt(jobID, 10)} {
		if !strings.Contains(command.Text, want) {
			t.Errorf("uncertain notice omitted %q: %q", want, command.Text)
		}
	}
	if strings.Contains(command.Text, "fixture_upstream_reason") || strings.Contains(command.Text, "PRIVATE_ORIGINAL_MESSAGE") {
		t.Fatalf("uncertain notice exposed failure details: %q", command.Text)
	}
	if err := s.RetryDeliveries(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	retried, err := s.ClaimDelivery(context.Background())
	if err != nil || retried == nil || retried.ID != delivery.ID {
		t.Fatalf("manual retry = (%+v, %v)", retried, err)
	}
	if err := s.FailDelivery(context.Background(), retried.ID, "fixture_upstream_reason", 1, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if got := deliveryNoticeCount(t, s); got != 1 {
		t.Fatalf("manual retry created %d notices, want one", got)
	}
	var noticeKey string
	if err := s.db.QueryRow(`SELECT key FROM jobs WHERE kind='notice'`).Scan(&noticeKey); err != nil || noticeKey != "delivery-notice:"+strconv.FormatInt(jobID, 10)+":"+strconv.FormatInt(delivery.ID, 10) {
		t.Fatalf("notice key = %q, %v", noticeKey, err)
	}
}

func TestRecoverNotifiesUncertainAndExistingFailedDeliveriesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deliveries.sqlite")
	s := testStore(t, path)
	payload := mustJSON(t, domain.Submission{UserID: 7, ChatID: 7})
	target := domain.DeliveryTarget{Channel: "telegram", Address: "-100123"}
	jobID, sending := createSendingDelivery(t, s, "analyze", "delivery-recover", payload, target,
		domain.Delivery{Text: "first"}, domain.Delivery{Text: "second"})
	if _, err := s.db.Exec(`UPDATE deliveries SET status='failed' WHERE job_id=? AND part=2`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, path)
	if err := s.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM deliveries WHERE id=?`, sending.ID).Scan(&status); err != nil || status != "uncertain" {
		t.Fatalf("sending delivery recovery = %q, %v", status, err)
	}
	if got := deliveryNoticeCount(t, s); got != 2 {
		t.Fatalf("Recover() generated %d notices, want two terminal deliveries exactly once", got)
	}
	if next, err := s.ClaimDelivery(context.Background()); err != nil || next != nil {
		t.Fatalf("recovered delivery was automatically retried: %+v, %v", next, err)
	}
	for i := 0; i < 2; i++ {
		notice, err := s.ClaimJob(context.Background())
		if err != nil || notice == nil || notice.Kind != "notice" || notice.Targets[0] != telegramTargets(7)[0] {
			t.Fatalf("recovery notice %d = (%+v, %v)", i, notice, err)
		}
		var command domain.Command
		if err := json.Unmarshal(notice.Payload, &command); err != nil || !strings.Contains(command.Text, "/retry_delivery "+strconv.FormatInt(jobID, 10)) {
			t.Fatalf("recovery notice payload = %+v, %v", command, err)
		}
	}
}

func TestDeliveryNoticeFailureRollsBackState(t *testing.T) {
	s := testStore(t, ":memory:")
	payload := mustJSON(t, domain.Submission{UserID: 7, ChatID: 7})
	_, delivery := createSendingDelivery(t, s, "analyze", "delivery-notice-rollback", payload, domain.DeliveryTarget{Channel: "telegram", Address: "-100123"})
	if _, err := s.db.Exec(`CREATE TRIGGER stop_delivery_notice BEFORE INSERT ON jobs WHEN NEW.kind='notice' BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.FailDelivery(context.Background(), delivery.ID, "fixture_upstream_reason", 1, time.Now(), false); err == nil {
		t.Fatal("notice persistence failure was ignored")
	}
	var state string
	if err := s.db.QueryRow(`SELECT status FROM deliveries WHERE id=?`, delivery.ID).Scan(&state); err != nil || state != "sending" {
		t.Fatalf("delivery failure transition partially committed: %q, %v", state, err)
	}
}

func TestDeliveryNoticeKeySurvivesArchiveDeliveryIDReuse(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	target := domain.DeliveryTarget{Channel: "telegram", Address: "-100123"}
	firstPayload := mustJSON(t, domain.Submission{UserID: 7, ChatID: 7, CacheKey: "first"})
	firstJobID, firstDelivery := createSendingDelivery(t, s, "analyze", "reuse-first", firstPayload, target)
	if err := s.FailDelivery(ctx, firstDelivery.ID, "temporary", 1, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteArchiveEntry(ctx, 7, firstJobID); err != nil {
		t.Fatal(err)
	}
	secondPayload := mustJSON(t, domain.Submission{UserID: 7, ChatID: 7, CacheKey: "second"})
	secondJobID, secondDelivery := createSendingDelivery(t, s, "analyze", "reuse-second", secondPayload, target)
	if secondDelivery.ID != firstDelivery.ID || secondJobID == firstJobID {
		t.Fatalf("fixture did not reuse delivery ID under a different parent: first=(%d,%d) second=(%d,%d)", firstJobID, firstDelivery.ID, secondJobID, secondDelivery.ID)
	}
	if err := s.FailDelivery(ctx, secondDelivery.ID, "temporary", 1, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if got := deliveryNoticeCount(t, s); got != 2 {
		t.Fatalf("reused delivery ID suppressed new parent notice: got %d notices", got)
	}
}

func TestRecoverNoticeFailureRollsBackUncertainTransition(t *testing.T) {
	s := testStore(t, ":memory:")
	payload := mustJSON(t, domain.Submission{UserID: 7, ChatID: 7})
	_, delivery := createSendingDelivery(t, s, "analyze", "recover-notice-rollback", payload, domain.DeliveryTarget{Channel: "telegram", Address: "-100123"})
	if _, err := s.db.Exec(`CREATE TRIGGER stop_recovery_notice BEFORE INSERT ON jobs WHEN NEW.kind='notice' BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(context.Background()); err == nil {
		t.Fatal("notice persistence failure was ignored")
	}
	var state string
	if err := s.db.QueryRow(`SELECT status FROM deliveries WHERE id=?`, delivery.ID).Scan(&state); err != nil || state != "sending" {
		t.Fatalf("recovery transition partially committed: %q, %v", state, err)
	}
}
