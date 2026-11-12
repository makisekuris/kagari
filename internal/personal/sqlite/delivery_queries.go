package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"kagari/internal/personal"
)

func requireDeliveryReadTx(ctx context.Context, tx *sql.Tx, scope personal.ReadScope) error {
	if scope.EventID <= 0 || scope.RunID != 0 {
		return errors.New("personal: delivery queries require a direct user event")
	}
	return requireUserEvent(ctx, tx, scope.EventID, false)
}

func (s *Store) Delivery(ctx context.Context, scope personal.ReadScope, id int64) (*personal.DeliveryIntent, error) {
	if id <= 0 {
		return nil, errors.New("personal: delivery ID must be positive")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireDeliveryReadTx(ctx, tx, scope); err != nil {
		return nil, err
	}
	intents, err := loadDeliverySnapshotsTx(ctx, tx, `SELECT `+deliveryIntentColumns+` FROM personal_delivery_intents WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(intents) == 0 {
		return nil, personal.ErrRecordNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &intents[0], nil
}

func (s *Store) QueryDeliveries(ctx context.Context, scope personal.ReadScope, query personal.DeliveryQuery) (personal.DeliveryPage, error) {
	page := personal.DeliveryPage{Items: []personal.DeliveryIntent{}, StoppedTaskIDs: []int64{}}
	if query.TaskID < 0 || query.BeforeID < 0 || query.Limit < 0 || query.Limit > personal.MaxDeliveryPageSize {
		return page, errors.New("personal: invalid delivery page")
	}
	limit := query.Limit
	if limit == 0 {
		limit = personal.DefaultDeliveryPageSize
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err := requireDeliveryReadTx(ctx, tx, scope); err != nil {
		return page, err
	}
	if query.TaskID > 0 {
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM personal_tasks WHERE id=?`, query.TaskID).Scan(&id); err != nil {
			return page, recordNotFound(err)
		}
	}
	page.Items, err = loadDeliverySnapshotsTx(ctx, tx, `SELECT `+deliveryIntentColumns+` FROM personal_delivery_intents
		WHERE (?=0 OR id<?) AND (?=0 OR EXISTS(SELECT 1 FROM personal_delivery_scopes s
			WHERE s.delivery_id=personal_delivery_intents.id AND s.task_id=?))
		ORDER BY id DESC LIMIT ?`, query.BeforeID, query.BeforeID, query.TaskID, query.TaskID, limit+1)
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextBeforeID = page.Items[len(page.Items)-1].ID
	}
	// Report gates for this page's Tasks and the requested filter even when the
	// cursor yields no rows. A global list must not append every historical gate.
	taskIDs := make(map[int64]struct{})
	if query.TaskID > 0 {
		taskIDs[query.TaskID] = struct{}{}
	}
	for _, intent := range page.Items {
		for _, itemScope := range intent.Scopes {
			taskIDs[itemScope.TaskID] = struct{}{}
		}
	}
	for taskID := range taskIDs {
		stopped, err := taskDeliveryStoppedTx(ctx, tx, taskID)
		if err != nil {
			return page, err
		}
		if stopped {
			page.StoppedTaskIDs = append(page.StoppedTaskIDs, taskID)
		}
	}
	sort.Slice(page.StoppedTaskIDs, func(i, j int) bool { return page.StoppedTaskIDs[i] < page.StoppedTaskIDs[j] })
	if err := tx.Commit(); err != nil {
		return page, err
	}
	return page, nil
}
