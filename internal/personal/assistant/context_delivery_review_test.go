package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"kagari/internal/channel"
	"kagari/internal/inputbudget"
	"kagari/internal/personal"
)

func TestDeliveryBodyExpansionPreservesMixedHistoryOrder(t *testing.T) {
	input := personal.TurnInput{Event: personal.Event{ID: 9, Kind: personal.EventUser, Text: "What was accepted?"}}
	for i := int64(1); i <= 5; i++ {
		message := personal.Message{HistoryID: i, EventID: i, Role: "user", Text: fmt.Sprintf("message %d", i)}
		if i%2 == 0 {
			message.Role = "assistant"
			message.Delivery = &personal.DeliveryIntent{
				ID: i + 10, EventID: i, Purpose: personal.DeliveryResult, State: personal.DeliveryIntentRendered,
				Draft: personal.DeliveryDraft{Text: fmt.Sprintf("draft %d", i)},
				Progress: &personal.DeliveryProgress{AcceptedItems: 1, Items: []personal.DeliveryItem{
					{OutboxID: i + 20, DeliveryPart: 1, Text: fmt.Sprintf("accepted part %d", i), Status: "sent", Receipt: &channel.Receipt{MessageID: fmt.Sprintf("remote-%d", i)}},
					{OutboxID: i + 30, DeliveryPart: 2, AttachmentID: fmt.Sprintf("file-%d", i), Status: "pending"},
				}},
			}
		}
		input.History = append(input.History, message)
	}
	input.RepliedMessage = &personal.Message{
		HistoryID: 4, OutboxID: 24, EventID: 4, Role: "assistant", Text: "accepted part 4",
		Delivery: input.History[3].Delivery,
	}
	before, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := inputbudget.NewLimits(50_000, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := assembleTurn(context.Background(), input, "instruction", nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(assembled.History) != len(input.History) {
		t.Fatalf("mixed history lost messages: %+v", assembled.History)
	}
	for i, message := range assembled.History {
		original := input.History[i]
		if message.HistoryID != original.HistoryID || message.EventID != original.EventID || message.Role != original.Role || message.Text != original.Text || message.TextOmitted {
			t.Fatalf("expanded message %d overwrote another record: got=%+v want=%+v", i, message, original)
		}
		if original.Delivery != nil && (message.Delivery == nil || message.Delivery.ID != original.Delivery.ID || len(message.Delivery.Progress.Items) != 2 || message.Delivery.Progress.Items[1].Status != "pending") {
			t.Fatalf("message %d lost delivery identity or partial status: %+v", i, message)
		}
	}
	if assembled.RepliedMessage.Text != "accepted part 4" || assembled.RepliedMessage.OutboxID != 24 || assembled.RepliedMessage.Delivery.ID != 14 {
		t.Fatalf("exact reply changed while expanding surrounding history: %+v", assembled.RepliedMessage)
	}
	after, err := json.Marshal(input)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("assembly mutated the source snapshot: %v", err)
	}
}
