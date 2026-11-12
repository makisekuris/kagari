package personal

import (
	"slices"
	"time"

	"kagari/internal/channel"
)

// DeliveryItem is the exact channel operation. Sent means channel acceptance,
// not that the owner read the text or opened the attachment.
type DeliveryItem struct {
	OutboxID      int64            `json:"outbox_id"`
	DeliveryPart  int              `json:"delivery_part"`
	Text          string           `json:"text,omitempty"`
	AttachmentID  string           `json:"attachment_id,omitempty"`
	Status        string           `json:"status"`
	Detail        string           `json:"detail,omitempty"`
	Receipt       *channel.Receipt `json:"receipt,omitempty"`
	Attempts      int              `json:"attempts"`
	NextAttemptAt time.Time        `json:"next_attempt_at"`
}

// FullyAccepted requires at least one item, every item sent, and no preparation
// error. A successfully sent missing-file notice does not complete the draft.
type DeliveryProgress struct {
	Items         []DeliveryItem `json:"items"`
	AcceptedItems int            `json:"accepted_items"`
	FullyAccepted bool           `json:"fully_accepted"`
}

// DeliveryCard omits generated text while retaining every item, outcome and
// receipt. get_delivery can reopen the text; the card never implies acceptance.
func DeliveryCard(intent DeliveryIntent) DeliveryIntent {
	intent.Scopes = slices.Clone(intent.Scopes)
	intent.Draft.Attachments = slices.Clone(intent.Draft.Attachments)
	intent.ContentOmitted = intent.ContentOmitted || intent.Draft.Text != ""
	intent.Draft.Text = ""
	if intent.Progress != nil {
		progress := *intent.Progress
		progress.Items = slices.Clone(progress.Items)
		for i := range progress.Items {
			intent.ContentOmitted = intent.ContentOmitted || progress.Items[i].Text != ""
			progress.Items[i].Text = ""
		}
		intent.Progress = &progress
	}
	return intent
}
