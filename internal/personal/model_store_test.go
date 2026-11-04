package personal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMainAgentForgetToolAppliesToRealStore(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "forget.db"), time.Minute)
	first := openUserEvent(t, s, 1, 1, "我住在杭州")
	if _, err := s.QueueMemory(ctx, first.ID, "seed", MemoryChange{Operation: "put", Key: "home_city", Value: "杭州", Evidence: first.Text}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, first, TurnOutput{}, nil)
	r := testRuntime(s)
	for i := 0; i < 2; i++ {
		if _, err := r.memory(ctx); err != nil {
			t.Fatal(err)
		}
	}
	event := openUserEvent(t, s, 2, 2, "忘记我住在哪个城市")
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("memory_forget", memoryForgetArgs{Key: "home_city", ExpectedVersion: 1, Evidence: event.Text}, "forget"),
		responseText(`{"text":"遗忘操作已排队。","notify":true}`),
	}}
	main := MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 4}
	output, err := main.Think(ctx, TurnInput{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, output, nil)
	for i := 0; i < 2; i++ {
		if _, err := r.memory(ctx); err != nil {
			t.Fatal(err)
		}
	}
	memory, err := s.Memory(ctx, "home_city")
	if err != nil || memory == nil || !memory.Deleted || memory.Value != "" {
		t.Fatalf("tool did not forget: %+v, %v", memory, err)
	}
	if callback, err := s.ClaimEvent(ctx); err != nil || callback != nil {
		t.Fatalf("successful forget emitted failure: %+v, %v", callback, err)
	}
}

func TestMainAgentReportsSilentMemoryFailureWhenUserQueriesRun(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	source := openUserEvent(t, s, 1, 1, "I prefer concise answers.")
	finishEvent(t, s, source, TurnOutput{}, nil)
	run, err := s.ClaimRun(ctx, RunMemoryReview)
	if err != nil || run == nil {
		t.Fatalf("memory run: %+v, %v", run, err)
	}
	if err := s.FinishMemoryReview(ctx, run.ID, MemoryReview{}, "memory review unavailable"); err != nil {
		t.Fatal(err)
	}
	event := openUserEvent(t, s, 2, 2, "Show the memory run result.")
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("get_run", runArgs{RunID: run.ID}, "get-memory-run"),
		responseText(`{"text":"记忆整理失败，尚未更新。","notify":false}`),
	}}
	main := MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 3}
	output, err := main.Think(ctx, TurnInput{Event: event})
	if err != nil || output.Silent || output.Text == "" {
		t.Fatalf("memory query must reply: %+v, %v", output, err)
	}
	if len(fixture.requests) != 2 || !strings.Contains(requestInput(fixture.requests[1]), "memory review unavailable") {
		t.Fatal("stored memory failure was not returned by get_run")
	}
}
