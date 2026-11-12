package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

// loadDeliverySnapshotsTx attaches the live outbox projection to immutable
// delivery intents. Callers keep the surrounding read transaction open until
// all related records have also been read.
func loadDeliverySnapshotsTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]personal.DeliveryIntent, error) {
	intents, err := loadDeliveryIntentsTx(ctx, tx, query, args...)
	if err != nil {
		return nil, err
	}
	if err := attachDeliveryProgressTx(ctx, tx, intents); err != nil {
		return nil, err
	}
	return intents, nil
}

func loadDeliverySnapshotTx(ctx context.Context, tx *sql.Tx, deliveryID int64) (*personal.DeliveryIntent, error) {
	intents, err := loadDeliverySnapshotsTx(ctx, tx, `SELECT `+deliveryIntentColumns+` FROM personal_delivery_intents WHERE id=?`, deliveryID)
	if err != nil {
		return nil, err
	}
	if len(intents) == 0 {
		return nil, nil
	}
	return &intents[0], nil
}

func attachDeliveryProgressTx(ctx context.Context, tx *sql.Tx, intents []personal.DeliveryIntent) error {
	if len(intents) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(intents))
	preparationErrors := make(map[int64]string, len(intents))
	progresses := make(map[int64]*personal.DeliveryProgress, len(intents))
	for _, intent := range intents {
		ids = append(ids, intent.ID)
		preparationErrors[intent.ID] = intent.PreparationError
		progresses[intent.ID] = &personal.DeliveryProgress{Items: []personal.DeliveryItem{}}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,delivery_id,delivery_part,text,attachment_id,status,detail,
		receipt_channel,receipt_account,receipt_conversation,receipt_message_id,attempts,next_attempt_at
		FROM personal_outbox WHERE delivery_id IN (`+inspectionIDs(ids)+`) ORDER BY delivery_id,delivery_part`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item personal.DeliveryItem
		var deliveryID int64
		var attachmentID sql.NullString
		var receipt channel.Receipt
		var nextAttempt string
		if err := rows.Scan(&item.OutboxID, &deliveryID, &item.DeliveryPart, &item.Text, &attachmentID, &item.Status, &item.Detail,
			&receipt.Channel, &receipt.Account, &receipt.Conversation, &receipt.MessageID, &item.Attempts, &nextAttempt); err != nil {
			rows.Close()
			return err
		}
		item.AttachmentID = attachmentID.String
		if receipt != (channel.Receipt{}) {
			item.Receipt = &receipt
		}
		item.NextAttemptAt, err = parseTimestamp(nextAttempt)
		if err != nil {
			rows.Close()
			return fmt.Errorf("personal: delivery %d item %d next attempt: %w", deliveryID, item.DeliveryPart, err)
		}
		progress, ok := progresses[deliveryID]
		if !ok {
			rows.Close()
			return fmt.Errorf("personal: outbox item %d references an unloaded delivery %d", item.OutboxID, deliveryID)
		}
		progress.Items = append(progress.Items, item)
		if item.Status == "sent" {
			progress.AcceptedItems++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for i := range intents {
		progress := progresses[intents[i].ID]
		progress.FullyAccepted = len(progress.Items) > 0 && progress.AcceptedItems == len(progress.Items) && preparationErrors[intents[i].ID] == ""
		intents[i].Progress = progress
	}
	return nil
}

func uniqueDeliveryTaskID(scopes []personal.DeliveryScope) int64 {
	var taskID int64
	for _, scope := range scopes {
		if taskID == 0 {
			taskID = scope.TaskID
			continue
		}
		if taskID != scope.TaskID {
			return 0
		}
	}
	return taskID
}

func loadDeliveryTaskIDTx(ctx context.Context, tx *sql.Tx, deliveryID int64) (int64, error) {
	var taskID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN COUNT(DISTINCT task_id)=1 THEN MIN(task_id) ELSE NULL END
		FROM personal_delivery_scopes WHERE delivery_id=?`, deliveryID).Scan(&taskID)
	if err != nil {
		return 0, err
	}
	return taskID.Int64, nil
}

func loadTaskDeliveryStatesTx(ctx context.Context, tx *sql.Tx, taskID int64) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o.status,COUNT(*) FROM personal_outbox o
		WHERE EXISTS(SELECT 1 FROM personal_delivery_scopes s WHERE s.delivery_id=o.delivery_id AND s.task_id=?)
		GROUP BY o.status`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := make(map[string]int)
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		states[state] = count
	}
	return states, rows.Err()
}
