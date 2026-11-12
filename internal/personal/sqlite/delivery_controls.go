package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"kagari/internal/personal"
)

type cancelTaskControlSnapshot struct {
	Task personal.Task `json:"task"`
}

func (s *Store) ControlDelivery(ctx context.Context, eventID int64, key string, request personal.DeliveryControlRequest) (personal.DeliveryControlResult, error) {
	if eventID <= 0 || !requestKeyValid(key) {
		return personal.DeliveryControlResult{}, errors.New("personal: event ID and control request key are required")
	}
	if err := validateDeliveryControlRequest(request); err != nil {
		return personal.DeliveryControlResult{}, err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return personal.DeliveryControlResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.DeliveryControlResult{}, err
	}
	defer tx.Rollback()
	var oldRequest, oldResult string
	var oldID int64
	var oldDeliveryID int64
	var oldCreated string
	err = tx.QueryRowContext(ctx, `SELECT id,request_json,result_json,result_delivery_id,created_at FROM personal_delivery_controls
		WHERE event_id=? AND request_key=?`, eventID, key).Scan(&oldID, &oldRequest, &oldResult, &oldDeliveryID, &oldCreated)
	if err == nil {
		if oldRequest != string(requestJSON) {
			return personal.DeliveryControlResult{}, errors.New("personal: delivery control key already has a different request")
		}
		var result personal.DeliveryControlResult
		if err := json.Unmarshal([]byte(oldResult), &result); err != nil {
			return personal.DeliveryControlResult{}, fmt.Errorf("personal: decode saved delivery control result: %w", err)
		}
		createdAt, err := parseTimestamp(oldCreated)
		if err != nil {
			return personal.DeliveryControlResult{}, err
		}
		if result.Request != request || result.DeliveryID != oldDeliveryID || result.ID != 0 && result.ID != oldID {
			return personal.DeliveryControlResult{}, errors.New("personal: saved delivery control result does not match its request")
		}
		result.ID = oldID
		result.CreatedAt = createdAt
		if result.ID <= 0 {
			return personal.DeliveryControlResult{}, errors.New("personal: saved delivery control result has no identity")
		}
		if err := tx.Commit(); err != nil {
			return personal.DeliveryControlResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return personal.DeliveryControlResult{}, err
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.DeliveryControlResult{}, err
	}
	now := time.Now().UTC()
	var result personal.DeliveryControlResult
	var targetTaskID, targetRevision, targetDeliveryID int64
	var action string
	if request.Action == personal.DeliveryResend {
		original, err := scanDeliveryIntent(tx.QueryRowContext(ctx, `SELECT `+deliveryIntentColumns+` FROM personal_delivery_intents WHERE id=?`, request.DeliveryID))
		if err != nil {
			return personal.DeliveryControlResult{}, recordNotFound(err)
		}
		original.Scopes, err = loadDeliveryIntentScopesTx(ctx, tx, original.ID)
		if err != nil {
			return personal.DeliveryControlResult{}, err
		}
		for _, id := range original.Draft.Attachments {
			if _, err := s.validateStoredAttachment(ctx, tx, id); err != nil {
				return personal.DeliveryControlResult{}, err
			}
		}
		draft := original.Draft
		draft.Text = fmt.Sprintf("（历史版本：%s；原交付 #%d，生成于 %s）\n%s", deliveryScopeLabel(original.Scopes), original.ID, original.CreatedAt.UTC().Format(time.RFC3339Nano), original.Draft.Text)
		intent, err := insertDeliveryIntentTx(ctx, tx, deliveryIntentInsert{
			eventID: eventID, scheduleID: original.ScheduleID, originalDeliveryID: original.ID,
			purpose: personal.DeliveryHistorical, draft: draft, scopes: original.Scopes,
		}, now)
		if err != nil {
			return personal.DeliveryControlResult{}, err
		}
		result = personal.DeliveryControlResult{Request: request, DeliveryID: intent.ID, CreatedAt: now}
		targetDeliveryID, action = original.ID, "resend"
	} else {
		var scopes []personal.DeliveryScope
		text := ""
		switch {
		case request.Action == personal.DeliveryStop && request.TaskID > 0,
			request.Action == personal.DeliveryResume && request.TaskID > 0:
			var task *personal.Task
			task, err = scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM personal_tasks WHERE id=?`, request.TaskID))
			if err != nil {
				return personal.DeliveryControlResult{}, recordNotFound(err)
			}
			targetTaskID = task.ID
			scope := personal.DeliveryScope{TaskID: task.ID, Revision: task.Revision}
			runErr := tx.QueryRowContext(ctx, `SELECT id FROM personal_runs WHERE task_id=? AND revision=? AND kind='task' ORDER BY id DESC LIMIT 1`, task.ID, task.Revision).Scan(&scope.RunID)
			if runErr != nil && !errors.Is(runErr, sql.ErrNoRows) {
				return personal.DeliveryControlResult{}, runErr
			}
			scopes = []personal.DeliveryScope{scope}
			text = fmt.Sprintf("已于 %s 停止任务「%s」的后续结果和状态通知。此操作只影响尚未开始的发送及未来内容；已接受或正在发送的消息不会撤回，任务仍可继续执行。",
				now.Format(time.RFC3339), task.Title)
			if request.Action == personal.DeliveryStop {
				action = "stop"
			} else {
				action = "resume"
				text = fmt.Sprintf("已于 %s 恢复任务「%s」的后续结果和状态通知。此前已抑制的交付不会自动恢复，需要时可单独重发。", now.Format(time.RFC3339), task.Title)
			}
		case request.Action == personal.DeliveryStop && request.DeliveryID > 0:
			original, err := scanDeliveryIntent(tx.QueryRowContext(ctx, `SELECT `+deliveryIntentColumns+` FROM personal_delivery_intents WHERE id=?`, request.DeliveryID))
			if err != nil {
				return personal.DeliveryControlResult{}, recordNotFound(err)
			}
			scopes, err = loadDeliveryIntentScopesTx(ctx, tx, original.ID)
			if err != nil {
				return personal.DeliveryControlResult{}, err
			}
			text = fmt.Sprintf("已于 %s 停止交付 #%d。此操作只抑制尚未开始的发送；已接受或正在发送的消息不会撤回。", now.Format(time.RFC3339), original.ID)
			targetDeliveryID, action = original.ID, "stop"
		default:
			return personal.DeliveryControlResult{}, errors.New("personal: unsupported delivery control action")
		}
		intent, err := insertDeliveryIntentTx(ctx, tx, deliveryIntentInsert{
			eventID: eventID, purpose: personal.DeliveryControl,
			draft: personal.DeliveryDraft{Text: text, Attachments: []string{}}, scopes: scopes,
		}, now)
		if err != nil {
			return personal.DeliveryControlResult{}, err
		}
		result = personal.DeliveryControlResult{Request: request, DeliveryID: intent.ID, CreatedAt: now}
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return personal.DeliveryControlResult{}, err
	}
	resultID, err := insertDeliveryControlTx(ctx, tx, eventID, key, string(requestJSON), string(resultJSON), result.DeliveryID,
		action, targetTaskID, targetRevision, targetDeliveryID, now)
	if err != nil {
		return personal.DeliveryControlResult{}, err
	}
	result.ID = resultID
	if request.Action == personal.DeliveryStop {
		if request.TaskID > 0 {
			if err := suppressTaskDeliveriesTx(ctx, tx, request.TaskID, deliverySuppressionTaskStopped); err != nil {
				return personal.DeliveryControlResult{}, err
			}
		} else if err := suppressDeliveryTx(ctx, tx, request.DeliveryID, "delivery_stopped"); err != nil {
			return personal.DeliveryControlResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return personal.DeliveryControlResult{}, err
	}
	return result, nil
}

func validateDeliveryControlRequest(request personal.DeliveryControlRequest) error {
	switch request.Action {
	case personal.DeliveryStop:
		if (request.TaskID > 0) == (request.DeliveryID > 0) || request.TaskID < 0 || request.DeliveryID < 0 {
			return errors.New("personal: stop requires exactly one positive task or delivery ID")
		}
	case personal.DeliveryResume:
		if request.TaskID <= 0 || request.DeliveryID != 0 {
			return errors.New("personal: resume requires a positive task ID")
		}
	case personal.DeliveryResend:
		if request.DeliveryID <= 0 || request.TaskID != 0 {
			return errors.New("personal: resend requires a positive delivery ID")
		}
	default:
		return fmt.Errorf("personal: unsupported delivery control action %q", request.Action)
	}
	return nil
}

func deliveryScopeLabel(scopes []personal.DeliveryScope) string {
	if len(scopes) == 0 {
		return "无任务关联"
	}
	var builder strings.Builder
	for i, scope := range scopes {
		if i > 0 {
			builder.WriteString("、")
		}
		fmt.Fprintf(&builder, "任务 #%d 修订版 %d", scope.TaskID, scope.Revision)
	}
	return builder.String()
}

func insertDeliveryControlTx(ctx context.Context, tx *sql.Tx, eventID int64, key, requestJSON, resultJSON string, resultDeliveryID int64,
	action string, taskID, revision, deliveryID int64, now time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO personal_delivery_controls(event_id,request_key,request_json,result_json,result_delivery_id,
		action,target_task_id,target_revision,target_delivery_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		eventID, key, requestJSON, resultJSON, resultDeliveryID, action, nullableID(taskID), nullableID(revision), nullableID(deliveryID), timestamp(now))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func insertCancelTaskControlTx(ctx context.Context, tx *sql.Tx, eventID int64, key string, requestJSON []byte, task personal.Task, now time.Time) error {
	scope := personal.DeliveryScope{TaskID: task.ID, Revision: task.Revision}
	err := tx.QueryRowContext(ctx, `SELECT id FROM personal_runs WHERE task_id=? AND revision=? AND kind='task' ORDER BY id DESC LIMIT 1`, task.ID, task.Revision).Scan(&scope.RunID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	text := fmt.Sprintf("已于 %s 取消任务「%s」。", now.Format(time.RFC3339), task.Title)
	intent, err := insertDeliveryIntentTx(ctx, tx, deliveryIntentInsert{
		eventID: eventID, purpose: personal.DeliveryControl,
		draft: personal.DeliveryDraft{Text: text, Attachments: []string{}}, scopes: []personal.DeliveryScope{scope},
	}, now)
	if err != nil {
		return err
	}
	resultJSON, err := json.Marshal(cancelTaskControlSnapshot{Task: task})
	if err != nil {
		return err
	}
	_, err = insertDeliveryControlTx(ctx, tx, eventID, key, string(requestJSON), string(resultJSON), intent.ID,
		"cancel_task", task.ID, task.Revision, 0, now)
	return err
}
