package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func startReplyTx(ctx context.Context, tx *sql.Tx, eventID int64) error {
	var kind personal.EventKind
	var created string
	if err := tx.QueryRowContext(ctx, `SELECT kind,created_at FROM personal_events WHERE id=?`, eventID).Scan(&kind, &created); err != nil {
		return err
	}
	if kind != personal.EventUser {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO personal_reply_groups(id,created_at) VALUES(?,?)`, eventID, created); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO personal_reply_inputs(event_id,group_id) VALUES(?,?)`, eventID, eventID)
	return err
}

func replyInputGroupTx(ctx context.Context, tx *sql.Tx, eventID int64) (int64, bool, error) {
	var groupID int64
	err := tx.QueryRowContext(ctx, `SELECT group_id FROM personal_reply_inputs WHERE event_id=?`, eventID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return groupID, err == nil, err
}

func bindReplyTaskTx(ctx context.Context, tx *sql.Tx, eventID, taskID int64) error {
	groupID, found, err := replyInputGroupTx(ctx, tx, eventID)
	if err != nil || !found {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT g.id FROM personal_reply_tasks rt
		JOIN personal_reply_groups g ON g.id=rt.group_id
		WHERE rt.task_id=? AND g.closed=0 AND g.id<>? ORDER BY g.id`, taskID, groupID)
	if err != nil {
		return err
	}
	var groups []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, otherID := range groups {
		groupID, err = mergeReplyGroupsTx(ctx, tx, groupID, otherID)
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO personal_reply_tasks(group_id,task_id) VALUES(?,?)`, groupID, taskID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE personal_reply_groups SET closed=0 WHERE id=? AND EXISTS(
		SELECT 1 FROM personal_tasks WHERE id=? AND status IN ('queued','running','awaiting_review'))`, groupID, taskID)
	return err
}

func mergeReplyInputsTx(ctx context.Context, tx *sql.Tx, eventID int64, inputIDs []int64) error {
	destination, found, err := replyInputGroupTx(ctx, tx, eventID)
	if err != nil || !found {
		return err
	}
	for _, inputID := range inputIDs {
		source, found, err := replyInputGroupTx(ctx, tx, inputID)
		if err != nil {
			return err
		}
		if !found || source == destination {
			continue
		}
		destination, err = mergeReplyGroupsTx(ctx, tx, destination, source)
		if err != nil {
			return err
		}
	}
	return nil
}

type replyGroup struct {
	id           int64
	createdAt    string
	waitNotified bool
	waitOutbox   sql.NullInt64
	waitHistory  sql.NullInt64
	hasContent   bool
	closed       bool
}

type replyWait struct {
	outboxID  int64
	historyID sql.NullInt64
	status    string
}

func loadReplyGroupTx(ctx context.Context, tx *sql.Tx, id int64) (replyGroup, error) {
	var group replyGroup
	var waitNotified, hasContent, closed int
	err := tx.QueryRowContext(ctx, `SELECT id,created_at,wait_notified,wait_outbox_id,wait_history_id,has_content,closed
		FROM personal_reply_groups WHERE id=?`, id).
		Scan(&group.id, &group.createdAt, &waitNotified, &group.waitOutbox, &group.waitHistory, &hasContent, &closed)
	group.waitNotified, group.hasContent, group.closed = waitNotified != 0, hasContent != 0, closed != 0
	return group, err
}

func loadReplyWaitTx(ctx context.Context, tx *sql.Tx, group replyGroup) (replyWait, bool, error) {
	if !group.waitOutbox.Valid {
		return replyWait{}, false, nil
	}
	var wait replyWait
	wait.outboxID, wait.historyID = group.waitOutbox.Int64, group.waitHistory
	err := tx.QueryRowContext(ctx, `SELECT status FROM personal_outbox WHERE id=?`, wait.outboxID).Scan(&wait.status)
	if errors.Is(err, sql.ErrNoRows) {
		return replyWait{}, false, err
	}
	return wait, err == nil, err
}

func mergeReplyGroupsTx(ctx context.Context, tx *sql.Tx, firstID, secondID int64) (int64, error) {
	if firstID == secondID {
		return firstID, nil
	}
	first, err := loadReplyGroupTx(ctx, tx, firstID)
	if err != nil {
		return 0, err
	}
	second, err := loadReplyGroupTx(ctx, tx, secondID)
	if err != nil {
		return 0, err
	}
	root, source := first, second
	if second.createdAt < first.createdAt || second.createdAt == first.createdAt && second.id < first.id {
		root, source = second, first
	}
	waits := make([]replyWait, 0, 2)
	for _, group := range []replyGroup{root, source} {
		wait, found, err := loadReplyWaitTx(ctx, tx, group)
		if err != nil {
			return 0, err
		}
		if found {
			waits = append(waits, wait)
		}
	}
	var selected *replyWait
	for i := range waits {
		if selected == nil || replyWaitPriority(waits[i].status) > replyWaitPriority(selected.status) {
			selected = &waits[i]
		}
	}
	var waitOutboxID, waitHistoryID any
	if selected != nil {
		waitOutboxID = selected.outboxID
		if selected.historyID.Valid {
			waitHistoryID = selected.historyID.Int64
		}
	}
	waitNotified := root.waitNotified || source.waitNotified
	hasContent := root.hasContent || source.hasContent
	closed := root.closed && source.closed
	createdAt := root.createdAt
	if source.createdAt < createdAt {
		createdAt = source.createdAt
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_reply_inputs SET group_id=? WHERE group_id=?`, root.id, source.id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO personal_reply_tasks(group_id,task_id)
		SELECT ?,task_id FROM personal_reply_tasks WHERE group_id=?`, root.id, source.id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM personal_reply_tasks WHERE group_id=?`, source.id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_reply_groups SET created_at=?,wait_notified=?,wait_outbox_id=?,wait_history_id=?,has_content=?,closed=? WHERE id=?`,
		createdAt, waitNotified, waitOutboxID, waitHistoryID, hasContent, closed, root.id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM personal_reply_groups WHERE id=?`, source.id); err != nil {
		return 0, err
	}
	for _, wait := range waits {
		if selected != nil && wait.outboxID == selected.outboxID {
			continue
		}
		if wait.status == "pending" {
			if err := deletePendingReplyWaitTx(ctx, tx, wait.outboxID, wait.historyID); err != nil {
				return 0, err
			}
		}
	}
	return root.id, nil
}

func replyWaitPriority(status string) int {
	switch status {
	case "sending":
		return 4
	case "sent", "uncertain":
		return 3
	case "pending":
		return 2
	default:
		return 1
	}
}

func recordReplyTx(ctx context.Context, tx *sql.Tx, eventID int64, output personal.TurnOutput) error {
	var kind personal.EventKind
	var eventTaskID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT kind,task_id FROM personal_events WHERE id=?`, eventID).Scan(&kind, &eventTaskID); err != nil {
		return err
	}
	var requestTasks []int64
	if kind == personal.EventUser {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT task_id FROM personal_requests WHERE event_id=?
			AND operation IN ('create_task','revise_task','continue_task','add_task_input') AND task_id IS NOT NULL AND run_id IS NOT NULL ORDER BY task_id`, eventID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var taskID int64
			if err := rows.Scan(&taskID); err != nil {
				rows.Close()
				return err
			}
			requestTasks = append(requestTasks, taskID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, taskID := range requestTasks {
			if err := bindReplyTaskTx(ctx, tx, eventID, taskID); err != nil {
				return err
			}
		}
	}
	visible := !output.Silent && (output.Text != "" || len(output.Attachments) > 0)
	if !visible {
		return nil
	}
	var groupIDs []int64
	addGroup := func(id int64) {
		if id <= 0 {
			return
		}
		for _, current := range groupIDs {
			if current == id {
				return
			}
		}
		groupIDs = append(groupIDs, id)
	}
	if kind == personal.EventUser {
		if groupID, found, err := replyInputGroupTx(ctx, tx, eventID); err != nil {
			return err
		} else if found {
			addGroup(groupID)
		}
	}
	callbackTaskID := eventTaskID.Int64
	if kind == personal.EventTask && callbackTaskID > 0 {
		if err := appendOpenReplyTaskGroupsTx(ctx, tx, callbackTaskID, &groupIDs); err != nil {
			return err
		}
	}
	// Ordinary replies can report progress through get_task while the task remains active.
	if kind == personal.EventUser && len(requestTasks) == 0 && output.TaskID > 0 {
		if err := appendOpenReplyTaskGroupsTx(ctx, tx, output.TaskID, &groupIDs); err != nil {
			return err
		}
	}
	for _, groupID := range groupIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_reply_groups SET has_content=1 WHERE id=?`, groupID); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM personal_reply_tasks rt JOIN personal_tasks t ON t.id=rt.task_id
			WHERE rt.group_id=? AND t.status IN ('queued','running','awaiting_review'))`, groupID).Scan(&active); err != nil {
			return err
		}
		if active == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE personal_reply_groups SET closed=1 WHERE id=?`, groupID); err != nil {
				return err
			}
		}
	}
	return suppressStaleWaitRepliesTx(ctx, tx)
}

func appendOpenReplyTaskGroupsTx(ctx context.Context, tx *sql.Tx, taskID int64, groupIDs *[]int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT rt.group_id FROM personal_reply_tasks rt
		JOIN personal_reply_groups g ON g.id=rt.group_id WHERE rt.task_id=? AND g.closed=0 ORDER BY rt.group_id`, taskID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var groupID int64
		if err := rows.Scan(&groupID); err != nil {
			rows.Close()
			return err
		}
		found := false
		for _, id := range *groupIDs {
			if id == groupID {
				found = true
				break
			}
		}
		if !found {
			*groupIDs = append(*groupIDs, groupID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

func appendReplyPartsTx(ctx context.Context, tx *sql.Tx, eventID int64, text string, attachments []channel.Attachment, chunks []string, scheduleID int64) (int, error) {
	var part int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(part),0) FROM personal_outbox WHERE event_id=?`, eventID).Scan(&part); err != nil {
		return 0, err
	}
	if text != "" || len(attachments) > 0 {
		if attachments == nil {
			attachments = []channel.Attachment{}
		}
		attachmentJSON, err := json.Marshal(attachments)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_history(event_id,role,text,attachments,schedule_id) VALUES(?,'assistant',?,?,?)`,
			eventID, text, string(attachmentJSON), nullableID(scheduleID)); err != nil {
			return 0, err
		}
	}
	now := timestamp(time.Now().UTC())
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		part++
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_outbox(event_id,part,text,status,next_attempt_at) VALUES(?,?,?,'pending',?)`, eventID, part, chunk, now); err != nil {
			return 0, err
		}
	}
	for _, attachment := range attachments {
		part++
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_outbox(event_id,part,text,attachment_id,status,next_attempt_at) VALUES(?,?,?,?,'pending',?)`,
			eventID, part, "", attachment.ID, now); err != nil {
			return 0, err
		}
	}
	return part, nil
}
