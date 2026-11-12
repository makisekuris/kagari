package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
	"kagari/internal/logging"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestLivePersonalReadingStage(t *testing.T) {
	engine := testkit.LiveEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := testkit.Store(t, filepath.Join(t.TempDir(), "reading.db"), 10*time.Minute)
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	code := hex.EncodeToString(random)
	const pageURL = "https://example.org/personal-eval/evidence"
	reads := 0
	engine.Read = func(_ context.Context, rawURL string) (domain.Source, error) {
		reads++
		if rawURL != pageURL {
			return domain.Source{URL: rawURL, Status: "failed", Reason: "unknown fixture URL"}, nil
		}
		return domain.Source{ID: "eval_evidence", URL: rawURL, RequestedURL: rawURL, Title: "虚构证据页", Content: "本次虚构实验唯一校验码是：" + code, Status: "ok", ReadMethod: "fixture", FetchedAt: time.Now().UTC()}, nil
	}
	engine.Config.Agent.MaxIterations = 5
	seed := testkit.OpenUserEvent(t, store, 1, 1, "读取 "+pageURL+"，告诉我页面中的本次实验唯一校验码，并引用证据页；不能猜测。")
	task, _ := testkit.CreateTask(t, store, seed, "seed-reading", "虚构页面读取", seed.Text, "仅使用本测试的受控页面。")
	testkit.FinishEvent(t, store, seed, personal.TurnOutput{TaskID: task.ID}, nil)
	run, err := store.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatal("cannot claim fictional reading task")
	}
	started := time.Now()
	result, executeErr := runTaskExecutor(t, ctx, engine, store, *run, nil)
	if err := store.FinishStage(ctx, run.ID, result, logging.ErrorReason(executeErr)); err != nil {
		t.Fatal(err)
	}
	t.Logf("reading stage duration=%s reads=%d sources=%d error=%s", time.Since(started).Round(time.Millisecond), reads, len(result.Sources), logging.ErrorReason(executeErr))
	if reads == 0 {
		t.Fatal("mechanism FAIL: TaskExecutor did not invoke read_url against the controlled page")
	}
	persisted, err := store.Run(ctx, run.ID)
	if executeErr != nil || err != nil || persisted == nil || persisted.Status != personal.RunCompleted || !strings.Contains(result.Summary, code) || len(result.Sources) != 1 || !strings.Contains(persisted.Result, code) {
		t.Fatal("semantic FAIL: reading stage did not return and persist the random evidence value")
	}
	t.Log("mechanism PASS: TaskExecutor read_url → controlled page → persisted real Store stage; semantic PASS: answer contains random source evidence")
}
