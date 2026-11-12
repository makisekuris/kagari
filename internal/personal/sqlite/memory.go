package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"kagari/internal/personal"
)

const maxMemoryKeyChars = 128
const maxMemoryValueChars = 4000
const maxReviewMemories = 100

var errMemoryVersionConflict = errors.New("personal: memory version conflict")

func validMemoryKey(key string) bool {
	if key == "" || len(key) > maxMemoryKeyChars {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func validateMemoryChange(change personal.MemoryChange, sourceText string) error {
	if !validMemoryKey(change.Key) {
		return errors.New("personal: memory key must be lowercase ASCII [a-z0-9_.-] and at most 128 characters")
	}
	switch change.Operation {
	case "put":
		if strings.TrimSpace(change.Value) == "" || utf8.RuneCountInString(change.Value) > maxMemoryValueChars {
			return errors.New("personal: memory value is required and limited to 4000 characters")
		}
		if strings.TrimSpace(change.Evidence) == "" || !strings.Contains(sourceText, change.Evidence) {
			return errors.New("personal: memory put requires an exact quote from its source user event")
		}
		if change.ExpectedVersion < 0 {
			return errors.New("personal: memory expected version cannot be negative")
		}
	case "forget":
		if strings.TrimSpace(change.Evidence) == "" || !strings.Contains(sourceText, change.Evidence) {
			return errors.New("personal: memory forget requires an exact quote from its source user event")
		}
		if change.Value != "" || change.ExpectedVersion <= 0 {
			return errors.New("personal: memory forget requires a positive version and no value")
		}
	default:
		return fmt.Errorf("personal: unsupported memory operation %q", change.Operation)
	}
	return nil
}

// WriteMemory commits an explicit user change and its replay receipt together.
func (s *Store) WriteMemory(ctx context.Context, eventID int64, key string, change personal.MemoryChange) (personal.Memory, error) {
	if !requestKeyValid(key) {
		return personal.Memory{}, errors.New("personal: request key is required")
	}
	payload, err := encodeRequest(requestPayload{Change: &change})
	if err != nil {
		return personal.Memory{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.Memory{}, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.Memory{}, err
	}
	_, _, historyID, found, err := previousRequest(ctx, tx, eventID, key, "write_memory", payload)
	if err != nil {
		return personal.Memory{}, err
	}
	if found {
		// Read the original result before checking a version that this request already advanced.
		memory, err := scanMemory(tx.QueryRowContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memory_history WHERE id=?`, historyID))
		if err != nil {
			return personal.Memory{}, err
		}
		if err := commitStoreOutcome(ctx, tx, "memory.write", "replayed", *memory,
			zap.Int64("event_id", eventID), zap.Int64("memory_history_id", historyID),
			zap.String("memory_key", memory.Key), zap.Int64("version", memory.Version),
			zap.Bool("deleted", memory.Deleted), zap.String("memory_operation", change.Operation)); err != nil {
			return personal.Memory{}, err
		}
		return *memory, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.Memory{}, err
	}
	var sourceText string
	if err := tx.QueryRowContext(ctx, `SELECT text FROM personal_events WHERE id=?`, eventID).Scan(&sourceText); err != nil {
		return personal.Memory{}, err
	}
	memory, historyID, err := applyMemoryChange(ctx, tx, eventID, 0, change, sourceText, false)
	if err != nil {
		return personal.Memory{}, err
	}
	if err := insertRequest(ctx, tx, eventID, key, "write_memory", payload, 0, 0, historyID); err != nil {
		return personal.Memory{}, err
	}
	if err := commitStoreOutcome(ctx, tx, "memory.write", "applied", *memory,
		zap.Int64("event_id", eventID), zap.Int64("memory_history_id", historyID),
		zap.String("memory_key", memory.Key), zap.Int64("version", memory.Version),
		zap.Bool("deleted", memory.Deleted), zap.String("memory_operation", change.Operation)); err != nil {
		return personal.Memory{}, err
	}
	return *memory, nil
}

func memorySource(ctx context.Context, tx *sql.Tx, eventID int64) (string, error) {
	var kind personal.EventKind
	var status personal.EventStatus
	var text string
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,text FROM personal_events WHERE id=?`, eventID).Scan(&kind, &status, &text); err != nil {
		return "", err
	}
	if kind != personal.EventUser || status != personal.EventCompleted {
		return "", errors.New("personal: memory writes require a completed source user event")
	}
	return text, nil
}

func applyMemoryChange(ctx context.Context, tx *sql.Tx, sourceEventID, runID int64, change personal.MemoryChange, sourceText string, automatic bool) (*personal.Memory, int64, error) {
	if err := validateMemoryChange(change, sourceText); err != nil {
		return nil, 0, err
	}
	var previous personal.Memory
	var exists bool
	var deleted int
	var updated string
	err := tx.QueryRowContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memories WHERE key=?`, change.Key).
		Scan(&previous.Key, &previous.Value, &previous.Version, &previous.SourceEventID, &deleted, &updated)
	if err == nil {
		exists = true
		previous.Deleted = deleted != 0
		previous.UpdatedAt, err = parseTimestamp(updated)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, err
	}
	if automatic && exists && previous.SourceEventID >= sourceEventID {
		return nil, 0, nil
	}
	if exists && sourceEventID < previous.SourceEventID {
		return nil, 0, fmt.Errorf("%w: source event is older than %q", errMemoryVersionConflict, change.Key)
	}
	if (exists && change.ExpectedVersion != previous.Version) || (!exists && change.ExpectedVersion != 0) {
		return nil, 0, fmt.Errorf("%w for %q", errMemoryVersionConflict, change.Key)
	}
	if change.Operation == "forget" && exists && previous.Deleted {
		return nil, 0, errors.New("personal: memory is already forgotten")
	}
	version := int64(1)
	if exists {
		if previous.Version == int64(^uint64(0)>>1) {
			return nil, 0, errors.New("personal: memory version exhausted")
		}
		version = previous.Version + 1
	}
	value := change.Value
	deletedFlag := 0
	if change.Operation == "forget" {
		value, deletedFlag = "", 1
	}
	updatedAt := time.Now().UTC()
	now := timestamp(updatedAt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO personal_memories(key,value,version,source_event_id,deleted,updated_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,version=excluded.version,
		source_event_id=excluded.source_event_id,deleted=excluded.deleted,updated_at=excluded.updated_at`,
		change.Key, value, version, sourceEventID, deletedFlag, now); err != nil {
		return nil, 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_memory_history(key,value,version,source_event_id,run_id,deleted,updated_at) VALUES(?,?,?,?,?,?,?)`,
		change.Key, value, version, sourceEventID, nullableID(runID), deletedFlag, now)
	if err != nil {
		return nil, 0, err
	}
	historyID, err := res.LastInsertId()
	if err != nil {
		return nil, 0, err
	}
	return &personal.Memory{Key: change.Key, Value: value, Version: version, SourceEventID: sourceEventID, Deleted: deletedFlag != 0, UpdatedAt: updatedAt}, historyID, nil
}

func (s *Store) MemoryReviewInput(ctx context.Context, runID int64) (personal.MemoryReviewInput, error) {
	var input personal.MemoryReviewInput
	var eventID int64
	var kind personal.RunKind
	var status personal.RunStatus
	if err := s.db.QueryRowContext(ctx, `SELECT event_id,kind,status FROM personal_runs WHERE id=?`, runID).Scan(&eventID, &kind, &status); err != nil {
		return input, err
	}
	if kind != personal.RunMemoryReview || status != personal.RunRunning {
		return input, errors.New("personal: memory review input requires an active review run")
	}
	var sourceKind personal.EventKind
	var eventStatus personal.EventStatus
	if err := s.db.QueryRowContext(ctx, `SELECT kind,status FROM personal_events WHERE id=?`, eventID).Scan(&sourceKind, &eventStatus); err != nil {
		return input, err
	}
	event, err := s.readEvent(ctx, s.db, eventID)
	if err != nil {
		return personal.MemoryReviewInput{}, err
	}
	input.Event = event
	if sourceKind != personal.EventUser || eventStatus != personal.EventCompleted {
		return personal.MemoryReviewInput{}, errors.New("personal: memory review source is not a completed user event")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memories ORDER BY source_event_id DESC,key LIMIT ?`, maxReviewMemories)
	if err != nil {
		return personal.MemoryReviewInput{}, err
	}
	defer rows.Close()
	input.Memories = make([]personal.Memory, 0, maxReviewMemories)
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return personal.MemoryReviewInput{}, err
		}
		input.Memories = append(input.Memories, *m)
	}
	return input, rows.Err()
}

func (s *Store) readEvent(ctx context.Context, db attachmentQuery, id int64) (personal.Event, error) {
	event, err := scanEvent(db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM personal_events WHERE id=?`, id))
	if err != nil {
		return personal.Event{}, err
	}
	if err := s.refreshAttachments(ctx, db, event.Attachments); err != nil {
		return personal.Event{}, err
	}
	return *event, nil
}

func (s *Store) FinishMemoryReview(ctx context.Context, runID int64, review personal.MemoryReview, failure string) error {
	if runID <= 0 {
		return errors.New("personal: run ID must be positive")
	}
	if utf8.RuneCountInString(review.Question) > personal.MaxTaskChars || utf8.RuneCountInString(failure) > personal.MaxTaskChars {
		return errors.New("personal: memory review message exceeds its size limit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var eventID int64
	var kind personal.RunKind
	var status personal.RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT event_id,kind,status FROM personal_runs WHERE id=?`, runID).Scan(&eventID, &kind, &status); err != nil {
		return err
	}
	if kind != personal.RunMemoryReview {
		return errors.New("personal: FinishMemoryReview requires a review run")
	}
	if status == personal.RunCompleted || status == personal.RunFailed || status == personal.RunCancelled {
		return commitStoreOutcome(ctx, tx, "memory.review.finish", "ignored_terminal", nil,
			zap.Int64("event_id", eventID), zap.Int64("run_id", runID),
			zap.String("run_status", string(status)), zap.Int("proposed_count", len(review.Changes)))
	}
	if status != personal.RunRunning {
		return fmt.Errorf("personal: cannot finish memory review %d in state %q", runID, status)
	}
	sourceText, err := memorySource(ctx, tx, eventID)
	if err != nil {
		return err
	}
	failureText := failure
	if failureText == "" && review.Question == "" {
		seen := make(map[string]bool, len(review.Changes))
		for _, change := range review.Changes {
			if err := validateMemoryChange(change, sourceText); err != nil {
				failureText = err.Error()
				break
			}
			if seen[change.Key] {
				failureText = "memory review returned duplicate key " + change.Key
				break
			}
			seen[change.Key] = true
		}
	}
	appliedMemories := make([]personal.Memory, 0, len(review.Changes))
	conflicts := 0
	if failureText == "" && review.Question == "" {
		for _, change := range review.Changes {
			memory, _, err := applyMemoryChange(ctx, tx, eventID, runID, change, sourceText, true)
			if errors.Is(err, errMemoryVersionConflict) {
				// Reviews are snapshots: version conflicts and facts superseded by newer input are skipped.
				conflicts++
				continue
			}
			if err != nil {
				return err
			}
			if memory != nil {
				appliedMemories = append(appliedMemories, *memory)
			}
		}
	}
	raw, err := json.Marshal(review)
	if err != nil {
		return err
	}
	finalStatus := personal.RunCompleted
	if failureText != "" {
		finalStatus = personal.RunFailed
		if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='failed',result=?,error=? WHERE id=? AND status='running'`, string(raw), failureText, runID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_runs SET status='completed',result=?,error='' WHERE id=? AND status='running'`, string(raw), runID); err != nil {
			return err
		}
	}
	payload := memoryReviewOutcomePayload{Review: review, Applied: appliedMemories, Failure: failureText}
	return commitStoreOutcome(ctx, tx, "memory.review.finish", string(finalStatus), payload,
		zap.Int64("event_id", eventID), zap.Int64("run_id", runID), zap.String("run_status", string(finalStatus)),
		zap.Int("proposed_count", len(review.Changes)), zap.Int("applied_count", len(appliedMemories)),
		zap.Int("skipped_count", len(review.Changes)-len(appliedMemories)), zap.Int("conflict_count", conflicts))
}

