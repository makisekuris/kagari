package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"kagari/internal/channel"
	"kagari/internal/domain"
)

// OpenExisting is deliberately separate from NewStore: diagnostic commands may
// not create a database, initialize schema, change permissions, or recover work.
func OpenExisting(ctx context.Context, path string) (*Store, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, errors.New("personal inspection requires an existing database file")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("personal database must be a regular file")
	}
	query := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(5000)"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: query.Encode()}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, baseDir: filepath.Dir(abs)}, nil
}

type EventRecord struct {
	Event
	Status EventStatus `json:"status"`
}

type RunExport struct {
	Run
	Sources []domain.Source `json:"expanded_sources,omitempty"`
}

func (s *Store) ExportRuns(ctx context.Context, taskID int64, limit int) ([]RunExport, error) {
	runs, err := s.Runs(ctx, taskID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RunExport, 0, len(runs))
	for _, run := range runs {
		export := RunExport{Run: run}
		if run.Result != "" {
			var result StageResult
			if err = json.Unmarshal([]byte(run.Result), &result); err != nil {
				return nil, err
			}
			for _, ref := range result.Sources {
				source, err := s.ExpandSource(ctx, ref.ID)
				if err != nil {
					return nil, err
				}
				export.Sources = append(export.Sources, source)
			}
		}
		out = append(out, export)
	}
	return out, nil
}

type DeliveryRecord struct {
	ID           int64            `json:"id"`
	EventID      int64            `json:"event_id"`
	Part         int              `json:"part"`
	Text         string           `json:"text,omitempty"`
	Status       string           `json:"status"`
	Attempts     int              `json:"attempts"`
	AttachmentID string           `json:"attachment_id,omitempty"`
	Receipt      *channel.Receipt `json:"receipt,omitempty"`
	Detail       string           `json:"detail,omitempty"`
}

type DiagnosticSnapshot struct {
	StoreID    string           `json:"store_id"`
	Events     []EventRecord    `json:"events"`
	Tasks      []Task           `json:"tasks"`
	Runs       []Run            `json:"runs"`
	Deliveries []DeliveryRecord `json:"deliveries"`
	Memories   []Memory         `json:"memories"`
}

func scanEventRecord(row scanner) (EventRecord, error) {
	var record EventRecord
	var task, run, scheduleID, replyOutboxID sql.NullInt64
	var attachments, created string
	err := row.Scan(&record.ID, &record.Kind, &record.Text, &task, &run, &record.Channel, &record.Account, &record.Sender,
		&record.MessageID, &record.ReplyToID, &attachments, &record.Interrupted, &created, &scheduleID, &replyOutboxID, &record.Status)
	if err != nil {
		return record, err
	}
	record.TaskID, record.RunID = task.Int64, run.Int64
	record.ScheduleID, record.ReplyOutboxID = scheduleID.Int64, replyOutboxID.Int64
	if err := json.Unmarshal([]byte(attachments), &record.Attachments); err != nil {
		return record, err
	}
	record.CreatedAt, err = parseTimestamp(created)
	return record, err
}

