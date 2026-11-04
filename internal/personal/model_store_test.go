package personal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMainAgentForgetToolCommitsToRealStore(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "forget.db"), time.Minute)
	first := openUserEvent(t, s, 1, 1, "我住在杭州")
	if _, err := s.WriteMemory(ctx, first.ID, "seed", MemoryChange{Operation: "put", Key: "home_city", Value: "杭州", Evidence: first.Text}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, first, TurnOutput{}, nil)
	event := openUserEvent(t, s, 2, 2, "忘记我住在哪个城市")
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("memory_forget", memoryForgetArgs{Key: "home_city", ExpectedVersion: 1, Evidence: event.Text}, "forget"),
		responseText(`{"text":"已忘记。","notify":true,"attachments":[]}`),
	}}
	main := MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 4}
	output, err := main.Think(ctx, TurnInput{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := s.Memory(ctx, "home_city")
	if err != nil || memory == nil || !memory.Deleted || memory.Value != "" || memory.Version != 2 {
		t.Fatalf("forget tool did not commit before returning: %+v, %v", memory, err)
	}
	toolResult := requestInput(fixture.requests[1])
	if !strings.Contains(toolResult, `\"deleted\":true`) || strings.Contains(toolResult, "杭州") || strings.Contains(toolResult, `\"queued\":true`) || strings.Contains(toolResult, `\"run\":`) {
		t.Fatalf("forget tool did not return a value-free committed tombstone: %s", toolResult)
	}
	finishEvent(t, s, event, output, nil)
	var explicitRuns int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE event_id=? AND kind='memory'`, event.ID).Scan(&explicitRuns); err != nil || explicitRuns != 0 {
		t.Fatalf("explicit forget created a background mutation run: count=%d err=%v", explicitRuns, err)
	}
	if callback, err := s.ClaimEvent(ctx); err != nil || callback != nil {
		t.Fatalf("successful forget emitted failure: %+v, %v", callback, err)
	}
}

func TestMainAgentMemoryToolReplayAndConsecutiveWrites(t *testing.T) {
	t.Run("put replay", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "put-replay.db"), time.Minute)
		event := openUserEvent(t, s, 1, 1, "请记住我喜欢简洁回答。")
		args := memoryPutArgs{Key: "answer_style", Value: "concise", Evidence: event.Text}
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("memory_put", args, "put-once"),
			responseTool("memory_put", args, "put-replay"),
			responseText(`{"text":"已保存。","notify":true,"attachments":[]}`),
		}}
		main := MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 4}
		if _, err := main.Think(ctx, TurnInput{Event: event}); err != nil {
			t.Fatal(err)
		}
		memory, err := s.Memory(ctx, args.Key)
		if err != nil || memory == nil || memory.Version != 1 || memory.Value != args.Value {
			t.Fatalf("put replay was rejected or applied twice: %+v, %v", memory, err)
		}
		if !strings.Contains(requestInput(fixture.requests[2]), `\"version\":1`) {
			t.Fatal("replayed put did not return its original committed memory result")
		}
	})

	t.Run("forget replay", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "forget-replay.db"), time.Minute)
		seed := openUserEvent(t, s, 1, 1, "我住在杭州")
		if _, err := s.WriteMemory(ctx, seed.ID, "seed", MemoryChange{Operation: "put", Key: "home_city", Value: "杭州", Evidence: seed.Text}); err != nil {
			t.Fatal(err)
		}
		finishEvent(t, s, seed, TurnOutput{}, nil)
		event := openUserEvent(t, s, 2, 2, "请忘记我住在哪个城市")
		args := memoryForgetArgs{Key: "home_city", ExpectedVersion: 1, Evidence: event.Text}
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("memory_forget", args, "forget-once"),
			responseTool("memory_forget", args, "forget-replay"),
			responseText(`{"text":"已忘记。","notify":true,"attachments":[]}`),
		}}
		main := MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 4}
		if _, err := main.Think(ctx, TurnInput{Event: event}); err != nil {
			t.Fatal(err)
		}
		memory, err := s.Memory(ctx, args.Key)
		if err != nil || memory == nil || !memory.Deleted || memory.Value != "" || memory.Version != 2 {
			t.Fatalf("forget replay was rejected or applied twice: %+v, %v", memory, err)
		}
		if !strings.Contains(requestInput(fixture.requests[2]), `\"deleted\":true`) || strings.Contains(requestInput(fixture.requests[2]), "杭州") {
			t.Fatal("replayed forget did not return its original value-free tombstone")
		}
	})

	t.Run("same event uses latest version", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t, filepath.Join(t.TempDir(), "consecutive.db"), time.Minute)
		evidence1, evidence2 := "请记住我喜欢简洁回答。", "更正为详细回答。"
		event := openUserEvent(t, s, 1, 1, evidence1+evidence2)
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("memory_put", memoryPutArgs{Key: "answer_style", Value: "concise", Evidence: evidence1}, "put-v1"),
			responseTool("memory_put", memoryPutArgs{Key: "answer_style", Value: "detailed", ExpectedVersion: 1, Evidence: evidence2}, "put-v2"),
			responseText(`{"text":"已更正并保存。","notify":true,"attachments":[]}`),
		}}
		main := MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 4}
		if _, err := main.Think(ctx, TurnInput{Event: event}); err != nil {
			t.Fatal(err)
		}
		memory, err := s.Memory(ctx, "answer_style")
		if err != nil || memory == nil || memory.Version != 2 || memory.Value != "detailed" || memory.SourceEventID != event.ID {
			t.Fatalf("same-event latest-version write failed: %+v, %v", memory, err)
		}
	})
}

func TestMainAgentMemoryCommitSurvivesReplyFailure(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "reply-error.db"), time.Minute)
	event := openUserEvent(t, s, 1, 1, "请记住我喜欢简洁回答。")
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("memory_put", memoryPutArgs{Key: "answer_style", Value: "concise", Evidence: event.Text}, "committed-put"),
		responseText("not a personal response"),
		responseText("still not a personal response"),
	}}
	main := MainAgent{Store: s, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 4}
	if _, err := main.Think(ctx, TurnInput{Event: event}); err == nil {
		t.Fatal("expected a failed reply format")
	}
	memory, err := s.Memory(ctx, "answer_style")
	if err != nil || memory == nil || memory.Version != 1 || memory.Value != "concise" {
		t.Fatalf("reply failure rolled back a committed memory change: %+v, %v", memory, err)
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
		responseText(`{"text":"记忆整理失败，尚未更新。","notify":false,"attachments":[]}`),
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
