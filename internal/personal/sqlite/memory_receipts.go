package sqlite

import (
	"context"

	"kagari/internal/personal"
)

// EventMemoryReceipts returns the last memory change per key committed by an
// event. Tombstones deliberately omit their previous value.
func (s *Store) EventMemoryReceipts(ctx context.Context, eventID int64) ([]personal.MemoryReceipt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT memory.key,
		CASE WHEN memory.deleted=1 THEN '' ELSE memory.value END,
		memory.version,memory.deleted
		FROM personal_memory_history AS memory
		JOIN (
			SELECT MAX(history.id) AS id
			FROM personal_requests AS request
			JOIN personal_memory_history AS history ON history.id=request.memory_history_id
			WHERE request.event_id=? AND request.operation='write_memory'
			GROUP BY history.key
		) AS latest ON latest.id=memory.id
		ORDER BY memory.id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	receipts := make([]personal.MemoryReceipt, 0)
	for rows.Next() {
		var receipt personal.MemoryReceipt
		var deleted int
		if err := rows.Scan(&receipt.Key, &receipt.Value, &receipt.Version, &deleted); err != nil {
			return nil, err
		}
		receipt.Deleted = deleted != 0
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}
