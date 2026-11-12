package personal

import (
	"reflect"
	"testing"

	"kagari/internal/channel"
)

func TestDeliveryCardRetainsPartialAcceptanceAndDoesNotMutateSource(t *testing.T) {
	receipt := &channel.Receipt{MessageID: "accepted-text"}
	source := DeliveryIntent{
		ID: 7, OriginalDeliveryID: 3, Purpose: DeliveryHistorical, State: DeliveryIntentSuppressed,
		Detail: "task_delivery_stopped", PreparationError: "original file unavailable",
		Draft:  DeliveryDraft{Text: "saved draft", Attachments: []string{"report", "evidence"}},
		Scopes: []DeliveryScope{{TaskID: 5, Revision: 2, RunID: 9}},
		Progress: &DeliveryProgress{AcceptedItems: 1, Items: []DeliveryItem{
			{OutboxID: 10, DeliveryPart: 0, Text: "accepted part", Status: "sent", Receipt: receipt},
			{OutboxID: 11, DeliveryPart: 1, AttachmentID: "report", Status: "uncertain", Detail: "interrupted send"},
			{OutboxID: 12, DeliveryPart: 2, AttachmentID: "evidence", Status: "suppressed", Detail: "stopped remainder"},
		}},
	}
	card := DeliveryCard(source)
	if !card.ContentOmitted || card.Draft.Text != "" || card.Progress.Items[0].Text != "" {
		t.Fatalf("body was not explicitly omitted: %+v", card)
	}
	if source.Draft.Text != "saved draft" || source.Progress.Items[0].Text != "accepted part" || source.ContentOmitted {
		t.Fatal("card mutated the source snapshot")
	}
	card.Draft.Text = source.Draft.Text
	card.Progress.Items[0].Text = source.Progress.Items[0].Text
	card.ContentOmitted = false
	if !reflect.DeepEqual(card, source) {
		t.Fatalf("card changed delivery facts: got=%+v want=%+v", card, source)
	}
}
