package telegram

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"kagari/internal/personal"

	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A duplicate update must not look like a second accepted message in a trace.
// Use the real inbox transaction rather than a mock that invents event IDs.
func TestPersonalInputLogsCommittedIdentityAndDuplicate(t *testing.T) {
	s, err := personal.NewStore(filepath.Join(t.TempDir(), "personal.db"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	core, observed := observer.New(zap.InfoLevel)
	c, err := newPersonalClient(personalConfig(), s, zap.New(core), "https://telegram.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	u := &models.Update{ID: 1, Message: &models.Message{ID: 10, Text: "hello", From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate}}}
	for i := 0; i < 2; i++ {
		if err := c.acceptUpdate(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	entries := observed.FilterMessage("telegram.input").All()
	if len(entries) != 2 {
		t.Fatalf("input records: %d", len(entries))
	}
	first, second := entries[0].ContextMap(), entries[1].ContextMap()
	if first["outcome"] != "accepted" || first["event_id"] != int64(1) || first["message_id"] != int64(10) {
		t.Fatalf("accepted identity: %v", first)
	}
	if second["outcome"] != "duplicate" || second["event_id"] != nil || second["payload"] != nil {
		t.Fatalf("duplicate was logged as new input: %v", second)
	}
}

// Retain the 409 code that identifies competing pollers, and emit recovery only
// after a successful response, not merely after a retry timer elapsed.
func TestPollingLogsFailureCodeAndRecovery(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := pollUpdates(ctx, func(context.Context) (int64, error) { return 0, nil },
		func(context.Context, int64) ([]json.RawMessage, error) {
			calls++
			if calls == 1 {
				return nil, &pollFailure{code: 409, httpStatus: 409, retryAfter: time.Millisecond}
			}
			cancel()
			return nil, nil
		}, nil, nil, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	failures := observed.FilterMessage("telegram.polling.failed").All()
	if len(failures) != 1 || failures[0].ContextMap()["api_code"] != int64(409) {
		t.Fatalf("missing conflict code: %+v", failures)
	}
	if observed.FilterMessage("telegram.polling.ready").Len() != 1 || observed.FilterMessage("telegram.polling.recovered").Len() != 1 {
		t.Fatal("successful poll did not record readiness and recovery")
	}
}
