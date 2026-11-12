package testkit

import (
	"context"
	"strconv"
	"testing"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/sqlite"
)

func Store(t *testing.T, path string, budget time.Duration) *sqlite.Store {
	t.Helper()
	s := EmptyStore(t, path, budget)
	if err := s.BindChannel(context.Background(), "telegram", "test-bot", "test-owner", false); err != nil {
		t.Fatal(err)
	}
	return s
}

func EmptyStore(t *testing.T, path string, budget time.Duration) *sqlite.Store {
	t.Helper()
	s, err := sqlite.NewStore(path, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func SourceInput(messageID, text string) personal.Input {
	return personal.Input{Channel: "telegram", Account: "test-bot", Sender: "test-owner", MessageID: messageID, Text: text}
}

func AcceptInput(t *testing.T, s *sqlite.Store, cursor string, input personal.Input) int64 {
	t.Helper()
	ids, err := s.AcceptBatch(context.Background(), input.Channel, input.Account, cursor, []personal.Input{input})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("AcceptBatch returned %d IDs", len(ids))
	}
	return ids[0]
}

func OpenUserEvent(t *testing.T, s *sqlite.Store, updateID, messageID int64, text string) personal.Event {
	t.Helper()
	id := AcceptInput(t, s, strconv.FormatInt(updateID, 10), SourceInput(strconv.FormatInt(messageID, 10), text))
	event, err := s.ClaimEvent(context.Background())
	if err != nil || event == nil || event.ID != id {
		t.Fatalf("ClaimEvent() = (%+v, %v), want event %d", event, err, id)
	}
	return *event
}

func CreateTask(t *testing.T, s *sqlite.Store, event personal.Event, key, title, goal, contextText string) (personal.Task, personal.Run) {
	t.Helper()
	task, run, err := s.CreateTask(context.Background(), event.ID, key, title, personal.TaskInput{Goal: goal, Context: contextText})
	if err != nil {
		t.Fatal(err)
	}
	return task, run
}

func FinishEvent(t *testing.T, s *sqlite.Store, event personal.Event, output personal.TurnOutput, chunks []string) {
	t.Helper()
	if err := s.CompleteEvent(context.Background(), event.ID, output, chunks); err != nil {
		t.Fatal(err)
	}
}

func AssistantMessage(t *testing.T, s *sqlite.Store, eventID int64) personal.Message {
	t.Helper()
	history, err := s.History(context.Background(), eventID+1, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found []personal.Message
	for _, message := range history {
		if message.EventID == eventID && message.Role == "assistant" {
			found = append(found, message)
		}
	}
	if len(found) != 1 {
		t.Fatalf("event %d has %d assistant messages, want one", eventID, len(found))
	}
	return found[0]
}

func CreateRunningTask(t *testing.T, s *sqlite.Store, key string, input personal.TaskInput) personal.Run {
	t.Helper()
	ctx := context.Background()
	event := OpenUserEvent(t, s, 1, 1, "Run task stage")
	if _, _, err := s.CreateTask(ctx, event.ID, key, key, input); err != nil {
		t.Fatal(err)
	}
	FinishEvent(t, s, event, personal.TurnOutput{}, nil)
	run, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatalf("ClaimRun() = (%+v, %v)", run, err)
	}
	return *run
}
