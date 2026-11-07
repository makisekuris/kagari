package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"kagari/internal/channel"
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

func deletePendingReplyWaitTx(ctx context.Context, tx *sql.Tx, outboxID int64, historyID sql.NullInt64) error {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, outboxID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err != nil || status != "pending" {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_reply_groups SET wait_outbox_id=NULL,wait_history_id=NULL
		WHERE wait_outbox_id=? OR wait_history_id=?`, outboxID, nullableID(historyID.Int64)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM personal_outbox WHERE id=? AND status='pending'`, outboxID); err != nil {
		return err
	}
	if historyID.Valid {
		if _, err := tx.ExecContext(ctx, `DELETE FROM personal_history WHERE id=?`, historyID.Int64); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CanDeferReply(ctx context.Context, eventID int64) (bool, error) {
	var kind EventKind
	if err := s.db.QueryRowContext(ctx, `SELECT kind FROM personal_events WHERE id=?`, eventID).Scan(&kind); err != nil {
		return false, err
	}
	if kind != EventUser {
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

func (s *Store) QueueWaitingReply(ctx context.Context, now time.Time, delay time.Duration) (bool, error) {
	if delay < 0 {
		return false, errors.New("personal: reply wait delay cannot be negative")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	cutoff := timestamp(now.Add(-delay).UTC())
	rows, err := tx.QueryContext(ctx, `SELECT g.id,e.status,e.channel,e.account,e.sender,
		COUNT(rt.task_id),
		SUM(CASE WHEN t.status='queued' THEN 1 ELSE 0 END),
		SUM(CASE WHEN t.status='running' THEN 1 ELSE 0 END),
		SUM(CASE WHEN t.status='awaiting_review' THEN 1 ELSE 0 END)
		FROM personal_reply_groups g JOIN personal_events e ON e.id=g.id
		LEFT JOIN personal_reply_tasks rt ON rt.group_id=g.id
		LEFT JOIN personal_tasks t ON t.id=rt.task_id
		WHERE g.created_at<=? AND g.wait_notified=0 AND g.has_content=0 AND g.closed=0 AND e.kind='user'
		GROUP BY g.id,e.status,e.channel,e.account,e.sender ORDER BY g.created_at,g.id`, cutoff)
	if err != nil {
		return false, err
	}
	type candidate struct {
		id                                 int64
		eventStatus                        EventStatus
		channel, account, sender           string
		taskCount, queued, running, review int
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var taskCount, queued, running, review sql.NullInt64
		if err := rows.Scan(&c.id, &c.eventStatus, &c.channel, &c.account, &c.sender, &taskCount, &queued, &running, &review); err != nil {
			rows.Close()
			return false, err
		}
		c.taskCount, c.queued, c.running, c.review = int(taskCount.Int64), int(queued.Int64), int(running.Int64), int(review.Int64)
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		message := ""
		if candidate.taskCount > 0 {
			switch {
			case candidate.running > 0:
				message = "任务正在处理，完成后我会继续回复。"
			case candidate.queued > 0:
				message = "任务已排队，有结果后我会继续回复。"
			case candidate.review > 0:
				message = "任务正在整理阶段结果，稍后我会继续回复。"
			default:
				continue
			}
			if candidate.taskCount == 1 {
				var title string
				if err := tx.QueryRowContext(ctx, `SELECT t.title FROM personal_reply_tasks rt
					JOIN personal_tasks t ON t.id=rt.task_id WHERE rt.group_id=?`, candidate.id).Scan(&title); err != nil {
					return false, err
				}
				message = "「" + clip(title, 80) + "」" + strings.TrimPrefix(message, "任务")
			}
		} else {
			if candidate.eventStatus != EventQueued && candidate.eventStatus != EventRunning {
				continue
			}
			var earlier int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM personal_reply_groups older
				JOIN personal_events oe ON oe.id=older.id
				WHERE older.id<>? AND older.created_at<=? AND older.closed=0 AND older.has_content=0
				AND oe.kind='user' AND oe.status IN ('queued','running')
				AND oe.channel=? AND oe.account=? AND oe.sender=?
				AND NOT EXISTS(SELECT 1 FROM personal_reply_tasks ort WHERE ort.group_id=older.id)
				AND (older.created_at < (SELECT created_at FROM personal_reply_groups WHERE id=?)
					OR (older.created_at = (SELECT created_at FROM personal_reply_groups WHERE id=?) AND older.id < ?))
			)`, candidate.id, cutoff, candidate.channel, candidate.account, candidate.sender, candidate.id, candidate.id, candidate.id).Scan(&earlier); err != nil {
				return false, err
			}
			if earlier != 0 {
				continue
			}
			if candidate.eventStatus == EventQueued {
				message = "消息已收到，正在等候处理。"
			} else {
				message = "我正在处理这条消息，稍后回复。"
			}
		}
		attachmentJSON, err := json.Marshal([]channel.Attachment{})
		if err != nil {
			return false, err
		}
		var part int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(part),0)+1 FROM personal_outbox WHERE event_id=?`, candidate.id).Scan(&part); err != nil {
			return false, err
		}
		history, err := tx.ExecContext(ctx, `INSERT INTO personal_history(event_id,role,text,attachments) VALUES(?,'assistant',?,?)`,
			candidate.id, message, string(attachmentJSON))
		if err != nil {
			return false, err
		}
		historyID, err := history.LastInsertId()
		if err != nil {
			return false, err
		}
		outbox, err := tx.ExecContext(ctx, `INSERT INTO personal_outbox(event_id,part,text,status,next_attempt_at) VALUES(?,?,?,'pending',?)`,
			candidate.id, part, message, timestamp(now.UTC()))
		if err != nil {
			return false, err
		}
		outboxID, err := outbox.LastInsertId()
		if err != nil {
			return false, err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE personal_reply_groups SET wait_notified=1,wait_outbox_id=?,wait_history_id=? WHERE id=? AND wait_notified=0 AND has_content=0 AND closed=0`, outboxID, historyID, candidate.id)
		if err != nil {
			return false, err
		}
		if changed, err := updated.RowsAffected(); err != nil {
			return false, err
		} else if changed != 1 {
			return false, nil
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// suppressStaleWaitRepliesTx leaves sending and settled deliveries intact.
func suppressStaleWaitRepliesTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT g.wait_outbox_id,g.wait_history_id FROM personal_reply_groups g
		JOIN personal_outbox d ON d.id=g.wait_outbox_id
		WHERE d.status='pending' AND (g.has_content=1 OR g.closed=1 OR (
			EXISTS(SELECT 1 FROM personal_reply_tasks rt WHERE rt.group_id=g.id) AND
			NOT EXISTS(SELECT 1 FROM personal_reply_tasks rt JOIN personal_tasks t ON t.id=rt.task_id
				WHERE rt.group_id=g.id AND t.status IN ('queued','running','awaiting_review'))
		))`)
	if err != nil {
		return err
	}
	var stale []replyWait
	for rows.Next() {
		var wait replyWait
		if err := rows.Scan(&wait.outboxID, &wait.historyID); err != nil {
			rows.Close()
			return err
		}
		wait.status = "pending"
		stale = append(stale, wait)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, wait := range stale {
		if err := deletePendingReplyWaitTx(ctx, tx, wait.outboxID, wait.historyID); err != nil {
			return err
		}
	}
	return nil
}
