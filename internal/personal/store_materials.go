package personal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"kagari/internal/domain"
	"kagari/internal/textpage"
)

type sourceMetadata struct {
	FormatVersion int           `json:"format_version"`
	RequestedURL  string        `json:"requested_url"`
	URL           string        `json:"url"`
	Kind          string        `json:"kind"`
	Title         string        `json:"title"`
	Author        string        `json:"author"`
	PublishedAt   *time.Time    `json:"published_at,omitempty"`
	Links         []domain.Link `json:"links"`
	Status        string        `json:"status"`
	Reason        string        `json:"reason"`
	ReadMethod    string        `json:"read_method"`
	Truncated     bool          `json:"truncated"`
}

type storedMaterial struct {
	Material
	body, metadata string
	chars          int
}

func (s *Store) SaveToolRecord(ctx context.Context, scope ReadScope, key, body string, names []string) (string, error) {
	if !requestKeyValid(key) || !utf8.ValidString(body) || len(body) > 40<<20 || runeLen(body) > 10<<20 {
		return "", errors.New("material_too_large")
	}
	for _, name := range names {
		if strings.HasPrefix(name, "memory_") || (name == "read_attachment" || name == "write_attachment") {
			return "", errors.New("personal: memory and attachment bodies cannot be copied into tool records")
		}
	}
	var messages []*schema.AgenticMessage
	if err := json.Unmarshal([]byte(body), &messages); err != nil {
		return "", err
	}
	var callIDs []string
	for _, message := range messages {
		for _, block := range message.ContentBlocks {
			if call := block.FunctionToolCall; call != nil {
				callIDs = append(callIDs, call.CallID)
			}
		}
	}
	raw, err := json.Marshal(struct {
		FormatVersion int      `json:"format_version"`
		RecordType    string   `json:"record_type"`
		Tools         []string `json:"tools"`
		ToolCallIDs   []string `json:"tool_call_ids"`
	}{1, "tool_round", names, callIDs})
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	eventID, runID, taskID := scope.EventID, scope.RunID, int64(0)
	if (eventID > 0) == (runID > 0) {
		return "", errors.New("personal: exactly one tool-record scope required")
	}
	if runID > 0 {
		r, err := activeRunTx(ctx, tx, runID)
		if err != nil {
			return "", err
		}
		eventID, taskID = r.EventID, r.TaskID
	} else {
		if err = authorizeRead(ctx, tx, scope, 0, true); err != nil {
			return "", err
		}
	}
	var id, oldBody, metadata string
	err = tx.QueryRowContext(ctx, `SELECT id,body,metadata FROM personal_materials WHERE event_id=? AND COALESCE(run_id,0)=? AND capture_key=?`, eventID, runID, key).Scan(&id, &oldBody, &metadata)
	if err == nil {
		if oldBody != body || metadata != string(raw) {
			return "", errors.New("capture_conflict")
		}
		return id, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return "", err
	}
	id = "m_" + hex.EncodeToString(random[:])
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_materials(id,event_id,run_id,task_id,kind,capture_key,body,metadata,content_sha256,byte_count,char_count,collected_at) VALUES(?,?,?,?,'tool_record',?,?,?,?,?,?,?)`, id, eventID, nullableID(runID), nullableID(taskID), key, body, string(raw), textpage.Version(body), len(body), runeLen(body), timestamp(time.Now()))
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func activeRunTx(ctx context.Context, q attachmentQuery, id int64) (*Run, error) {
	r, err := scanRun(q.QueryRowContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	var revision int64
	var status TaskStatus
	if r.Kind != RunTask || r.Status != RunRunning {
		return nil, errors.New("personal: active task run required")
	}
	if err = q.QueryRowContext(ctx, `SELECT revision,status FROM personal_tasks WHERE id=?`, r.TaskID).Scan(&revision, &status); err != nil {
		return nil, err
	}
	if revision != r.Revision || status != TaskRunning {
		return nil, errors.New("personal: stale task run")
	}
	return r, nil
}

func authorizeRead(ctx context.Context, q attachmentQuery, scope ReadScope, targetRun int64, private bool) error {
	if (scope.EventID > 0) == (scope.RunID > 0) {
		return errors.New("personal: exactly one read scope is required")
	}
	if scope.EventID > 0 {
		var status EventStatus
		if err := q.QueryRowContext(ctx, `SELECT status FROM personal_events WHERE id=?`, scope.EventID).Scan(&status); err != nil {
			return err
		}
		if status != EventRunning {
			return errors.New("personal: read requires an active event")
		}
		return nil
	}
	r, err := activeRunTx(ctx, q, scope.RunID)
	if err != nil {
		return err
	}
	var taskID sql.NullInt64
	if err = q.QueryRowContext(ctx, `SELECT task_id FROM personal_runs WHERE id=?`, targetRun).Scan(&taskID); err != nil {
		return err
	}
	if taskID.Int64 == r.TaskID {
		return nil
	}
	if private {
		return errors.New("personal: external run private data is not authorized")
	}
	var n int
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_task_run_refs WHERE task_id=? AND revision=? AND source_run_id=?`, r.TaskID, r.Revision, targetRun).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("personal: run is not explicitly associated with this task")
	}
	return nil
}

