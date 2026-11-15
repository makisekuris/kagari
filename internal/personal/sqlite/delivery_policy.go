package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"kagari/internal/personal"
)

const (
	deliverySuppressionWaitSuperseded = "reply_wait_superseded"
	deliverySuppressionWaitRetired    = "automatic_reply_wait_retired"
	deliverySuppressionTaskRevision   = "task_revision_changed"
	deliverySuppressionTaskRun        = "task_run_changed"
	deliverySuppressionTaskCancelled  = "task_cancelled_status"
	deliverySuppressionTaskObsolete   = "task_status_obsolete"
	deliverySuppressionTaskStopped    = "task_delivery_stopped"
	deliverySuppressionPartFailed     = "delivery_part_failed"
	deliverySuppressionPartUncertain  = "delivery_part_uncertain"
)

// suppressDeliveryTx makes a persisted delivery ineligible for future sends.
// Sending and terminal outbox rows remain untouched so known receipts survive.
func suppressDeliveryTx(ctx context.Context, tx *sql.Tx, deliveryID int64, reason string) error {
	if deliveryID <= 0 || reason == "" {
		return errors.New("personal: delivery ID and suppression reason are required")
	}
	var state, detail string
	if err := tx.QueryRowContext(ctx, `SELECT state,detail FROM personal_delivery_intents WHERE id=?`, deliveryID).Scan(&state, &detail); err != nil {
		return err
	}
	if state != string(personal.DeliveryIntentPending) && state != string(personal.DeliveryIntentRendered) && state != string(personal.DeliveryIntentSuppressed) {
		return fmt.Errorf("personal: delivery %d has invalid state %q", deliveryID, state)
	}
	var activeItems int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_outbox WHERE delivery_id=? AND status IN ('pending','sending')`, deliveryID).Scan(&activeItems); err != nil {
		return err
	}
	if state == string(personal.DeliveryIntentRendered) && activeItems == 0 {
		return nil
	}
	intentDetail := reason
	if detail != "" && state != string(personal.DeliveryIntentSuppressed) {
		intentDetail = detail + "; " + reason
	}
	if state != string(personal.DeliveryIntentSuppressed) {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_delivery_intents SET state='suppressed',detail=? WHERE id=? AND state<>'suppressed'`, intentDetail, deliveryID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_outbox SET status='suppressed',detail=? WHERE delivery_id=? AND status='pending'`, reason, deliveryID); err != nil {
		return err
	}
	return nil
}

func taskDeliveryStoppedTx(ctx context.Context, tx *sql.Tx, taskID int64) (bool, error) {
	var action string
	err := tx.QueryRowContext(ctx, `SELECT action FROM personal_delivery_controls
		WHERE target_task_id=? AND action IN ('stop','resume') ORDER BY id DESC LIMIT 1`, taskID).Scan(&action)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return action == string(personal.DeliveryStop), nil
}

func suppressTaskDeliveriesTx(ctx context.Context, tx *sql.Tx, taskID int64, reason string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT d.id FROM personal_delivery_intents d
		JOIN personal_delivery_scopes s ON s.delivery_id=d.id
		WHERE s.task_id=? AND d.purpose<>'control' AND (d.state<>'suppressed' OR EXISTS(
			SELECT 1 FROM personal_outbox o WHERE o.delivery_id=d.id AND o.status='pending')) ORDER BY d.id`, taskID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := suppressDeliveryTx(ctx, tx, id, reason); err != nil {
			return err
		}
	}
	return nil
}

