package personal

import (
	"context"
	"database/sql"
)

// A terminal Run cannot keep input assigned; only a successful stage marks it applied.
func releaseTerminalTaskInputsTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE personal_task_inputs SET status='pending',run_id=NULL
		WHERE status='assigned' AND EXISTS(SELECT 1 FROM personal_runs r
		WHERE r.id=personal_task_inputs.run_id AND r.status IN ('cancelled','failed','completed'))`)
	return err
}
