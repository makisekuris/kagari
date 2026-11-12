package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"kagari/internal/personal"
)

// Task and run queries.
func (s *Store) Task(ctx context.Context, id int64) (*personal.Task, error) {
	if id <= 0 {
		return nil, nil
	}
	task, err := scanTaskOrNil(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, id))
	if err != nil || task == nil {
		return task, err
	}
	if err := s.refreshAttachments(ctx, s.db, task.Attachments); err != nil {
		return nil, err
	}
	return task, nil
}

func scanTaskOrNil(row scanner) (*personal.Task, error) {
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return task, err
}

func (s *Store) Tasks(ctx context.Context, limit int) ([]personal.Task, error) {
	if limit <= 0 {
		return []personal.Task{}, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks ORDER BY updated_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]personal.Task, 0, limit)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range tasks {
		if err := s.refreshAttachments(ctx, s.db, tasks[i].Attachments); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

func (s *Store) Run(ctx context.Context, id int64) (*personal.Run, error) {
	if id <= 0 {
		return nil, nil
	}
	run, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || run == nil {
		return run, err
	}
	return run, s.refreshRunAttachments(ctx, s.db, run)
}

func (s *Store) Runs(ctx context.Context, taskID int64, limit int) ([]personal.Run, error) {
	if taskID <= 0 || limit <= 0 {
		return []personal.Run{}, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE task_id=? ORDER BY id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]personal.Run, 0, limit)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range runs {
		if err := s.refreshRunAttachments(ctx, s.db, &runs[i]); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

func (s *Store) ClaimRun(ctx context.Context, kind personal.RunKind) (*personal.Run, error) {
	if kind != personal.RunTask && kind != personal.RunMemoryReview {
		return nil, fmt.Errorf("personal: unknown run kind %q", kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var query string
	if kind == personal.RunTask {
		query = `SELECT r.id FROM personal_runs r JOIN personal_tasks t ON t.id=r.task_id JOIN personal_events e ON e.id=r.event_id
			WHERE r.kind='task' AND r.status='queued' AND e.status='completed' AND t.status='queued' AND t.revision=r.revision
			AND t.used_ms<t.budget_ms AND NOT EXISTS(SELECT 1 FROM personal_runs active WHERE active.task_id=t.id AND active.revision=t.revision AND active.status='running')
			ORDER BY r.id LIMIT 1`
	} else {
		query = `SELECT r.id FROM personal_runs r JOIN personal_events e ON e.id=r.event_id
			WHERE r.kind='memory_review' AND r.status='queued' AND e.status='completed'
			AND NOT EXISTS(SELECT 1 FROM personal_runs active WHERE active.kind='memory_review' AND active.status='running')
			ORDER BY r.id LIMIT 1`
	}
	var id int64
	if err := tx.QueryRowContext(ctx, query).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var taskID int64
	var revision int64
	var remaining int64
	if kind == personal.RunTask {
		if err := tx.QueryRowContext(ctx, `SELECT r.task_id,r.revision,CASE WHEN t.used_ms>=t.budget_ms THEN 0 ELSE t.budget_ms-t.used_ms END FROM personal_runs r JOIN personal_tasks t ON t.id=r.task_id WHERE r.id=?`, id).Scan(&taskID, &revision, &remaining); err != nil {
			return nil, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='running' WHERE id=? AND status='queued'`, id)
	if err != nil {
		return nil, err
	}
	if affected, err := res.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("personal: run claim lost")
	}
	if kind == personal.RunTask {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_tasks SET status='running',updated_at=? WHERE id=? AND revision=? AND status='queued'`, timestamp(time.Now()), taskID, revision); err != nil {
			return nil, err
		}
		if err := claimTaskInputsTx(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.refreshRunAttachments(ctx, tx, run); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	run.RemainingMS = remaining
	return run, nil
}
