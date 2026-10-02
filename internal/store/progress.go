package store

import (
	"context"
	"fmt"
)

// SaveJobProgress 在外部调用前保存输入、调用后保存产物；恢复任务无需丢失快照。
func (s *Store) SaveJobProgress(ctx context.Context, id int64, result []byte) error {
	if err := validJSON(result); err != nil {
		return err
	}
	updated, err := s.db.ExecContext(ctx, `UPDATE jobs SET result=? WHERE id=? AND status='processing'`, result, id)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("store: job %d is missing or is not processing", id)
	}
	return nil
}
