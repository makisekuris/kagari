package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"kagari/internal/textpage"
)

func progressVersion(ctx context.Context, q attachmentQuery, taskID int64) (int64, error) {
	var v int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM personal_progress WHERE task_id=?`, taskID).Scan(&v)
	return v, err
}
func applyProgressTx(ctx context.Context, tx *sql.Tx, run Run, eventID, expected int64, updates []ProgressUpdate, attachments map[string]bool) error {
	if len(updates) == 0 {
		return nil
	}
	if err := validateProgressUpdates(updates); err != nil {
		return err
	}
	v, err := progressVersion(ctx, tx, run.TaskID)
	if err != nil {
		return err
	}
	if expected >= 0 && v != expected {
		return fmt.Errorf("progress_conflict: current version is %d", v)
	}
	for _, u := range updates {
		for _, ref := range u.Evidence {
			switch ref.Kind {
			case "material":
				var sourceRun int64
				var kind string
				if err := tx.QueryRowContext(ctx, `SELECT run_id,kind FROM personal_materials WHERE id=?`, ref.ID).Scan(&sourceRun, &kind); err != nil {
					return err
				}
				if kind != "source" {
					return errors.New("personal: tool records are not source evidence")
				}
				if eventID == 0 {
					if err := authorizeRead(ctx, tx, ReadScope{RunID: run.ID}, sourceRun, false); err != nil {
						return err
					}
				} else {
					if err := progressRunAuthorized(ctx, tx, run, sourceRun); err != nil {
						return err
					}
				}
			case "attachment":
				if !attachments[ref.ID] {
					return errors.New("personal: progress attachment is not associated with this run")
				}
				var status string
				if err := tx.QueryRowContext(ctx, `SELECT status FROM personal_attachments WHERE id=?`, ref.ID).Scan(&status); err != nil {
					return err
				}
				if status != "available" {
					return fmt.Errorf("%w: %s", ErrAttachmentUnavailable, ref.ID)
				}
			case "run_result":
				id, err := strconv.ParseInt(ref.ID, 10, 64)
				if err != nil {
					return err
				}
				if err = progressRunAuthorized(ctx, tx, run, id); err != nil {
					return err
				}
			default:
				return errors.New("personal: unsupported progress evidence")
			}
		}
		if u.Supersedes != "" {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_progress WHERE task_id=? AND task_revision=? AND entry_id=?`, run.TaskID, run.Revision, u.Supersedes).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return errors.New("personal: superseded progress does not exist")
			}
		}
		raw, err := json.Marshal(u)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO personal_progress(task_id,task_revision,version,entry_id,run_id,event_id,update_json) VALUES(?,?,?,?,?,?,?)`, run.TaskID, run.Revision, v+1, u.ID, run.ID, nullableID(eventID), string(raw)); err != nil {
			return err
		}
	}
	return nil
}
func progressRunAuthorized(ctx context.Context, q attachmentQuery, run Run, id int64) error {
	var owner int64
	if err := q.QueryRowContext(ctx, `SELECT task_id FROM personal_runs WHERE id=?`, id).Scan(&owner); err != nil {
		return err
	}
	if owner == run.TaskID {
		return nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_task_run_refs WHERE task_id=? AND revision=? AND source_run_id=?`, run.TaskID, run.Revision, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("personal: evidence run is not associated")
	}
	return nil
}
func (s *Store) TaskProgress(ctx context.Context, scope ReadScope, taskID int64, p ProgressQuery) (TaskProgress, error) {
	out := TaskProgress{FormatVersion: 1, Entries: []ProgressEntry{}}
	key, err := queryPageKey(p.ListPage, struct {
		TaskID  int64
		History bool
	}{taskID, p.History})
	if err != nil {
		return out, err
	}
	out.QueryKey = key
	limit, err := validateList(p.ListPage)
	if err != nil {
		return out, err
	}
	if err = authorizeTaskRead(ctx, s.db, scope, taskID); err != nil {
		return out, err
	}
	out.Version, err = progressVersion(ctx, s.db, taskID)
	if err != nil {
		return out, err
	}
	snapshot := p.SnapshotMaxID
	if snapshot == 0 {
		if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM personal_progress WHERE task_id=?`, taskID).Scan(&snapshot); err != nil {
			return out, err
		}
	}
	out.SnapshotMaxID = snapshot
	filter := `p.task_id=? AND p.seq<=? AND (?=0 OR p.seq<?)`
	args := []any{taskID, snapshot, p.BeforeID, p.BeforeID}
	if !p.History {
		filter += ` AND NOT EXISTS(SELECT 1 FROM personal_progress newer WHERE newer.task_id=p.task_id AND newer.task_revision=p.task_revision AND newer.entry_id=p.entry_id AND newer.seq>p.seq AND newer.seq<=?)`
		args = append(args, snapshot)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT p.seq,p.task_revision,p.version,p.entry_id,p.run_id,p.event_id,p.update_json FROM personal_progress p WHERE `+filter+` ORDER BY p.seq DESC LIMIT ?`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		e := ProgressEntry{TaskID: taskID}
		var event sql.NullInt64
		var raw string
		if err = rows.Scan(&e.Seq, &e.TaskRevision, &e.Version, &e.ID, &e.RunID, &event, &raw); err != nil {
			return out, err
		}
		e.EventID = event.Int64
		if err = json.Unmarshal([]byte(raw), &e.ProgressUpdate); err != nil {
			return out, err
		}
		out.Entries = append(out.Entries, e)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Entries) > limit {
		out.HasMore = true
		out.Entries = out.Entries[:limit]
		out.NextBeforeID = out.Entries[len(out.Entries)-1].Seq
	}
	return out, nil
}

