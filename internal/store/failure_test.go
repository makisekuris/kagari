package store

import (
	"context"
	"testing"
	"time"

	"kagari/internal/domain"
)

func TestFailureEvidenceAndFinalNoticeAreAtomic(t *testing.T) {
	s := testStore(t, ":memory:")
	ctx := context.Background()
	id, _, err := s.Enqueue(ctx, "analyze", "failure", []byte(`{"user_id":7,"chat_id":7}`), 900)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	notice := &domain.Command{UserID: 7, ChatID: 7, Text: "最终失败"}
	first := []byte(`{"attempt":1}`)
	retryAt := time.Now().Add(time.Hour)
	if err := s.FailJob(ctx, id, first, "temporary", 2, retryAt, notice); err != nil {
		t.Fatal(err)
	}
	job, err := s.Job(ctx, id)
	if err != nil || job.Status != "pending" || !job.NextAttemptAt.Equal(retryAt) || string(job.Result) != string(first) {
		t.Fatalf("retry state or evidence lost: %+v, %v", job, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs WHERE kind='notice'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("intermediate failure notified: %d, %v", count, err)
	}
	if _, err := s.StartJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER stop_notice BEFORE INSERT ON jobs WHEN NEW.kind='notice' BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	final := []byte(`{"attempt":2}`)
	if err := s.FailJob(ctx, id, final, "exhausted", 2, retryAt, notice); err == nil {
		t.Fatal("notice persistence failure was ignored")
	}
	job, err = s.Job(ctx, id)
	if err != nil || job.Status != "processing" || string(job.Result) != string(first) {
		t.Fatalf("failure transaction was partially committed: %+v, %v", job, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER stop_notice`); err != nil {
		t.Fatal(err)
	}
	if err := s.FailJob(ctx, id, final, "exhausted", 2, retryAt, notice); err != nil {
		t.Fatal(err)
	}
	job, err = s.Job(ctx, id)
	if err != nil || job.Status != "failed" || string(job.Result) != string(final) {
		t.Fatalf("terminal state or evidence lost: %+v, %v", job, err)
	}
	if err := s.FailJob(ctx, id, final, "exhausted", 2, retryAt, notice); err == nil {
		t.Fatal("completed failure transition accepted twice")
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs WHERE kind='notice' AND target_chat_id=7`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one private terminal notice: %d, %v", count, err)
	}
	if err := s.RetryJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.FailJob(ctx, id, first, "exhausted", 1, retryAt, notice); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM jobs WHERE kind='notice'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("manual retry round did not get its own final notice: %d, %v", count, err)
	}
}
