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
	if tools.settledDelivery != nil {
		if modelErr != nil {
			logging.FromContext(ctx).Warn("conversation.reply.recovered", append(logging.ErrorFields(modelErr), zap.Int64("event_id", input.Event.ID))...)
		}
		return personalTurnResponse{
			Text:        tools.settledDelivery.Text,
			ReplyKind:   personal.ReplyContent,
			Attachments: append([]string{}, tools.settledDelivery.Attachments...),
		}, nil
	}
	if tools.persistedControl {
		if modelErr != nil {
			logging.FromContext(ctx).Warn("conversation.reply.recovered", append(logging.ErrorFields(modelErr), zap.Int64("event_id", input.Event.ID))...)
		}
		response := personalTurnResponse{ReplyKind: personal.ReplyContent, Attachments: []string{}}
		seen := make(map[string]struct{})
		for _, delivery := range tools.controlDeliveries {
			if delivery.Draft.Text != "" {
				if response.Text != "" {
					response.Text += "\n\n"
				}
				response.Text += delivery.Draft.Text
			}
			for _, id := range delivery.Draft.Attachments {
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
				response.Attachments = append(response.Attachments, id)
			}
		}
		if !tools.supplementalWork {
			return response, nil
		}
		if modelErr != nil {
			response.SupplementalReplyFailed = true
			return response, nil
		}
		supplemental, err := decodePersonalTurnResponseOrTrailingEnvelope(raw)
		if err != nil {
			supplemental, err = a.formatTurn(ctx, raw, createdAttachments)
		}
		if err != nil {
			logging.FromContext(ctx).Warn("conversation.reply.supplemental_recovered", append(logging.ErrorFields(err), zap.Int64("event_id", input.Event.ID))...)
			response.SupplementalReplyFailed = true
			return response, nil
		}
		if required == "" && input.Event.Kind == personal.EventUser && tools.supplementalPurpose == personal.DeliveryStatus && (supplemental.ReplyKind == personal.ReplyReceipt || supplemental.ReplyKind == personal.ReplySilent) {
			deferred, err := a.Store.CanDeferReply(ctx, input.Event.ID)
			if err != nil {
				return personalTurnResponse{}, err
			}
			if deferred {
				return response, nil
			}
		}
		if supplemental.ReplyKind == personal.ReplySilent || strings.TrimSpace(supplemental.Text) == "" && len(supplemental.Attachments) == 0 {
			response.SupplementalReplyFailed = true
			return response, nil
		}
		purpose := tools.supplementalPurpose
		if purpose == "" {
			purpose = personal.DeliveryControl
		}
		response.SupplementalReply = &personal.SupplementalReply{
			Draft:      personal.DeliveryDraft{Text: supplemental.Text, Attachments: append([]string{}, supplemental.Attachments...)},
			TaskIDs:    tools.supplementalTaskIDsSorted(),
			ScheduleID: tools.supplementalScheduleID(),
			Purpose:    purpose,
		}
		return response, nil
	}
	if text, ok := tools.currentMemoryAnswerReply(input.Event, required); ok {
		return personalTurnResponse{
			Text:                  text,
			ReplyKind:             personal.ReplyContent,
			Attachments:           []string{},
			SkipReplyPresentation: true,
		}, nil
	}
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
	response, err := decodePersonalTurnResponseOrTrailingEnvelope(raw)
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
	if err == nil && input.Event.Kind == personal.EventUser && !tools.mustNotify && response.ReplyKind == personal.ReplyReceipt {
		deferred, err := a.Store.CanDeferReply(ctx, input.Event.ID)
		if err != nil {
			return personalTurnResponse{}, err
		}
		if deferred {
			return personalTurnResponse{ReplyKind: personal.ReplySilent}, nil
		}
	}
	return response, err
}

func decodePersonalTurnResponseOrTrailingEnvelope(raw string) (personalTurnResponse, error) {
	response, err := decodePersonalTurnResponse(raw)
	if err == nil {
		return response, nil
	}
	if strings.Contains(raw, "```") {
		return personalTurnResponse{}, err
	}
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	beforeObject, afterObject, found := strings.Cut(raw, "{")
	if !found {
		return personalTurnResponse{}, err
	}
	separator := strings.LastIndex(beforeObject, "\n\n")
	if separator < 0 || strings.TrimSpace(beforeObject[separator+2:]) != "" {
		return personalTurnResponse{}, err
	}
	prefix := strings.TrimSpace(beforeObject[:separator])
	if prefix == "" || strings.Contains(prefix, "}") || strings.HasSuffix(prefix, ":") || strings.HasSuffix(prefix, "：") {
		return personalTurnResponse{}, err
	}
	response, envelopeErr := decodePersonalTurnResponse("{" + afterObject)
	if envelopeErr != nil {
		return personalTurnResponse{}, err
	}
	return response, nil
}
