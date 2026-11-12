package service

import (
	"context"
	"errors"

	"kagari/internal/personal"
)

// The unfinished source event is the recovery cursor for committed deliveries.
// Completing it also commits every rendered delivery, so a restart either
// presents the saved drafts or observes the complete existing outbox.
func (r *Runtime) publishIntents(ctx context.Context, event personal.Event, intents []personal.DeliveryIntent, output personal.TurnOutput) error {
	renders := make([]personal.RenderedDelivery, 0, len(intents))
	for _, intent := range intents {
		if intent.State == personal.DeliveryIntentSuppressed {
			continue
		}
		output := personal.TurnOutput{Text: intent.Draft.Text, Attachments: intent.Draft.Attachments, ScheduleID: intent.ScheduleID, Purpose: intent.Purpose}
		if len(intent.Scopes) == 1 {
			output.TaskID = intent.Scopes[0].TaskID
		}
		presented, chunks, err := r.renderReply(ctx, event, output)
		if err != nil {
			return err
		}
		renders = append(renders, personal.RenderedDelivery{DeliveryID: intent.ID, Text: presented.Text, Chunks: chunks})
	}
	completion := personal.TurnOutput{Silent: true, RequiredFeedback: output.RequiredFeedback, TaskIDs: output.TaskIDs}
	if output.SupplementalReply != nil {
		supplement := *output.SupplementalReply
		draft := personal.TurnOutput{Text: supplement.Draft.Text, Attachments: supplement.Draft.Attachments,
			TaskIDs: supplement.TaskIDs, ScheduleID: supplement.ScheduleID, Purpose: supplement.Purpose}
		if supplement.Purpose == personal.DeliveryStatus {
			draft.ReplyKind = personal.ReplyReceipt
		}
		presented, chunks, err := r.renderReply(ctx, event, draft)
		if err != nil {
			return err
		}
		supplement.Draft.Text, supplement.Chunks = presented.Text, chunks
		completion.SupplementalReply = &supplement
	}
	for {
		err := r.Store.CompleteEvent(ctx, event.ID, completion, nil, renders...)
		var unavailable *personal.DeliveryAttachmentError
		if errors.Is(err, personal.ErrAttachmentUnavailable) && !errors.As(err, &unavailable) && completion.SupplementalReply != nil && len(completion.SupplementalReply.Draft.Attachments) > 0 {
			text, chunks, prepareErr := r.unavailableAttachmentReply(ctx, event)
			if prepareErr != nil {
				return prepareErr
			}
			completion.SupplementalReply.Draft = personal.DeliveryDraft{Text: text, Attachments: []string{}}
			completion.SupplementalReply.Chunks = chunks
			continue
		}
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