// refreshDeliveryValidityTx rechecks every non-Historical delivery that shares
// this Task scope. A result is tied to its revision; a status is also tied to
// the Run that produced it. Stop gates affect only automatic result/status work.
func refreshDeliveryValidityTx(ctx context.Context, tx *sql.Tx, taskID int64) error {
	if taskID <= 0 {
		return errors.New("personal: delivery validity requires a positive task ID")
	}
	var currentRevision int64
	var taskStatus personal.TaskStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, taskID).Scan(&currentRevision, &taskStatus); err != nil {
		return err
	}
	var latestRunID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT id FROM personal_runs WHERE task_id=? AND revision=? AND kind='task' ORDER BY id DESC LIMIT 1`, taskID, currentRevision).Scan(&latestRunID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	stopped, err := taskDeliveryStoppedTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT d.id,d.purpose,d.state,d.detail,s.revision,COALESCE(s.run_id,0)
		FROM personal_delivery_intents d JOIN personal_delivery_scopes s ON s.delivery_id=d.id
		WHERE s.task_id=? AND (d.state<>'suppressed' OR EXISTS(SELECT 1 FROM personal_outbox o WHERE o.delivery_id=d.id AND o.status='pending'))
		AND d.purpose<>'control' ORDER BY d.id,s.ordinal`, taskID)
	if err != nil {
		return err
	}
	type scopedDelivery struct {
		id       int64
		purpose  personal.DeliveryPurpose
		state    string
		detail   string
		revision int64
		runID    int64
	}
	deliveries := map[int64]scopedDelivery{}
	for rows.Next() {
		var item scopedDelivery
		if err := rows.Scan(&item.id, &item.purpose, &item.state, &item.detail, &item.revision, &item.runID); err != nil {
			rows.Close()
			return err
		}
		if old, ok := deliveries[item.id]; ok {
			if item.revision != old.revision || item.runID != old.runID || item.purpose != old.purpose || item.state != old.state || item.detail != old.detail {
				rows.Close()
				return fmt.Errorf("personal: delivery %d has inconsistent Task scopes", item.id)
			}
		} else {
			deliveries[item.id] = item
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range deliveries {
		if item.state == string(personal.DeliveryIntentSuppressed) {
			reason := item.detail
			if reason == "" {
				reason = "delivery_already_suppressed"
			}
			if err := suppressDeliveryTx(ctx, tx, item.id, reason); err != nil {
				return err
			}
			continue
		}
		if item.purpose == personal.DeliveryHistorical {
			continue
		}
		reason := ""
		if item.revision != currentRevision {
			reason = deliverySuppressionTaskRevision
		} else if stopped && (item.purpose == personal.DeliveryResult || item.purpose == personal.DeliveryStatus) {
			reason = deliverySuppressionTaskStopped
		} else if item.purpose == personal.DeliveryStatus {
			switch {
			case taskStatus == personal.TaskCancelled:
				reason = deliverySuppressionTaskCancelled
			case taskStatus == personal.TaskCompleted || taskStatus == personal.TaskFailed || taskStatus == personal.TaskWaitingInput:
				reason = deliverySuppressionTaskObsolete
			case latestRunID.Valid && item.runID != latestRunID.Int64:
				reason = deliverySuppressionTaskRun
			case !latestRunID.Valid && item.runID != 0:
				reason = deliverySuppressionTaskRun
			}
		}
		if reason != "" {
			if err := suppressDeliveryTx(ctx, tx, item.id, reason); err != nil {
				return err
			}
		}
	}
	return nil
}

func refreshAllDeliveryValidityTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT task_id FROM personal_delivery_scopes ORDER BY task_id`)
	if err != nil {
		return err
	}
	var taskIDs []int64
	for rows.Next() {
		var taskID int64
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, taskID := range taskIDs {
		if err := refreshDeliveryValidityTx(ctx, tx, taskID); err != nil {
			return err
		}
	}
	return nil
}

func refreshEventDeliveryValidityTx(ctx context.Context, tx *sql.Tx, eventID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT s.task_id FROM personal_delivery_intents d
		JOIN personal_delivery_scopes s ON s.delivery_id=d.id WHERE d.event_id=? ORDER BY s.task_id`, eventID)
	if err != nil {
		return err
	}
	var taskIDs []int64
	for rows.Next() {
		var taskID int64
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, taskID := range taskIDs {
		if err := refreshDeliveryValidityTx(ctx, tx, taskID); err != nil {
			return err
		}
	}
	return nil
}

func refreshDeliveryIntentValidityTx(ctx context.Context, tx *sql.Tx, deliveryID int64) error {
	var state, detail string
	if err := tx.QueryRowContext(ctx, `SELECT state,detail FROM personal_delivery_intents WHERE id=?`, deliveryID).Scan(&state, &detail); err != nil {
		return err
	}
	if state == string(personal.DeliveryIntentSuppressed) {
		reason := detail
		if reason == "" {
			reason = "delivery_already_suppressed"
		}
		return suppressDeliveryTx(ctx, tx, deliveryID, reason)
	}
	rows, err := tx.QueryContext(ctx, `SELECT task_id FROM personal_delivery_scopes WHERE delivery_id=? ORDER BY ordinal`, deliveryID)
	if err != nil {
		return err
	}
	var taskIDs []int64
	for rows.Next() {
		var taskID int64
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, taskID := range taskIDs {
		if err := refreshDeliveryValidityTx(ctx, tx, taskID); err != nil {
			return err
		}
	}
	return nil
}