func (s *Store) SaveSource(ctx context.Context, runID int64, key string, src domain.Source) (domain.SourceRef, error) {
	if !requestKeyValid(key) {
		return domain.SourceRef{}, errors.New("personal: capture key required")
	}
	if !utf8.ValidString(src.Content) || len(src.Content) > 40<<20 || utf8.RuneCountInString(src.Content) > 10<<20 {
		return domain.SourceRef{}, errors.New("material_too_large: invalid UTF-8 or text limit exceeded")
	}
	meta := sourceMetadata{1, src.RequestedURL, src.URL, src.Kind, src.Title, src.Author, src.PublishedAt, src.Links, src.Status, src.Reason, src.ReadMethod, src.Truncated}
	raw, err := json.Marshal(meta)
	if err != nil {
		return domain.SourceRef{}, err
	}
	if len(raw) > 64<<10 || src.Status == "" || src.FetchedAt.IsZero() {
		return domain.SourceRef{}, errors.New("personal: source metadata is invalid or too large")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SourceRef{}, err
	}
	defer tx.Rollback()
	r, err := activeRunTx(ctx, tx, runID)
	if err != nil {
		return domain.SourceRef{}, err
	}
	old, err := scanMaterial(tx.QueryRowContext(ctx, `SELECT `+materialColumns+` FROM personal_materials WHERE event_id=? AND run_id=? AND capture_key=?`, r.EventID, runID, key))
	if err == nil {
		if old.Kind != "source" || old.TaskID != r.TaskID || old.body != src.Content || old.metadata != string(raw) || !old.CollectedAt.Equal(src.FetchedAt) {
			return domain.SourceRef{}, errors.New("capture_conflict")
		}
		return *old.Source, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.SourceRef{}, err
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return domain.SourceRef{}, err
	}
	id := "m_" + hex.EncodeToString(random[:])
	chars := utf8.RuneCountInString(src.Content)
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_materials(id,event_id,run_id,task_id,kind,capture_key,body,metadata,content_sha256,byte_count,char_count,collected_at) VALUES(?,?,?,?,'source',?,?,?,?,?,?,?)`, id, r.EventID, runID, r.TaskID, key, src.Content, string(raw), textpage.Version(src.Content), len(src.Content), chars, timestamp(src.FetchedAt))
	if err != nil {
		return domain.SourceRef{}, err
	}
	ref := domain.SourceRef{ID: id, RunID: runID, URL: src.URL, Title: src.Title, Status: src.Status, FetchedAt: src.FetchedAt.UTC(), SourceTruncated: src.Truncated, TotalRunes: chars}
	return ref, tx.Commit()
}

const materialColumns = `seq,id,event_id,run_id,task_id,kind,content_sha256,collected_at,body,metadata,char_count`

func scanMaterial(row scanner) (storedMaterial, error) {
	var m storedMaterial
	var run, task sql.NullInt64
	var collected string
	err := row.Scan(&m.Seq, &m.ID, &m.EventID, &run, &task, &m.Kind, &m.Version, &collected, &m.body, &m.metadata, &m.chars)
	if err != nil {
		return m, err
	}
	m.RunID, m.TaskID = run.Int64, task.Int64
	m.CollectedAt, err = parseTimestamp(collected)
	if err != nil {
		return m, err
	}
	if m.Kind == "source" {
		var meta sourceMetadata
		if err = json.Unmarshal([]byte(m.metadata), &meta); err != nil {
			return m, err
		}
		if meta.FormatVersion != 1 {
			return m, errors.New("personal: unsupported material format")
		}
		m.Source = &domain.SourceRef{ID: m.ID, RunID: m.RunID, URL: meta.URL, Title: meta.Title, Status: meta.Status, FetchedAt: m.CollectedAt, SourceTruncated: meta.Truncated, TotalRunes: m.chars}
	}
	return m, nil
}

func validateList(p ListPage) (int, error) {
	if p.Limit < 0 || p.Limit > 50 || p.BeforeID < 0 || p.SnapshotMaxID < 0 {
		return 0, textpage.ErrInvalidPage
	}
	if p.Limit == 0 {
		return 20, nil
	}
	return p.Limit, nil
}
func (s *Store) ListMaterials(ctx context.Context, scope ReadScope, runID int64, p ListPage) (MaterialPage, error) {
	var out MaterialPage
	key, err := queryPageKey(p, struct{ RunID int64 }{runID})
	if err != nil {
		return out, err
	}
	out.QueryKey = key
	limit, err := validateList(p)
	if err != nil {
		return out, err
	}
	if err = authorizeRead(ctx, s.db, scope, runID, false); err != nil {
		return out, err
	}
	private := authorizeRead(ctx, s.db, scope, runID, true) == nil
	maxID := p.SnapshotMaxID
	if maxID == 0 {
		if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM personal_materials WHERE run_id=?`, runID).Scan(&maxID); err != nil {
			return out, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+materialColumns+` FROM personal_materials WHERE run_id=? AND seq<=? AND (?=0 OR seq<?) AND (? OR kind='source') ORDER BY seq DESC LIMIT ?`, runID, maxID, p.BeforeID, p.BeforeID, private, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.SnapshotMaxID = maxID
	out.Items = []Material{}
	for rows.Next() {
		m, err := scanMaterial(rows)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, materialCard(m.Material))
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > limit {
		out.HasMore = true
		out.Items = out.Items[:limit]
	}
	if out.HasMore {
		out.NextBeforeID = out.Items[len(out.Items)-1].Seq
	}
	return out, nil
}
func (s *Store) materialForRead(ctx context.Context, scope ReadScope, id string) (storedMaterial, error) {
	m, err := scanMaterial(s.db.QueryRowContext(ctx, `SELECT `+materialColumns+` FROM personal_materials WHERE id=?`, id))
	if err != nil {
		return m, err
	}
	if m.RunID == 0 {
		if scope.RunID > 0 || scope.EventID != m.EventID {
			return m, errors.New("personal: tool record belongs to another event")
		}
	}
	return m, authorizeRead(ctx, s.db, scope, m.RunID, m.Kind == "tool_record")
}
func (s *Store) ReadMaterial(ctx context.Context, scope ReadScope, id, part string, p textpage.Request) (MaterialText, error) {
	m, err := s.materialForRead(ctx, scope, id)
	if err != nil {
		return MaterialText{}, err
	}
	body := m.body
	if part == "metadata" {
		body = m.metadata
	} else if part != "" && part != "body" {
		return MaterialText{}, errors.New("personal: part must be body or metadata")
	}
	page, err := textpage.Read(body, p, false)
	if err != nil {
		return MaterialText{}, err
	}
	if m.Source != nil {
		page.SourceTruncated = m.Source.SourceTruncated
	}
	return MaterialText{Material: materialCard(m.Material), Page: page}, nil
}
func (s *Store) SearchMaterial(ctx context.Context, scope ReadScope, id, query string, p SearchPage) (MatchPage, error) {
	var out MatchPage
	if query == "" || utf8.RuneCountInString(query) > 256 || p.Offset < 0 {
		return out, textpage.ErrInvalidPage
	}
	limit, err := validateList(ListPage{Limit: p.Limit})
	if err != nil {
		return out, err
	}
	m, err := s.materialForRead(ctx, scope, id)
	if err != nil {
		return out, err
	}
	out.Material = materialCard(m.Material)
	out.Matches = []TextMatch{}
	runes, needle := []rune(m.body), []rune(query)
	if p.Offset > len(runes) {
		return out, textpage.ErrInvalidPage
	}
	pos := p.Offset
	for pos < len(runes) {
		idx := strings.Index(string(runes[pos:]), query)
		if idx < 0 {
			pos = len(runes)
			break
		}
		start := pos + utf8.RuneCountInString(string(runes[pos:])[:idx])
		end := start + len(needle)
		from, to := max(0, start-40), min(len(runes), start+120)
		out.Matches = append(out.Matches, TextMatch{start, end, string(runes[from:to])})
		pos = start + 1
		if len(out.Matches) == limit {
			break
		}
	}
	out.NextOffset = pos
	out.HasMore = pos < len(runes)
	return out, nil
}