func (s *Store) ReadProgress(ctx context.Context, scope ReadScope, taskID, seq int64, req textpage.Request) (ProgressText, error) {
	out := ProgressText{TaskID: taskID, Seq: seq}
	if taskID <= 0 || seq <= 0 {
		return out, errors.New("personal: task ID and progress sequence must be positive")
	}
	if err := authorizeTaskRead(ctx, s.db, scope, taskID); err != nil {
		return out, err
	}
	entry := ProgressEntry{TaskID: taskID}
	var event sql.NullInt64
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT seq,task_revision,version,entry_id,run_id,event_id,update_json FROM personal_progress WHERE task_id=? AND seq=?`, taskID, seq).
		Scan(&entry.Seq, &entry.TaskRevision, &entry.Version, &entry.ID, &entry.RunID, &event, &raw)
	if err != nil {
		return out, err
	}
	entry.EventID = event.Int64
	if err := json.Unmarshal([]byte(raw), &entry.ProgressUpdate); err != nil {
		return out, err
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return out, err
	}
	out.Page, err = textpage.Read(string(body), req, false)
	if err != nil {
		return out, err
	}
	if _, err = textpage.EncodeBounded(&out, textpage.MaxBytes(ctx), out.Page.Shrink); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Store) CorrectProgress(ctx context.Context, eventID, taskID, revision, expected int64, updates []ProgressUpdate) (TaskProgress, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskProgress{}, err
	}
	defer tx.Rollback()
	kind, _, err := eventState(ctx, tx, eventID)
	if err != nil {
		return TaskProgress{}, err
	}
	if kind == EventUser {
		err = requireUserEvent(ctx, tx, eventID, false)
	} else {
		err = requireCallback(ctx, tx, eventID, taskID, revision)
	}
	if err != nil {
		return TaskProgress{}, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, taskID))
	if err != nil {
		return TaskProgress{}, err
	}
	if task.Revision != revision {
		return TaskProgress{}, errors.New("personal: stale task revision")
	}
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE task_id=? AND revision=? ORDER BY id DESC LIMIT 1`, taskID, revision))
	if err != nil {
		return TaskProgress{}, err
	}
	allowed := map[string]bool{}
	for _, a := range task.Attachments {
		allowed[a.ID] = true
	}
	var result StageResult
	if run.Result != "" {
		if err = json.Unmarshal([]byte(run.Result), &result); err != nil {
			return TaskProgress{}, err
		}
	}
	for _, a := range result.Attachments {
		allowed[a.ID] = true
	}
	if err = applyProgressTx(ctx, tx, *run, eventID, expected, updates, allowed); err != nil {
		return TaskProgress{}, err
	}
	if err = tx.Commit(); err != nil {
		return TaskProgress{}, err
	}
	return s.TaskProgress(ctx, ReadScope{EventID: eventID}, taskID, ProgressQuery{})
}
