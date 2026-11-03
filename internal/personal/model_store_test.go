package personal

import (
	"context"
	"path/filepath"
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

func TestMemoryFailureCannotBeSilencedByModel(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseText(`{"text":"记忆更新失败，需要确认。","notify":false}`)}}
	main := MainAgent{Store: &toolRepo{}, Model: fixture.model(t), Timeout: time.Second, MaxIterations: 2}
	output, err := main.Think(context.Background(), TurnInput{Event: Event{ID: 1, Kind: EventMemory, Text: "memory failed"}})
	if err != nil || output.Silent {
		t.Fatalf("important memory event suppressed: %+v, %v", output, err)
	}
}
