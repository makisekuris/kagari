package assistant

import "kagari/internal/personal"

func compactDeliveryHistoryMessage(message personal.Message) personal.Message {
	message.TextOmitted = message.TextOmitted || message.Text != ""
	message.Text = ""
	// The immutable attachment IDs and per-item status remain in Delivery.
	message.Attachments = nil
	if message.Delivery != nil {
		intent := personal.DeliveryCard(*message.Delivery)
		message.Delivery = &intent
	}
	return message
}
