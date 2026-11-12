package service

import (
	"context"
	"errors"

	"kagari/internal/personal"
)

// The unfinished source event is the recovery cursor for committed settlements.
// Completing it also commits every rendered delivery, so a restart either
// presents the saved drafts or observes the complete existing outbox.
func (r *Runtime) publishIntents(ctx context.Context, event personal.Event, intents []personal.DeliveryIntent) error {
	renders := make([]personal.RenderedDelivery, 0, len(intents))
	for _, intent := range intents {
		output := personal.TurnOutput{Text: intent.Draft.Text, Attachments: intent.Draft.Attachments, ScheduleID: intent.ScheduleID}
		if len(intent.Scopes) == 1 {
			output.TaskID = intent.Scopes[0].TaskID
		}
		presented, chunks, err := r.renderReply(ctx, event, output)
		if err != nil {
			return err
		}
		renders = append(renders, personal.RenderedDelivery{DeliveryID: intent.ID, Text: presented.Text, Chunks: chunks})
	}
	for {
		err := r.Store.CompleteEvent(ctx, event.ID, personal.TurnOutput{Silent: true}, nil, renders...)
		var unavailable *personal.DeliveryAttachmentError
		if !errors.As(err, &unavailable) || !errors.Is(err, personal.ErrAttachmentUnavailable) {
			return err
		}
		index := -1
		for i := range renders {
			if renders[i].DeliveryID == unavailable.DeliveryID && !renders[i].AttachmentFailure {
				index = i
				break
			}
		}
		if index < 0 {
			return err
		}
		text, chunks, prepareErr := r.unavailableAttachmentReply(ctx, event)
		if prepareErr != nil {
			return prepareErr
		}
		renders[index].Text = text
		renders[index].Chunks = chunks
		renders[index].AttachmentFailure = true
	}
}
