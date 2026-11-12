package assistant

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestMainAgentForgetToolCommitsToRealStore(t *testing.T) {
	ctx := context.Background()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "forget.db"), time.Minute)
	first := testkit.OpenUserEvent(t, s, 1, 1, "我住在杭州")
	if _, err := s.WriteMemory(ctx, first.ID, "seed", personal.MemoryChange{Operation: "put", Key: "home_city", Value: "杭州", Evidence: first.Text}); err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, s, first, personal.TurnOutput{}, nil)
	event := testkit.OpenUserEvent(t, s, 2, 2, "忘记我住在哪个城市")
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("memory_forget", memoryForgetArgs{Key: "home_city", ExpectedVersion: 1, Evidence: event.Text}, "forget"),
		testkit.ResponseText(`{"text":"已忘记。","reply_kind":"content","attachments":[]}`),
	}}
	main := MainAgent{Store: s, Model: fixture.Model(t), Timeout: time.Second, MaxIterations: 4}
	output, err := main.Think(ctx, personal.TurnInput{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := s.Memory(ctx, "home_city")
	if err != nil || memory == nil || !memory.Deleted || memory.Value != "" || memory.Version != 2 {
		t.Fatalf("forget tool did not commit before returning: %+v, %v", memory, err)
	}
	toolResult := testkit.RequestInput(fixture.Requests[1])
	if !strings.Contains(toolResult, `\"deleted\":true`) || strings.Contains(toolResult, "杭州") || strings.Contains(toolResult, `\"queued\":true`) || strings.Contains(toolResult, `\"run\":`) {
		t.Fatalf("forget tool did not return a value-free committed tombstone: %s", toolResult)
	}
	testkit.FinishEvent(t, s, event, output, nil)
	diagnostic, err := s.Diagnostic(ctx, event.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range diagnostic.Runs {
		if run.Kind == personal.RunKind("memory") {
			t.Fatalf("explicit forget created a background mutation run: %+v", run)
		}
	}
	if callback, err := s.ClaimEvent(ctx); err != nil || callback != nil {
		t.Fatalf("successful forget emitted failure: %+v, %v", callback, err)
	}
}

func TestMainAgentMemoryToolReplayAndConsecutiveWrites(t *testing.T) {
	t.Run("put replay", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "put-replay.db"), time.Minute)
		event := testkit.OpenUserEvent(t, s, 1, 1, "请记住我喜欢简洁回答。")
		args := memoryPutArgs{Key: "answer_style", Value: "concise", Evidence: event.Text}
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("memory_put", args, "put-once"),
			testkit.ResponseTool("memory_put", args, "put-replay"),
			testkit.ResponseText(`{"text":"已保存。","reply_kind":"content","attachments":[]}`),
		}}
		main := MainAgent{Store: s, Model: fixture.Model(t), Timeout: time.Second, MaxIterations: 4}
		if _, err := main.Think(ctx, personal.TurnInput{Event: event}); err != nil {
			t.Fatal(err)
		}
		memory, err := s.Memory(ctx, args.Key)
		if err != nil || memory == nil || memory.Version != 1 || memory.Value != args.Value {
			t.Fatalf("put replay was rejected or applied twice: %+v, %v", memory, err)
		}
		if !strings.Contains(testkit.RequestInput(fixture.Requests[2]), `\"version\":1`) {
			t.Fatal("replayed put did not return its original committed memory result")
		}
	})

	t.Run("forget replay", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "forget-replay.db"), time.Minute)
		seed := testkit.OpenUserEvent(t, s, 1, 1, "我住在杭州")
		if _, err := s.WriteMemory(ctx, seed.ID, "seed", personal.MemoryChange{Operation: "put", Key: "home_city", Value: "杭州", Evidence: seed.Text}); err != nil {
			t.Fatal(err)
		}
		testkit.FinishEvent(t, s, seed, personal.TurnOutput{}, nil)
		event := testkit.OpenUserEvent(t, s, 2, 2, "请忘记我住在哪个城市")
		args := memoryForgetArgs{Key: "home_city", ExpectedVersion: 1, Evidence: event.Text}
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("memory_forget", args, "forget-once"),
			testkit.ResponseTool("memory_forget", args, "forget-replay"),
			testkit.ResponseText(`{"text":"已忘记。","reply_kind":"content","attachments":[]}`),
		}}
		main := MainAgent{Store: s, Model: fixture.Model(t), Timeout: time.Second, MaxIterations: 4}
		if _, err := main.Think(ctx, personal.TurnInput{Event: event}); err != nil {
			t.Fatal(err)
		}
		memory, err := s.Memory(ctx, args.Key)
		if err != nil || memory == nil || !memory.Deleted || memory.Value != "" || memory.Version != 2 {
			t.Fatalf("forget replay was rejected or applied twice: %+v, %v", memory, err)
		}
		if !strings.Contains(testkit.RequestInput(fixture.Requests[2]), `\"deleted\":true`) || strings.Contains(testkit.RequestInput(fixture.Requests[2]), "杭州") {
			t.Fatal("replayed forget did not return its original value-free tombstone")
		}
	})

	t.Run("same event uses latest version", func(t *testing.T) {
		ctx := context.Background()
		s := testkit.Store(t, filepath.Join(t.TempDir(), "consecutive.db"), time.Minute)
		evidence1, evidence2 := "请记住我喜欢简洁回答。", "更正为详细回答。"
		event := testkit.OpenUserEvent(t, s, 1, 1, evidence1+evidence2)
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("memory_put", memoryPutArgs{Key: "answer_style", Value: "concise", Evidence: evidence1}, "put-v1"),
			testkit.ResponseTool("memory_put", memoryPutArgs{Key: "answer_style", Value: "detailed", ExpectedVersion: 1, Evidence: evidence2}, "put-v2"),
			testkit.ResponseText(`{"text":"已更正并保存。","reply_kind":"content","attachments":[]}`),
		}}
		main := MainAgent{Store: s, Model: fixture.Model(t), Timeout: time.Second, MaxIterations: 4}
		if _, err := main.Think(ctx, personal.TurnInput{Event: event}); err != nil {
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
	s := testkit.Store(t, filepath.Join(t.TempDir(), "reply-error.db"), time.Minute)
	event := testkit.OpenUserEvent(t, s, 1, 1, "请记住我喜欢简洁回答。")
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("memory_put", memoryPutArgs{Key: "answer_style", Value: "concise", Evidence: event.Text}, "committed-put"),
		testkit.ResponseText("not a personal response"),
		testkit.ResponseText("still not a personal response"),
	}}
	main := MainAgent{Store: s, Model: fixture.Model(t), Timeout: time.Second, MaxIterations: 4}
	if _, err := main.Think(ctx, personal.TurnInput{Event: event}); err == nil {
		t.Fatal("expected a failed reply format")
	}
	memory, err := s.Memory(ctx, "answer_style")
	if err != nil || memory == nil || memory.Version != 1 || memory.Value != "concise" {
		t.Fatalf("reply failure rolled back a committed memory change: %+v, %v", memory, err)
	}
}
