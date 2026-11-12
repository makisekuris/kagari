package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestRuntimeReportsSilentMemoryFailureWhenUserQueriesRun(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, ":memory:", time.Minute)
	source := testkit.OpenUserEvent(t, s, 1, 1, "I prefer concise answers.")
	testkit.FinishEvent(t, s, source, personal.TurnOutput{Silent: true}, nil)
	run, err := s.ClaimRun(ctx, personal.RunMemoryReview)
	if err != nil || run == nil {
		t.Fatalf("memory run: %+v, %v", run, err)
	}
	if err := s.FinishMemoryReview(ctx, run.ID, personal.MemoryReview{}, "memory review unavailable"); err != nil {
		t.Fatal(err)
	}
	testkit.AcceptInput(t, s, "2", testkit.SourceInput("2", "Show the memory run result."))
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("get_run", map[string]int64{"run_id": run.ID}, "get-memory-run"),
		testkit.ResponseText(`{"text":"记忆整理失败，尚未更新。","reply_kind":"silent","attachments":[]}`),
	}}
	main := testMainAgent(t, fixture, s)
	runtime := testRuntime(s)
	runtime.Think = main.Think
	if _, err := runtime.turn(ctx); err != nil {
		t.Fatal(err)
	}
	message, err := s.ClaimDelivery(ctx)
	if err != nil || message == nil || message.Text != "记忆整理失败，尚未更新。" {
		t.Fatalf("memory query must reply: %+v, %v", message, err)
	}
	if len(fixture.Requests) != 2 || !strings.Contains(testkit.RequestInput(fixture.Requests[1]), "memory review unavailable") {
		t.Fatal("stored memory failure was not returned by get_run")
	}
}
