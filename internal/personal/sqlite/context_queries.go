package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"kagari/internal/personal"
	"kagari/internal/textpage"
)

func (s *Store) RepliedMessage(ctx context.Context, id int64) (*personal.Message, error) {
	m := &personal.Message{OutboxID: id, Role: "assistant"}
	var task, schedule sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT o.event_id,o.text,e.task_id,e.schedule_id,COALESCE((SELECT id FROM personal_history h WHERE h.event_id=e.id AND h.role='assistant' ORDER BY h.id LIMIT 1),0) FROM personal_outbox o JOIN personal_events e ON e.id=o.event_id WHERE o.id=? AND o.status='sent'`, id).Scan(&m.EventID, &m.Text, &task, &schedule, &m.HistoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	m.TaskID, m.ScheduleID = task.Int64, schedule.Int64
	return m, err
}

func authorizeTaskRead(ctx context.Context, q attachmentQuery, scope personal.ReadScope, id int64) error {
	if scope.RunID > 0 {
		r, err := activeRunTx(ctx, q, scope.RunID)
		if err != nil {
			return err
		}
		if r.TaskID != id {
			return errors.New("personal: only the current task may be read")
		}
	}
	return authorizeRead(ctx, q, scope, scope.RunID, true)
}
func (s *Store) QueryTasks(ctx context.Context, scope personal.ReadScope, p personal.TaskQuery) (personal.TaskPage, error) {
	var out personal.TaskPage
	key, err := queryPageKey(p.ListPage, struct {
		Query    string
		From, To *time.Time
	}{p.Query, p.UpdatedFrom, p.UpdatedTo})
	if err != nil {
		return out, err
	}
	out.QueryKey = key
	limit, err := validateList(p.ListPage)
	if err != nil {
		return out, err
	}
	if scope.RunID > 0 || utf8.RuneCountInString(p.Query) > 256 {
		return out, errors.New("personal: task search requires conversation scope and a bounded query")
	}
	if err = authorizeRead(ctx, s.db, scope, 0, true); err != nil {
		return out, recordNotFound(err)
	}
	if p.UpdatedFrom != nil && p.UpdatedTo != nil && !p.UpdatedFrom.Before(*p.UpdatedTo) {
		return out, errors.New("personal: invalid updated range")
	}
	maxID := p.SnapshotMaxID
	if maxID == 0 {
		if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM personal_tasks`).Scan(&maxID); err != nil {
			return out, err
		}
	}
	filter := `id<=? AND (?=0 OR id<?) AND (?='' OR instr(title,?)>0 OR instr(goal,?)>0 OR instr(context,?)>0 OR instr(summary,?)>0)`
	args := []any{maxID, p.BeforeID, p.BeforeID, p.Query, p.Query, p.Query, p.Query, p.Query}
	if p.UpdatedFrom != nil {
		filter += ` AND updated_at>=?`
		args = append(args, timestamp(*p.UpdatedFrom))
	}
	if p.UpdatedTo != nil {
		filter += ` AND updated_at<?`
		args = append(args, timestamp(*p.UpdatedTo))
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE `+filter+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.SnapshotMaxID = maxID
	out.Tasks = []personal.Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return out, err
		}
		out.Tasks = append(out.Tasks, personal.TaskCard(*task))
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Tasks) > limit {
		out.HasMore = true
		out.Tasks = out.Tasks[:limit]
	}
	if out.HasMore {
		out.NextBeforeID = out.Tasks[len(out.Tasks)-1].ID
	}
	return out, nil
}
func (s *Store) QueryRuns(ctx context.Context, scope personal.ReadScope, taskID int64, p personal.ListPage) (personal.RunPage, error) {
	var out personal.RunPage
	key, err := queryPageKey(p, struct{ TaskID int64 }{taskID})
	if err != nil {
		return out, err
	}
	out.QueryKey = key
	limit, err := validateList(p)
	if err != nil {
		return out, err
	}
	if err = authorizeTaskRead(ctx, s.db, scope, taskID); err != nil {
		return out, recordNotFound(err)
	}
	maxID := p.SnapshotMaxID
	if maxID == 0 {
		if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM personal_runs WHERE task_id=?`, taskID).Scan(&maxID); err != nil {
			return out, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE task_id=? AND id<=? AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, taskID, maxID, p.BeforeID, p.BeforeID, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.SnapshotMaxID = maxID
	out.Runs = []personal.Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return out, err
		}
		out.Runs = append(out.Runs, personal.RunCard(*run))
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Runs) > limit {
		out.HasMore = true
		out.Runs = out.Runs[:limit]
	}
	if out.HasMore {
		out.NextBeforeID = out.Runs[len(out.Runs)-1].ID
	}
	return out, nil
}

func queryPageKey(p personal.ListPage, query any) (string, error) {
	raw, err := json.Marshal(query)
	if err != nil {
		return "", err
	}
	key := textpage.Version(string(raw))
	if (p.BeforeID > 0 || p.SnapshotMaxID > 0) && p.QueryKey != key {
		return "", fmt.Errorf("%w: query changed; start from the first page", textpage.ErrInvalidPage)
	}
	return key, nil
}
func (s *Store) ReadRecord(ctx context.Context, scope personal.ReadScope, r personal.RecordRequest) (personal.RecordText, error) {
	out := personal.RecordText{Kind: r.Kind, ID: r.ID, Section: r.Section}
	var body string
	var err error
	mutable := false
	if r.ID <= 0 {
		return out, errors.New("personal: record ID must be positive")
	}
	switch r.Kind {
	case "task":
		if err = authorizeTaskRead(ctx, s.db, scope, r.ID); err != nil {
			return out, recordNotFound(err)
		}
		if r.Section != "goal" && r.Section != "context" && r.Section != "checkpoint" {
			return out, errors.New("personal: unsupported task section")
		}
		err = s.db.QueryRowContext(ctx, `SELECT `+r.Section+`,revision FROM personal_tasks WHERE id=?`, r.ID).Scan(&body, &out.Revision)
		mutable = true
	case "run":
		if r.Section != "input" && r.Section != "result" && r.Section != "context_manifest" {
			return out, errors.New("personal: unsupported run section")
		}
		if err = authorizeRead(ctx, s.db, scope, r.ID, r.Section != "result"); err != nil {
			return out, recordNotFound(err)
		}
		err = s.db.QueryRowContext(ctx, `SELECT `+r.Section+`,revision FROM personal_runs WHERE id=?`, r.ID).Scan(&body, &out.Revision)
		mutable = true
	case "event", "message":
		if scope.RunID > 0 {
			return out, errors.New("personal: raw conversation is not task-authorized")
		}
		if err = authorizeRead(ctx, s.db, scope, 0, true); err != nil {
			return out, recordNotFound(err)
		}
		table := "personal_events"
		if r.Kind == "message" {
			table = "personal_history"
		}
		err = s.db.QueryRowContext(ctx, `SELECT text FROM `+table+` WHERE id=?`, r.ID).Scan(&body)
	default:
		return out, errors.New("personal: unknown record kind")
	}
	if err != nil {
		return out, recordNotFound(err)
	}
	version := textpage.Version(body)
	if r.Kind == "task" {
		version = fmt.Sprintf("%d:%s", out.Revision, version)
	}
	req := r.Request
	if mutable && req.Offset > 0 && req.Version == "" {
		return out, textpage.ErrVersionRequired
	}
	if req.Version != "" && req.Version != version {
		return out, &textpage.VersionChangedError{Version: version}
	}
	req.Version = ""
	out.Page, err = textpage.Read(body, req, false)
	out.Page.Version = version
	return out, err
}
