package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/config"
	"kagari/internal/personal"
	"kagari/internal/personal/sqlite"
)

func TestTraceDeliveryProgressPreservesReceiptsWithoutExposingBody(t *testing.T) {
	ctx := context.Background()
	var cfg config.Config
	cfg.Storage.Path = filepath.Join(t.TempDir(), "delivery.db")
	store, err := sqlite.NewStore(cfg.Storage.Path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	eventID, err := acceptTestInput(ctx, store, "a private request")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimEvent(ctx); err != nil {
		t.Fatal(err)
	}
	const first = "private accepted segment"
	const second = "private failed segment"
	if err := store.CompleteEvent(ctx, eventID, personal.TurnOutput{Text: first + "\n\n" + second}, []string{first, second}); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.ClaimDelivery(ctx)
	if err != nil || accepted == nil {
		t.Fatalf("claim first: %+v %v", accepted, err)
	}
	receipt := channel.Receipt{Channel: "telegram", Account: "bot", Conversation: "owner", MessageID: "exact-first-receipt"}
	if err := store.UpdateDelivery(ctx, accepted.ID, "sent", receipt, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	failed, err := store.ClaimDelivery(ctx)
	if err != nil || failed == nil {
		t.Fatalf("claim second: %+v %v", failed, err)
	}
	if err := store.UpdateDelivery(ctx, failed.ID, "failed", channel.Receipt{}, "known channel rejection", time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, full := range []bool{false, true} {
		args := []string{"--event", strconv.FormatInt(eventID, 10)}
		if full {
			args = append(args, "--full")
		}
		var out bytes.Buffer
		if err := inspectTrace(ctx, cfg, args, &out); err != nil {
			t.Fatal(err)
		}
		var got struct {
			State sqlite.DiagnosticSnapshot `json:"state"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.State.DeliveryIntents) != 1 {
			t.Fatalf("trace delivery groups: %+v", got.State.DeliveryIntents)
		}
		intent := got.State.DeliveryIntents[0]
		progress := intent.Progress
		if progress == nil || progress.FullyAccepted || progress.AcceptedItems != 1 || len(progress.Items) != 2 {
			t.Fatalf("trace erased partial acceptance: %+v", progress)
		}
		if progress.Items[0].OutboxID != accepted.ID || progress.Items[0].Receipt == nil || *progress.Items[0].Receipt != receipt || progress.Items[1].OutboxID != failed.ID || progress.Items[1].Status != "failed" || progress.Items[1].Detail != "known channel rejection" {
			t.Fatalf("trace lost exact outcome facts: %+v", progress.Items)
		}
		if strings.Contains(out.String(), first) != full || strings.Contains(out.String(), second) != full {
			t.Fatalf("full=%t trace body selection: %s", full, out.String())
		}
		if intent.ContentOmitted == full {
			t.Fatalf("full=%t content omission marker=%t", full, intent.ContentOmitted)
		}
	}
}
