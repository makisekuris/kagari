package sqlite

import (
	"context"
	"database/sql"

	"kagari/internal/personal"
)

// Keep the last successful mutation per target, including an update or cancellation
// made later in the same turn. Replayed requests reuse the original row.
func (s *Store) EventTimingReceipts(ctx context.Context, eventID int64) ([]personal.TimingReceipt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT reminder, json_extract(result_json,'$.id'),
		json_extract(result_json,'$.title'),json_extract(result_json,'$.status'),
		json_extract(result_json,'$.next_run_at'),COALESCE(json_extract(result_json,'$.timezone'),'')
		FROM (
			SELECT 0 AS reminder,result_json FROM personal_requests WHERE rowid IN
			(SELECT MAX(rowid) FROM personal_requests WHERE event_id=? AND operation IN
			('create_schedule','update_schedule','cancel_schedule') GROUP BY schedule_id)
			UNION ALL
			SELECT 1,result_json FROM personal_reminder_requests WHERE rowid IN
			(SELECT MAX(rowid) FROM personal_reminder_requests WHERE event_id=? GROUP BY reminder_id)
		) ORDER BY reminder,2`, eventID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var receipts []personal.TimingReceipt
	for rows.Next() {
		var receipt personal.TimingReceipt
		var next sql.NullString
		if err := rows.Scan(&receipt.Reminder, &receipt.ID, &receipt.Title, &receipt.Status, &next, &receipt.Timezone); err != nil {
			return nil, err
		}
		receipt.Next, err = scanOptionalTime(next)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}