func scanMemory(row scanner) (*personal.Memory, error) {
	var m personal.Memory
	var deleted int
	var updated string
	if err := row.Scan(&m.Key, &m.Value, &m.Version, &m.SourceEventID, &deleted, &updated); err != nil {
		return nil, err
	}
	m.Deleted = deleted != 0
	var err error
	m.UpdatedAt, err = parseTimestamp(updated)
	return &m, err
}

func (s *Store) Memory(ctx context.Context, key string) (*personal.Memory, error) {
	if !validMemoryKey(key) {
		return nil, errors.New("personal: invalid memory key")
	}
	m, err := scanMemory(s.db.QueryRowContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memories WHERE key=?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

func (s *Store) Memories(ctx context.Context, query string, limit int) ([]personal.Memory, error) {
	if limit <= 0 {
		return []personal.Memory{}, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memories
		WHERE deleted=0 AND (?='' OR lower(key) LIKE lower(?) OR lower(value) LIKE lower(?))
		ORDER BY source_event_id DESC,key LIMIT ?`, query, "%"+query+"%", "%"+query+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	memories := make([]personal.Memory, 0, limit)
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, *m)
	}
	return memories, rows.Err()
}

func (s *Store) MemoryHistory(ctx context.Context, key string, limit int) ([]personal.Memory, error) {
	if key != "" && !validMemoryKey(key) {
		return nil, errors.New("personal: invalid memory key")
	}
	if limit <= 0 {
		return []personal.Memory{}, nil
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM (
		SELECT key,value,version,source_event_id,deleted,updated_at,id FROM personal_memory_history
		WHERE (?='' OR key=?) ORDER BY id DESC LIMIT ?)
		ORDER BY id`, key, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history := make([]personal.Memory, 0, limit)
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		history = append(history, *m)
	}
	return history, rows.Err()
}