func materialCard(m Material) Material {
	if m.Source != nil && runeLen(m.Source.Title) > 200 {
		ref := *m.Source
		ref.Title = truncateRunes(ref.Title, 200)
		m.Source = &ref
		m.OmittedFields = []string{"source.title"}
	}
	return m
}

// ExpandSource is a diagnostic read; model callers use scoped paginated methods.
func (s *Store) ExpandSource(ctx context.Context, id string) (domain.Source, error) {
	m, err := scanMaterial(s.db.QueryRowContext(ctx, `SELECT `+materialColumns+` FROM personal_materials WHERE id=? AND kind='source'`, id))
	if err != nil {
		return domain.Source{}, err
	}
	var meta sourceMetadata
	if err = json.Unmarshal([]byte(m.metadata), &meta); err != nil {
		return domain.Source{}, err
	}
	return domain.Source{ID: m.ID, RequestedURL: meta.RequestedURL, URL: meta.URL, Kind: meta.Kind, Title: meta.Title, Author: meta.Author, PublishedAt: meta.PublishedAt, Content: m.body, Links: meta.Links, Status: meta.Status, Reason: meta.Reason, ReadMethod: meta.ReadMethod, FetchedAt: m.CollectedAt, Truncated: meta.Truncated}, nil
}

func (s *Store) SaveReadCheckpoint(ctx context.Context, runID int64, u ReadCheckpointUpdate) (ReadCheckpoint, error) {
	var out ReadCheckpoint
	if strings.TrimSpace(u.Note) == "" || runeLen(u.Note) > 2000 || u.ExpectedOffset < 0 || u.NextOffset < u.ExpectedOffset || u.Version == "" {
		return out, errors.New("personal: invalid reading checkpoint")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	r, err := activeRunTx(ctx, tx, runID)
	if err != nil {
		return out, err
	}
	switch u.ResourceKind {
	case "material":
		m, err := scanMaterial(tx.QueryRowContext(ctx, `SELECT `+materialColumns+` FROM personal_materials WHERE id=?`, u.ResourceID))
		if err != nil {
			return out, err
		}
		if err = authorizeRead(ctx, tx, ReadScope{RunID: runID}, m.RunID, m.Kind == "tool_record"); err != nil {
			return out, err
		}
		if m.Version != u.Version {
			return out, &textpage.VersionChangedError{Version: m.Version}
		}
		if u.NextOffset > m.chars {
			return out, textpage.ErrInvalidPage
		}
	case "attachment":
		if _, err = s.validateStoredAttachment(ctx, tx, u.ResourceID); err != nil {
			return out, err
		}
	case "file":
		// The workspace-bound tool verifies the current file version and returned intervals.
	default:
		return out, errors.New("personal: unsupported checkpoint resource")
	}
	var offset int
	var note string
	err = tx.QueryRowContext(ctx, `SELECT next_offset,note FROM personal_read_checkpoints WHERE task_id=? AND revision=? AND resource_kind=? AND resource_id=? AND version=?`, r.TaskID, r.Revision, u.ResourceKind, u.ResourceID, u.Version).Scan(&offset, &note)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if offset == u.NextOffset && note == u.Note {
		return ReadCheckpoint{u, r.TaskID, r.Revision, runID}, nil
	}
	if offset != u.ExpectedOffset {
		return out, errors.New("checkpoint_conflict")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_read_checkpoints(task_id,revision,resource_kind,resource_id,version,next_offset,note,run_id,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(task_id,revision,resource_kind,resource_id,version) DO UPDATE SET next_offset=excluded.next_offset,note=excluded.note,run_id=excluded.run_id,updated_at=excluded.updated_at`, r.TaskID, r.Revision, u.ResourceKind, u.ResourceID, u.Version, u.NextOffset, u.Note, runID, timestamp(time.Now()))
	if err != nil {
		return out, err
	}
	return ReadCheckpoint{u, r.TaskID, r.Revision, runID}, tx.Commit()
}

func (s *Store) readingCheckpoints(ctx context.Context, run Run) ([]ReadCheckpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource_kind,resource_id,version,next_offset,note,run_id FROM personal_read_checkpoints WHERE task_id=? AND revision=? ORDER BY resource_kind,resource_id`, run.TaskID, run.Revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReadCheckpoint{}
	for rows.Next() {
		p := ReadCheckpoint{TaskID: run.TaskID, Revision: run.Revision}
		if err = rows.Scan(&p.ResourceKind, &p.ResourceID, &p.Version, &p.NextOffset, &p.Note, &p.RunID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
