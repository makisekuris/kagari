package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func validatePreparedDeliveries(intents []personal.DeliveryIntent, prepared []personal.RenderedDelivery) (map[int64]personal.RenderedDelivery, error) {
	byID := make(map[int64]personal.RenderedDelivery, len(prepared))
	known := make(map[int64]personal.DeliveryIntentState, len(intents))
	for _, intent := range intents {
		known[intent.ID] = intent.State
	}
	for _, render := range prepared {
		if render.DeliveryID <= 0 {
			return nil, errors.New("personal: prepared delivery requires a positive delivery ID")
		}
		if _, exists := byID[render.DeliveryID]; exists {
			return nil, fmt.Errorf("personal: duplicate prepared delivery %d", render.DeliveryID)
		}
		if _, exists := known[render.DeliveryID]; !exists {
			return nil, fmt.Errorf("personal: prepared delivery %d is not saved for this event", render.DeliveryID)
		}
		byID[render.DeliveryID] = render
	}
	for _, intent := range intents {
		if intent.State == personal.DeliveryIntentPending {
			if _, ok := byID[intent.ID]; !ok {
				return nil, fmt.Errorf("personal: missing prepared delivery %d", intent.ID)
			}
		}
	}
	return byID, nil
}

func validatePreparedText(name, text string, chunks []string) error {
	if text == "" {
		for _, chunk := range chunks {
			if strings.TrimSpace(chunk) != "" {
				return fmt.Errorf("personal: %s has prepared text but no rendered text", name)
			}
		}
		return nil
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("personal: %s has blank rendered text", name)
	}
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) != "" {
			return nil
		}
	}
	return fmt.Errorf("personal: %s has incomplete prepared text", name)
}

func oneDeliveryTask(intents []personal.DeliveryIntent) (int64, bool) {
	if len(intents) == 0 {
		return 0, false
	}
	var first []int64
	for _, intent := range intents {
		current := []int64{}
		for _, scope := range intent.Scopes {
			found := false
			for _, id := range current {
				if id == scope.TaskID {
					found = true
					break
				}
			}
			if !found {
				current = append(current, scope.TaskID)
			}
		}
		if first == nil {
			first = current
			continue
		}
		if len(first) != len(current) {
			return 0, false
		}
		for i, id := range first {
			if current[i] != id {
				return 0, false
			}
		}
	}
	if len(first) != 1 {
		return 0, false
	}
	return first[0], true
}

func oneDeliverySchedule(intents []personal.DeliveryIntent) (int64, bool) {
	if len(intents) == 0 {
		return 0, false
	}
	scheduleID := intents[0].ScheduleID
	for _, intent := range intents[1:] {
		if scheduleID != intent.ScheduleID {
			return 0, false
		}
	}
	return scheduleID, scheduleID > 0
}

func deliveryIDs(intents []personal.DeliveryIntent) []int64 {
	ids := make([]int64, len(intents))
	for i, intent := range intents {
		ids[i] = intent.ID
	}
	return ids
}

func (s *Store) prepareIntentAttachmentsTx(ctx context.Context, tx *sql.Tx, intent personal.DeliveryIntent, attachmentFailure bool) ([]channel.Attachment, string, error) {
	attachments := make([]channel.Attachment, 0, len(intent.Draft.Attachments))
	var unavailable []string
	for _, id := range intent.Draft.Attachments {
		attachment, err := s.validateStoredAttachment(ctx, tx, id)
		if err != nil {
			if !errors.Is(err, personal.ErrAttachmentUnavailable) {
				return nil, "", err
			}
			unavailable = append(unavailable, err.Error())
			continue
		}
		attachments = append(attachments, attachment)
	}
	if !attachmentFailure {
		if len(unavailable) > 0 {
			return nil, "", &personal.DeliveryAttachmentError{DeliveryID: intent.ID, Err: fmt.Errorf("%w: %s", personal.ErrAttachmentUnavailable, strings.Join(unavailable, "; "))}
		}
		return attachments, "", nil
	}
	if len(intent.Draft.Attachments) == 0 || len(unavailable) == 0 {
		return nil, "", fmt.Errorf("personal: delivery %d claims an attachment failure without an unavailable selected file", intent.ID)
	}
	return nil, strings.Join(unavailable, "; "), nil
}

func (s *Store) validateOrdinaryAttachmentsTx(ctx context.Context, tx *sql.Tx, ids []string) ([]channel.Attachment, error) {
	attachments := make([]channel.Attachment, 0, len(ids))
	for _, id := range ids {
		attachment, err := s.validateStoredAttachment(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}
