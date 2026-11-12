package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"kagari/internal/personal"
)

type deliveryIntentInsert struct {
	eventID            int64
	scheduleID         int64
	originalDeliveryID int64
	purpose            personal.DeliveryPurpose
	draft              personal.DeliveryDraft
	scopes             []personal.DeliveryScope
	settlementTaskID   int64
	settlementRevision int64
	settlementPayload  string
	settlementResult   string
	detail             string
}

func normalizeDeliveryDraft(draft personal.DeliveryDraft) personal.DeliveryDraft {
	if draft.Attachments == nil {
		draft.Attachments = []string{}
	}
	return draft
}

func validateDeliveryDraft(draft personal.DeliveryDraft) error {
	if strings.TrimSpace(draft.Text) == "" && len(draft.Attachments) == 0 {
		return errors.New("personal: delivery draft requires text or an attachment")
	}
	seen := make(map[string]bool, len(draft.Attachments))
	for _, id := range draft.Attachments {
		if id == "" || seen[id] {
			return errors.New("personal: delivery attachment IDs must be nonempty and unique")
		}
		seen[id] = true
	}
	return nil
}

func canonicalDeliveryScopes(scopes []personal.DeliveryScope) []personal.DeliveryScope {
	result := append([]personal.DeliveryScope(nil), scopes...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].TaskID != result[j].TaskID {
			return result[i].TaskID < result[j].TaskID
		}
		if result[i].Revision != result[j].Revision {
			return result[i].Revision < result[j].Revision
		}
		return result[i].RunID < result[j].RunID
	})
	unique := result[:0]
	for _, scope := range result {
		if len(unique) > 0 && unique[len(unique)-1] == scope {
			continue
		}
		unique = append(unique, scope)
	}
	return unique
}

