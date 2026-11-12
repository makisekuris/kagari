package sqlite

import (
	"context"

	"kagari/internal/personal"
)

// ReferencedTasks preserves ambiguity when one reply covers several tasks.
func (s *Store) ReferencedTasks(ctx context.Context, eventID int64) ([]personal.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id IN (
		SELECT s.task_id FROM personal_events e JOIN personal_outbox d ON d.id=e.reply_outbox_id
		JOIN personal_delivery_scopes s ON s.delivery_id=d.delivery_id WHERE e.id=?
		UNION
		SELECT rt.task_id FROM personal_reply_tasks rt
		JOIN personal_reply_inputs ri ON ri.group_id=rt.group_id
		JOIN personal_events source ON source.id=ri.event_id
		JOIN personal_events e ON e.id=?
		WHERE e.reply_outbox_id IS NULL AND source.channel=e.channel AND source.account=e.account AND source.sender=e.sender AND source.message_id=e.reply_to_id AND e.reply_to_id<>''
		) ORDER BY id`, eventID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []personal.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, *task)
	}
	return tasks, rows.Err()
}