func (s *Store) RecentEvents(ctx context.Context, limit int) ([]EventRecord, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("event limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+`,status FROM personal_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []EventRecord{}
	for rows.Next() {
		event, err := scanEventRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range result {
		if err := s.refreshAttachments(ctx, s.db, result[i].Attachments); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Diagnostic reads one database snapshot, then releases its transaction before
// the CLI scans log files. A long log scan must not hold a SQLite read lock.
func (s *Store) Diagnostic(ctx context.Context, eventID, taskID int64) (DiagnosticSnapshot, error) {
	result := DiagnosticSnapshot{Events: []EventRecord{}, Tasks: []Task{}, Runs: []Run{}, Deliveries: []DeliveryRecord{}, Memories: []Memory{}}
	if (eventID > 0) == (taskID > 0) || eventID < 0 || taskID < 0 {
		return result, errors.New("specify exactly one positive event or task ID")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT value FROM personal_meta WHERE key='store_id'`).Scan(&result.StoreID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	taskIDs := []int64{}
	if taskID > 0 {
		var found int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM personal_tasks WHERE id=?`, taskID).Scan(&found); err != nil {
			return result, fmt.Errorf("find task %d: %w", taskID, err)
		}
		taskIDs = append(taskIDs, taskID)
	} else {
		var found int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM personal_events WHERE id=?`, eventID).Scan(&found); err != nil {
			return result, fmt.Errorf("find event %d: %w", eventID, err)
		}
		// Requests preserve links even when a conversation touched several tasks
		// and therefore could not attach a single task_id to its reply event.
		rows, err := tx.QueryContext(ctx, `SELECT task_id FROM personal_events WHERE id=? AND task_id IS NOT NULL
			UNION SELECT task_id FROM personal_runs WHERE event_id=? AND task_id IS NOT NULL
			UNION SELECT task_id FROM personal_requests WHERE event_id=? AND task_id IS NOT NULL
			UNION SELECT r.task_id FROM personal_runs r JOIN personal_events e ON e.run_id=r.id WHERE e.id=? AND r.task_id IS NOT NULL`, eventID, eventID, eventID, eventID)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return result, err
			}
			taskIDs = append(taskIDs, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
	}
	taskList := inspectionIDs(taskIDs)
	rows, err := tx.QueryContext(ctx, `SELECT `+eventColumns+`,status FROM personal_events WHERE id=? OR task_id IN (`+taskList+`)
		OR run_id IN (SELECT id FROM personal_runs WHERE task_id IN (`+taskList+`))
		OR id IN (SELECT event_id FROM personal_runs WHERE task_id IN (`+taskList+`))
		OR id IN (SELECT event_id FROM personal_requests WHERE task_id IN (`+taskList+`)) ORDER BY id`, eventID)
	if err != nil {
		return result, err
	}
	eventIDs := []int64{}
	for rows.Next() {
		event, err := scanEventRecord(rows)
		if err != nil {
			rows.Close()
			return result, err
		}
		result.Events = append(result.Events, event)
		eventIDs = append(eventIDs, event.ID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for i := range result.Events {
		if err := s.refreshAttachments(ctx, tx, result.Events[i].Attachments); err != nil {
			return result, err
		}
	}
	eventList := inspectionIDs(eventIDs)
	rows, err = tx.QueryContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id IN (`+taskList+`) ORDER BY id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return result, err
		}
		result.Tasks = append(result.Tasks, *task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT `+runColumns+` FROM personal_runs WHERE task_id IN (`+taskList+`) OR (task_id IS NULL AND event_id IN (`+eventList+`)) ORDER BY id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return result, err
		}
		result.Runs = append(result.Runs, *run)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,event_id,part,text,attachment_id,status,attempts,
		receipt_channel,receipt_account,receipt_conversation,receipt_message_id,detail
		FROM personal_outbox WHERE event_id IN (`+eventList+`) ORDER BY id`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var delivery DeliveryRecord
		var receipt channel.Receipt
		var attachmentID sql.NullString
		var receiptChannel, receiptAccount, receiptConversation, receiptMessageID string
		if err := rows.Scan(&delivery.ID, &delivery.EventID, &delivery.Part, &delivery.Text, &attachmentID, &delivery.Status,
			&delivery.Attempts, &receiptChannel, &receiptAccount, &receiptConversation, &receiptMessageID, &delivery.Detail); err != nil {
			rows.Close()
			return result, err
		}
		delivery.AttachmentID = attachmentID.String
		receipt.Channel, receipt.Account, receipt.Conversation, receipt.MessageID = receiptChannel, receiptAccount, receiptConversation, receiptMessageID
		if receipt != (channel.Receipt{}) {
			delivery.Receipt = &receipt
		}
		result.Deliveries = append(result.Deliveries, delivery)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	// A synchronous memory change has no Run. Follow its history to the current
	// value, including tombstones and later corrections by a different event.
	rows, err = tx.QueryContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memories
		WHERE key IN (SELECT key FROM personal_memory_history WHERE source_event_id IN (`+eventList+`)) ORDER BY key`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			rows.Close()
			return result, err
		}
		result.Memories = append(result.Memories, *memory)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func inspectionIDs(ids []int64) string {
	if len(ids) == 0 {
		return "NULL"
	}
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(values, ",")
}
