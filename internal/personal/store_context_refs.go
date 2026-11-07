package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

func bindTaskReferences(ctx context.Context, tx *sql.Tx, task *Task, eventID int64, input *TaskInput) error {
	if input.SourceRunIDs == nil {
		input.SourceRunIDs = append([]int64{}, task.SourceRunIDs...)
	}
	if input.MemoryRefs == nil {
		input.MemoryRefs = append([]string{}, task.MemoryRefs...)
	}
	slices.Sort(input.SourceRunIDs)
	input.SourceRunIDs = slices.Compact(input.SourceRunIDs)
	slices.Sort(input.MemoryRefs)
	input.MemoryRefs = slices.Compact(input.MemoryRefs)
	if len(input.SourceRunIDs) > 50 || len(input.MemoryRefs) > 50 {
		return errors.New("personal: at most 50 source runs and memory keys may be associated")
	}
	for _, id := range input.SourceRunIDs {
		var owner int64
		var status RunStatus
		var kind RunKind
		if err := tx.QueryRowContext(ctx, `SELECT task_id,status,kind FROM personal_runs WHERE id=?`, id).Scan(&owner, &status, &kind); err != nil {
			return err
		}
		if kind != RunTask || owner == task.ID || (status != RunCompleted && status != RunFailed && status != RunCancelled) {
			return errors.New("personal: only a terminated external task run may be associated")
		}
		if !slices.Contains(task.SourceRunIDs, id) {
			if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO personal_task_run_refs(task_id,revision,source_run_id,authorized_event_id) VALUES(?,?,?,?)`, task.ID, task.Revision, id, eventID); err != nil {
			return err
		}
	}
	for _, key := range input.MemoryRefs {
		if strings.TrimSpace(key) == "" {
			return errors.New("personal: empty memory key")
		}
		var deleted bool
		if err := tx.QueryRowContext(ctx, `SELECT deleted FROM personal_memories WHERE key=?`, key).Scan(&deleted); err != nil {
			return err
		}
		// An inherited forgotten key remains an auditable reference, but is never injected.
		if deleted && !slices.Contains(task.MemoryRefs, key) {
			return errors.New("personal: cannot select a forgotten memory")
		}
	}
	runs, err := json.Marshal(input.SourceRunIDs)
	if err != nil {
		return err
	}
	memories, err := json.Marshal(input.MemoryRefs)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE personal_tasks SET source_run_ids=?,memory_refs=? WHERE id=?`, string(runs), string(memories), task.ID)
	task.SourceRunIDs, task.MemoryRefs = input.SourceRunIDs, input.MemoryRefs
	return err
}
