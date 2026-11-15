package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"kagari/internal/personal"
)

const replySchema = `
CREATE TABLE IF NOT EXISTS personal_reply_groups (
	id INTEGER PRIMARY KEY REFERENCES personal_events(id),
	created_at TEXT NOT NULL,
	wait_notified INTEGER NOT NULL DEFAULT 0 CHECK(wait_notified IN (0,1)),
	wait_outbox_id INTEGER REFERENCES personal_outbox(id),
	wait_history_id INTEGER REFERENCES personal_history(id),
	has_content INTEGER NOT NULL DEFAULT 0 CHECK(has_content IN (0,1)),
	closed INTEGER NOT NULL DEFAULT 0 CHECK(closed IN (0,1))
);
CREATE INDEX IF NOT EXISTS personal_reply_groups_wait ON personal_reply_groups(created_at)
	WHERE wait_notified=0 AND has_content=0 AND closed=0;
CREATE TABLE IF NOT EXISTS personal_reply_inputs (
	event_id INTEGER PRIMARY KEY REFERENCES personal_events(id),
	group_id INTEGER NOT NULL REFERENCES personal_reply_groups(id)
);
CREATE INDEX IF NOT EXISTS personal_reply_inputs_group ON personal_reply_inputs(group_id,event_id);
CREATE TABLE IF NOT EXISTS personal_reply_tasks (
	group_id INTEGER NOT NULL REFERENCES personal_reply_groups(id),
	task_id INTEGER NOT NULL REFERENCES personal_tasks(id),
	PRIMARY KEY(group_id,task_id)
);
CREATE INDEX IF NOT EXISTS personal_reply_tasks_task ON personal_reply_tasks(task_id,group_id);
`

func suppressPendingReplyWaitTx(ctx context.Context, tx *sql.Tx, outboxID int64, reason string) error {
	var status string
	var deliveryID int64
	err := tx.QueryRowContext(ctx, `SELECT status,delivery_id FROM personal_outbox WHERE id=?`, outboxID).Scan(&status, &deliveryID)
	if errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err != nil {
		return err
	}
	if status != "pending" {
		return nil
	}
	return suppressDeliveryTx(ctx, tx, deliveryID, reason)
}

func (s *Store) CanDeferReply(ctx context.Context, eventID int64) (bool, error) {
	var kind personal.EventKind
	if err := s.db.QueryRowContext(ctx, `SELECT kind FROM personal_events WHERE id=?`, eventID).Scan(&kind); err != nil {
		return false, err
	}
	if kind != personal.EventUser {
		return true, nil
	}
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM personal_requests r JOIN personal_tasks t ON t.id=r.task_id
			WHERE r.event_id=? AND r.operation IN ('create_task','revise_task','continue_task','add_task_input')
			AND r.task_id IS NOT NULL AND r.run_id IS NOT NULL
		) AND NOT EXISTS(
			SELECT 1 FROM personal_requests r
			WHERE r.event_id=? AND (r.memory_history_id IS NOT NULL OR r.schedule_id IS NOT NULL)
		) AND NOT EXISTS(
			SELECT 1 FROM personal_requests r JOIN personal_tasks t ON t.id=r.task_id
			WHERE r.event_id=? AND r.operation IN ('create_task','revise_task','continue_task','add_task_input')
			AND r.task_id IS NOT NULL AND r.run_id IS NOT NULL
			AND t.status NOT IN ('queued','running','awaiting_review')
		)`, eventID, eventID, eventID).Scan(&allowed)
	return allowed, err
}

func suppressRetiredWaitingRepliesTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT g.wait_outbox_id FROM personal_reply_groups g
		JOIN personal_outbox d ON d.id=g.wait_outbox_id
		WHERE d.status='pending' ORDER BY g.wait_outbox_id`)
	if err != nil {
		return err
	}
	var pending []int64
	for rows.Next() {
		var outboxID int64
		if err := rows.Scan(&outboxID); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, outboxID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, outboxID := range pending {
		if err := suppressPendingReplyWaitTx(ctx, tx, outboxID, deliverySuppressionWaitRetired); err != nil {
			return err
		}
	}
	return nil
}

// suppressStaleWaitRepliesTx leaves sending and settled deliveries intact.
func suppressStaleWaitRepliesTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT g.wait_outbox_id FROM personal_reply_groups g
		JOIN personal_outbox d ON d.id=g.wait_outbox_id
		WHERE d.status='pending' AND (g.has_content=1 OR g.closed=1 OR (
			EXISTS(SELECT 1 FROM personal_reply_tasks rt WHERE rt.group_id=g.id) AND
			NOT EXISTS(SELECT 1 FROM personal_reply_tasks rt JOIN personal_tasks t ON t.id=rt.task_id
				WHERE rt.group_id=g.id AND t.status IN ('queued','running','awaiting_review'))
		))`)
	if err != nil {
		return err
	}
	var stale []int64
	for rows.Next() {
		var outboxID int64
		if err := rows.Scan(&outboxID); err != nil {
			rows.Close()
			return err
		}
		stale = append(stale, outboxID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, outboxID := range stale {
		if err := suppressPendingReplyWaitTx(ctx, tx, outboxID, deliverySuppressionWaitSuperseded); err != nil {
			return err
		}
	}
	return nil
}
