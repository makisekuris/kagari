package sqlite

import (
	"context"
	"testing"
	"time"

	"kagari/internal/personal"
)

// legacyWaitFixture persists the shape of a waiting notice created by older
// versions. Tests use it to exercise suppression, receipts, and group links.
func legacyWaitFixture(t *testing.T, s *Store, eventID int64) int64 {
	t.Helper()
	ctx := context.Background()
	const text = "Legacy automatic waiting notice"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	groupID, found, err := replyInputGroupTx(ctx, tx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("event %d has no reply group", eventID)
	}
	taskRows, err := tx.QueryContext(ctx, `SELECT task_id FROM personal_reply_tasks WHERE group_id=? ORDER BY task_id`, groupID)
	if err != nil {
		t.Fatal(err)
	}
	var taskIDs []int64
	for taskRows.Next() {
		var taskID int64
		if err := taskRows.Scan(&taskID); err != nil {
			taskRows.Close()
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := taskRows.Err(); err != nil {
		taskRows.Close()
		t.Fatal(err)
	}
	if err := taskRows.Close(); err != nil {
		t.Fatal(err)
	}
	scopes, err := deliveryScopesForTaskIDsTx(ctx, tx, taskIDs)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := insertDeliveryIntentTx(ctx, tx, deliveryIntentInsert{
		eventID: eventID,
		purpose: personal.DeliveryStatus,
		draft:   personal.DeliveryDraft{Text: text, Attachments: []string{}},
		scopes:  scopes,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendDeliveryPartsTx(ctx, tx, eventID, intent.ID, text, nil, []string{text}, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := updateDeliveryIntentRenderedTx(ctx, tx, intent.ID, ""); err != nil {
		t.Fatal(err)
	}
	var historyID, outboxID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM personal_history WHERE delivery_id=?`, intent.ID).Scan(&historyID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM personal_outbox WHERE delivery_id=? ORDER BY delivery_part LIMIT 1`, intent.ID).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	updated, err := tx.ExecContext(ctx, `UPDATE personal_reply_groups SET wait_notified=1,wait_outbox_id=?,wait_history_id=? WHERE id=? AND wait_notified=0`,
		outboxID, historyID, groupID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := updated.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("event %d reply group was not available for legacy wait fixture", eventID)
	}
	if err := refreshDeliveryIntentValidityTx(ctx, tx, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return intent.ID
}