func insertDeliveryIntentTx(ctx context.Context, tx *sql.Tx, input deliveryIntentInsert, now time.Time) (personal.DeliveryIntent, error) {
	input.draft = normalizeDeliveryDraft(input.draft)
	if input.eventID <= 0 || !validDeliveryPurpose(input.purpose) {
		return personal.DeliveryIntent{}, errors.New("personal: delivery event and purpose are required")
	}
	if err := validateDeliveryDraft(input.draft); err != nil {
		return personal.DeliveryIntent{}, err
	}
	if input.settlementTaskID == 0 {
		if input.settlementRevision != 0 || input.settlementPayload != "" || input.settlementResult != "" {
			return personal.DeliveryIntent{}, errors.New("personal: incomplete task settlement identity")
		}
	} else if input.settlementTaskID < 0 || input.settlementRevision <= 0 || input.settlementPayload == "" || input.settlementResult == "" {
		return personal.DeliveryIntent{}, errors.New("personal: complete task settlement identity is required")
	}
	attachmentsJSON, err := json.Marshal(input.draft.Attachments)
	if err != nil {
		return personal.DeliveryIntent{}, err
	}
	if input.scopes == nil {
		input.scopes = []personal.DeliveryScope{}
	}
	input.scopes = canonicalDeliveryScopes(input.scopes)
	for _, scope := range input.scopes {
		if scope.TaskID <= 0 || scope.Revision <= 0 {
			return personal.DeliveryIntent{}, errors.New("personal: delivery task scope requires a positive task ID and revision")
		}
		var currentRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM personal_tasks WHERE id=?`, scope.TaskID).Scan(&currentRevision); err != nil {
			return personal.DeliveryIntent{}, err
		}
		if scope.Revision != currentRevision && input.purpose != personal.DeliveryHistorical {
			return personal.DeliveryIntent{}, fmt.Errorf("personal: delivery task %d revision changed from %d to %d", scope.TaskID, scope.Revision, currentRevision)
		}
		if scope.RunID > 0 {
			var runTaskID sql.NullInt64
			var runRevision int64
			var kind personal.RunKind
			if err := tx.QueryRowContext(ctx, `SELECT task_id,revision,kind FROM personal_runs WHERE id=?`, scope.RunID).
				Scan(&runTaskID, &runRevision, &kind); err != nil {
				return personal.DeliveryIntent{}, err
			}
			if !runTaskID.Valid || runTaskID.Int64 != scope.TaskID || runRevision != scope.Revision || kind != personal.RunTask {
				return personal.DeliveryIntent{}, fmt.Errorf("personal: delivery run %d does not match task %d revision %d", scope.RunID, scope.TaskID, scope.Revision)
			}
		}
	}
	created := timestamp(now.UTC())
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_delivery_intents(
		event_id,schedule_id,original_delivery_id,purpose,draft_text,draft_attachments,state,detail,
		settlement_task_id,settlement_revision,settlement_payload_json,settlement_result_json,created_at)
		VALUES(?,?,?,?,?,?,'pending',?,?,?,?,?,?)`,
		input.eventID, nullableID(input.scheduleID), nullableID(input.originalDeliveryID), input.purpose,
		input.draft.Text, string(attachmentsJSON), input.detail, nullableID(input.settlementTaskID),
		nullableID(input.settlementRevision), input.settlementPayload, input.settlementResult, created)
	if err != nil {
		return personal.DeliveryIntent{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return personal.DeliveryIntent{}, err
	}
	for ordinal, scope := range input.scopes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_delivery_scopes(delivery_id,ordinal,task_id,revision,run_id) VALUES(?,?,?,?,?)`,
			id, ordinal, scope.TaskID, scope.Revision, nullableID(scope.RunID)); err != nil {
			return personal.DeliveryIntent{}, err
		}
	}
	return personal.DeliveryIntent{ID: id, EventID: input.eventID, ScheduleID: input.scheduleID,
		OriginalDeliveryID: input.originalDeliveryID, Purpose: input.purpose, Draft: input.draft,
		Scopes: input.scopes, State: personal.DeliveryIntentPending, Detail: input.detail, CreatedAt: now.UTC()}, nil
}

func validDeliveryPurpose(purpose personal.DeliveryPurpose) bool {
	switch purpose {
	case personal.DeliveryResult, personal.DeliveryStatus, personal.DeliveryControl, personal.DeliveryHistorical:
		return true
	default:
		return false
	}
}

func scanDeliveryIntent(row scanner) (personal.DeliveryIntent, error) {
	var intent personal.DeliveryIntent
	var scheduleID, originalID sql.NullInt64
	var attachments, created string
	if err := row.Scan(&intent.ID, &intent.EventID, &scheduleID, &originalID, &intent.Purpose,
		&intent.Draft.Text, &attachments, &intent.State, &intent.Detail, &created); err != nil {
		return intent, err
	}
	intent.ScheduleID, intent.OriginalDeliveryID = scheduleID.Int64, originalID.Int64
	if err := json.Unmarshal([]byte(attachments), &intent.Draft.Attachments); err != nil {
		return intent, fmt.Errorf("personal: decode delivery attachments: %w", err)
	}
	if intent.Draft.Attachments == nil {
		intent.Draft.Attachments = []string{}
	}
	var err error
	intent.CreatedAt, err = parseTimestamp(created)
	return intent, err
}

const deliveryIntentColumns = `id,event_id,schedule_id,original_delivery_id,purpose,draft_text,draft_attachments,state,detail,created_at`

func loadDeliveryIntentScopesTx(ctx context.Context, tx *sql.Tx, deliveryID int64) ([]personal.DeliveryScope, error) {
	rows, err := tx.QueryContext(ctx, `SELECT task_id,revision,COALESCE(run_id,0) FROM personal_delivery_scopes WHERE delivery_id=? ORDER BY ordinal`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scopes := []personal.DeliveryScope{}
	for rows.Next() {
		var scope personal.DeliveryScope
		if err := rows.Scan(&scope.TaskID, &scope.Revision, &scope.RunID); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return scopes, rows.Close()
}

func loadDeliveryIntentsTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]personal.DeliveryIntent, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var intents []personal.DeliveryIntent
	for rows.Next() {
		intent, err := scanDeliveryIntent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range intents {
		intents[i].Scopes, err = loadDeliveryIntentScopesTx(ctx, tx, intents[i].ID)
		if err != nil {
			return nil, err
		}
	}
	if intents == nil {
		intents = []personal.DeliveryIntent{}
	}
	return intents, nil
}

func loadPendingSettlementIntentsTx(ctx context.Context, tx *sql.Tx, eventID int64) ([]personal.DeliveryIntent, error) {
	return loadDeliveryIntentsTx(ctx, tx, `SELECT `+deliveryIntentColumns+` FROM personal_delivery_intents
		WHERE event_id=? AND settlement_task_id IS NOT NULL AND state='pending' ORDER BY id`, eventID)
}

func deliveryScopesForEventTx(ctx context.Context, tx *sql.Tx, eventID int64, kind personal.EventKind, taskID, runID, revision int64) ([]personal.DeliveryScope, error) {
	if kind == personal.EventTask {
		if taskID <= 0 || runID <= 0 || revision <= 0 {
			return nil, errors.New("personal: task delivery scope requires its source run")
		}
		return []personal.DeliveryScope{{TaskID: taskID, Revision: revision, RunID: runID}}, nil
	}
	if kind != personal.EventUser {
		return []personal.DeliveryScope{}, nil
	}
	scopes := []personal.DeliveryScope{}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT task_id FROM personal_requests WHERE event_id=?
		AND operation IN ('create_task','revise_task','continue_task','add_task_input')
		AND task_id IS NOT NULL ORDER BY task_id`, eventID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var scope personal.DeliveryScope
		if err := rows.Scan(&scope.TaskID); err != nil {
			rows.Close()
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM personal_tasks WHERE id=?`, scope.TaskID).Scan(&scope.Revision); err != nil {
			rows.Close()
			return nil, err
		}
		var runTaskID sql.NullInt64
		var runKind personal.RunKind
		err := tx.QueryRowContext(ctx, `SELECT r.id,r.task_id,r.kind FROM personal_runs r JOIN personal_requests q ON q.run_id=r.id
			WHERE q.event_id=? AND q.task_id=? AND r.revision=? ORDER BY r.id DESC LIMIT 1`, eventID, scope.TaskID, scope.Revision).
			Scan(&scope.RunID, &runTaskID, &runKind)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			rows.Close()
			return nil, err
		}
		if err == nil && (!runTaskID.Valid || runTaskID.Int64 != scope.TaskID || runKind != personal.RunTask) {
			rows.Close()
			return nil, fmt.Errorf("personal: request run %d does not belong to task %d", scope.RunID, scope.TaskID)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if taskID > 0 {
		found := false
		for _, scope := range scopes {
			if scope.TaskID == taskID {
				found = true
				break
			}
		}
		if !found {
			var currentRevision int64
			if err := tx.QueryRowContext(ctx, `SELECT revision FROM personal_tasks WHERE id=?`, taskID).Scan(&currentRevision); err != nil {
				return nil, err
			}
			scopes = append(scopes, personal.DeliveryScope{TaskID: taskID, Revision: currentRevision})
		}
	}
	return canonicalDeliveryScopes(scopes), nil
}

func (s *Store) EventDeliveries(ctx context.Context, eventID int64) ([]personal.DeliveryIntent, error) {
	if eventID <= 0 {
		return nil, errors.New("personal: event ID must be positive")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	intents, err := loadPendingSettlementIntentsTx(ctx, tx, eventID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return intents, nil
}

func updateDeliveryIntentRenderedTx(ctx context.Context, tx *sql.Tx, deliveryID int64, detail string) error {
	res, err := tx.ExecContext(ctx, `UPDATE personal_delivery_intents SET state='rendered',detail=? WHERE id=? AND state='pending'`, detail, deliveryID)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("personal: delivery intent %d is not pending", deliveryID)
	}
	return nil
}
