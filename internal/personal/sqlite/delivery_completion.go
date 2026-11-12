package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func insertSuppressedDraftHistoryTx(ctx context.Context, tx *sql.Tx, eventID int64, intent personal.DeliveryIntent) error {
	attachments := make([]channel.Attachment, 0, len(intent.Draft.Attachments))
	for _, id := range intent.Draft.Attachments {
		attachments = append(attachments, channel.Attachment{ID: id})
	}
	raw, err := json.Marshal(attachments)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_history(event_id,delivery_id,role,text,attachments,schedule_id)
		VALUES(?,?,'assistant',?,?,?)`, eventID, intent.ID, intent.Draft.Text, string(raw), nullableID(intent.ScheduleID))
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return errors.New("personal: suppressed delivery history was not recorded")
	}
	return nil
}

func scanIntentStateTx(ctx context.Context, tx *sql.Tx, deliveryID int64) (personal.DeliveryIntentState, error) {
	var state personal.DeliveryIntentState
	err := tx.QueryRowContext(ctx, `SELECT state FROM personal_delivery_intents WHERE id=?`, deliveryID).Scan(&state)
	return state, err
}

func detailIfAttachmentFailure(failed bool, detail string) string {
	if !failed {
		return ""
	}
	return detail
}

func (s *Store) appendSupplementalDeliveryTx(ctx context.Context, tx *sql.Tx, eventID int64, supplemental personal.SupplementalReply) (personal.DeliveryIntent, int, error) {
	if supplemental.Purpose != personal.DeliveryResult && supplemental.Purpose != personal.DeliveryStatus && supplemental.Purpose != personal.DeliveryControl {
		return personal.DeliveryIntent{}, 0, fmt.Errorf("personal: invalid supplemental delivery purpose %q", supplemental.Purpose)
	}
	if supplemental.ScheduleID < 0 {
		return personal.DeliveryIntent{}, 0, errors.New("personal: supplemental schedule ID cannot be negative")
	}
	if err := validateDeliveryDraft(supplemental.Draft); err != nil {
		return personal.DeliveryIntent{}, 0, err
	}
	for _, id := range supplemental.TaskIDs {
		if id <= 0 {
			return personal.DeliveryIntent{}, 0, errors.New("personal: supplemental task IDs must be positive")
		}
	}
	if err := validatePreparedText("supplemental reply", supplemental.Draft.Text, supplemental.Chunks); err != nil {
		return personal.DeliveryIntent{}, 0, err
	}
	attachments, err := s.validateOrdinaryAttachmentsTx(ctx, tx, supplemental.Draft.Attachments)
	if err != nil {
		return personal.DeliveryIntent{}, 0, err
	}
	scopes, err := deliveryScopesForTaskIDsTx(ctx, tx, supplemental.TaskIDs)
	if err != nil {
		return personal.DeliveryIntent{}, 0, err
	}
	intent, err := insertDeliveryIntentTx(ctx, tx, deliveryIntentInsert{
		eventID: eventID, scheduleID: supplemental.ScheduleID, purpose: supplemental.Purpose,
		draft: supplemental.Draft, scopes: scopes,
	}, time.Now().UTC())
	if err != nil {
		return personal.DeliveryIntent{}, 0, err
	}
	if err := refreshDeliveryIntentValidityTx(ctx, tx, intent.ID); err != nil {
		return personal.DeliveryIntent{}, 0, err
	}
	intent.State, err = scanIntentStateTx(ctx, tx, intent.ID)
	if err != nil {
		return personal.DeliveryIntent{}, 0, err
	}
	if intent.State == personal.DeliveryIntentSuppressed {
		if err := insertSuppressedDraftHistoryTx(ctx, tx, eventID, intent); err != nil {
			return personal.DeliveryIntent{}, 0, err
		}
		return intent, 0, nil
	}
	parts, err := appendDeliveryPartsTx(ctx, tx, eventID, intent.ID, supplemental.Draft.Text, attachments, supplemental.Chunks, supplemental.ScheduleID, 0)
	if err != nil {
		return personal.DeliveryIntent{}, 0, err
	}
	if err := updateDeliveryIntentRenderedTx(ctx, tx, intent.ID, ""); err != nil {
		return personal.DeliveryIntent{}, 0, err
	}
	return intent, parts, nil
}
