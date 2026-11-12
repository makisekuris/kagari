package assistant

import (
	"context"
	"strings"

	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/logging"
	"kagari/internal/personal"
)

// Tool outcomes and explicit questions take precedence over the model's reply label.
func (a *MainAgent) selectReply(ctx context.Context, input personal.TurnInput, tools *personalTools, raw string, createdAttachments []channel.Attachment, modelErr error) (personalTurnResponse, error) {
	required := tools.requiredTaskReply()
	if modelErr != nil {
		if required == "" {
			return personalTurnResponse{}, modelErr
		}
		logging.FromContext(ctx).Warn("conversation.reply.recovered", append(logging.ErrorFields(modelErr), zap.Int64("event_id", input.Event.ID))...)
		return personalTurnResponse{Text: required, ReplyKind: personal.ReplyContent}, nil
	}
	bareLink := input.Event.Kind == personal.EventUser && len(input.Event.Attachments) == 0 && personal.IsPureURLMessage(input.Event.Text)
	if bareLink && !tools.mustNotify {
		deferred, err := a.Store.CanDeferReply(ctx, input.Event.ID)
		if err != nil {
			return personalTurnResponse{}, err
		}
		if required != "" {
			if deferred {
				required += "\n\n已经安排的后台工作会继续处理。"
			}
			return personalTurnResponse{Text: required, ReplyKind: personal.ReplyContent}, nil
		}
		if deferred {
			// Queuing a bare link supplies no reading result, regardless of draft text or attachments.
			return personalTurnResponse{ReplyKind: personal.ReplySilent}, nil
		}
	}
	response, err := decodePersonalTurnResponse(raw)
	if err != nil {
		// Formatting gets one tool-free attempt; committed actions are never replayed.
		response, err = a.formatTurn(ctx, raw, createdAttachments)
	}
	if required != "" {
		if err != nil {
			logging.FromContext(ctx).Warn("conversation.reply.recovered", append(logging.ErrorFields(err), zap.Int64("event_id", input.Event.ID))...)
		}
		if err == nil && tools.mustNotify && strings.TrimSpace(response.Text) != "" {
			response.Text += "\n\n" + required
		} else {
			response.Text = required
		}
		response.ReplyKind = personal.ReplyContent
		return response, nil
	}
	return response, err
}
