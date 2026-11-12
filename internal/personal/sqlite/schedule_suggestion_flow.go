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

	"go.uber.org/zap"

	"kagari/internal/personal"
)

// Suggestion persistence and user decisions.
func (s *Store) ProposeScheduleContextUpdate(ctx context.Context, eventID int64, key string, taskID int64, proposal personal.ScheduleContextProposal, now time.Time) (personal.ScheduleSuggestion, bool, error) {
	if !requestKeyValid(key) || taskID <= 0 || now.IsZero() {
		return personal.ScheduleSuggestion{}, false, errors.New("personal: schedule suggestion requires a request key, task ID and timestamp")
	}
	payload, err := json.Marshal(scheduleSuggestionProposalRequest{TaskID: taskID, Proposal: proposal})
	if err != nil {
		return personal.ScheduleSuggestion{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.ScheduleSuggestion{}, false, err
	}
	defer tx.Rollback()
	if err := requireScheduleTaskEventOwner(ctx, tx, eventID, taskID); err != nil {
		return personal.ScheduleSuggestion{}, false, err
	}
	var replay personal.ScheduleSuggestion
	replayedScheduleID, replayed, err := previousScheduleSuggestionRequest(ctx, tx, eventID, key, "propose_schedule_context_update", string(payload), &replay)
	if err != nil {
		return personal.ScheduleSuggestion{}, false, err
	}
	if replayed {
		if replay.ScheduleID != replayedScheduleID {
			return personal.ScheduleSuggestion{}, false, errors.New("personal: saved schedule suggestion request has an inconsistent schedule ID")
		}
		if err := commitStoreOutcome(ctx, tx, "schedule.suggestion.propose", "replayed", replay,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", replay.ScheduleID), zap.Int64("task_id", taskID), zap.Int64("suggestion_id", replay.ID)); err != nil {
			return personal.ScheduleSuggestion{}, false, err
		}
		return replay, true, nil
	}
	callback, err := currentScheduleCallback(ctx, tx, eventID, taskID)
	if err != nil {
		return personal.ScheduleSuggestion{}, false, err
	}
	prepared, err := prepareScheduleSuggestion(ctx, tx, callback.Schedule, proposal)
	if err != nil {
		return personal.ScheduleSuggestion{}, false, err
	}
	now = now.UTC()
	suggestion, err := findScheduleSuggestion(ctx, tx, callback.ScheduleID, prepared.ChangeKey)
	if err != nil {
		return personal.ScheduleSuggestion{}, false, err
	}
	outcome := "created"
	if suggestion == nil {
		created, createErr := insertScheduleSuggestion(ctx, tx, personal.ScheduleSuggestion{
			Revision: 1, ScheduleID: callback.ScheduleID, ScheduleRevision: callback.Schedule.Revision,
			OriginEventID: eventID, TaskID: taskID, ChangeKey: prepared.ChangeKey, MemoryRefs: prepared.MemoryRefs,
			OldContext: callback.Schedule.Context, NewContext: prepared.NewContext, Replacements: prepared.Replacements,
			Status: "pending", CreatedAt: now, UpdatedAt: now,
		})
		if createErr != nil {
			return personal.ScheduleSuggestion{}, false, createErr
		}
		suggestion = &created
	} else if suggestion.Status != "pending" || suggestion.NotifiedEventID != 0 {
		outcome = "deduplicated"
	} else {
		outcome = "transferred"
		updated, err := updatePendingScheduleSuggestion(ctx, tx, *suggestion, eventID, taskID, callback.Schedule.Revision,
			callback.Schedule.Context, prepared, 0, false, now)
		if err != nil {
			return personal.ScheduleSuggestion{}, false, err
		}
		suggestion = &updated
	}
	if err := insertScheduleSuggestionRequest(ctx, tx, eventID, key, "propose_schedule_context_update", string(payload), callback.ScheduleID, *suggestion); err != nil {
		return personal.ScheduleSuggestion{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.suggestion.propose", outcome, *suggestion,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", callback.ScheduleID), zap.Int64("task_id", taskID), zap.Int64("suggestion_id", suggestion.ID)); err != nil {
		return personal.ScheduleSuggestion{}, false, err
	}
	return *suggestion, false, nil
}

func (s *Store) ScheduleSuggestions(ctx context.Context, scheduleID int64) ([]personal.ScheduleSuggestion, error) {
	if scheduleID <= 0 {
		return nil, errors.New("personal: schedule ID must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+suggestionColumns+` FROM personal_schedule_context_suggestions WHERE schedule_id=? AND status='pending' ORDER BY id DESC`, scheduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	suggestions := make([]personal.ScheduleSuggestion, 0)
	for rows.Next() {
		suggestion, err := scanScheduleSuggestion(rows)
		if err != nil {
			return nil, err
		}
		suggestions = append(suggestions, *suggestion)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return suggestions, nil
}

func (s *Store) ReplyScheduleSuggestions(ctx context.Context, outboxID int64) ([]personal.ScheduleSuggestion, error) {
	if outboxID <= 0 {
		return nil, errors.New("personal: outbox ID must be positive")
	}
	columns := "s." + strings.ReplaceAll(suggestionColumns, ",", ",s.")
	suggestion, err := scanScheduleSuggestion(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM personal_outbox d
		JOIN personal_schedule_context_suggestions s ON d.suggestion_id=s.id AND s.presented_event_id=d.event_id
		WHERE d.id=? AND d.status='sent'`, outboxID))
	if errors.Is(err, sql.ErrNoRows) {
		return []personal.ScheduleSuggestion{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []personal.ScheduleSuggestion{*suggestion}, nil
}

func scheduleSuggestionDisplayed(ctx context.Context, tx *sql.Tx, eventID int64, suggestion personal.ScheduleSuggestion) error {
	if suggestion.PresentedEventID == 0 {
		return errors.New("personal: schedule suggestion has not been shown to the user")
	}
	if suggestion.PresentedEventID == eventID {
		return errors.New("personal: a refreshed schedule suggestion requires a later user event")
	}
	var displayed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM personal_events e JOIN personal_outbox d ON d.event_id=e.id
		WHERE e.id=? AND e.status='completed' AND d.suggestion_id=?)`, suggestion.PresentedEventID, suggestion.ID).Scan(&displayed); err != nil {
		return err
	}
	if !displayed {
		return errors.New("personal: schedule suggestion presentation is not complete")
	}
	return nil
}

func currentSuggestionIsValid(ctx context.Context, tx *sql.Tx, schedule personal.Schedule, suggestion personal.ScheduleSuggestion) (bool, error) {
	if schedule.Status == personal.ScheduleCancelled || schedule.Revision != suggestion.ScheduleRevision || schedule.Context != suggestion.OldContext {
		return false, nil
	}
	memories, _, ok, err := scheduleSuggestionMemoryValues(ctx, tx, suggestion)
	if err != nil || !ok {
		return false, err
	}
	for _, ref := range suggestion.MemoryRefs {
		memory, found := memories[ref.Key]
		if !found || memory.Version != ref.Version || !strings.Contains(memory.Value, ref.Evidence) {
			return false, nil
		}
	}
	for _, replacement := range suggestion.Replacements {
		if _, found := memories[replacement.MemoryKey]; !found {
			return false, nil
		}
	}
	prepared, err := rebuildStoredScheduleSuggestion(schedule.Context, suggestion.Replacements)
	if err != nil {
		return false, err
	}
	if prepared.NewContext != suggestion.NewContext {
		return false, errors.New("personal: stored schedule suggestion does not match its replacements")
	}
	return true, nil
}

func rebuildStoredScheduleSuggestion(contextText string, replacements []personal.ContextReplacement) (preparedScheduleSuggestion, error) {
	var prepared preparedScheduleSuggestion
	if len(replacements) == 0 || len(replacements) > maxScheduleSuggestionItems {
		return prepared, errors.New("personal: stored schedule suggestion has an invalid replacement count")
	}
	placed := make([]placedReplacement, 0, len(replacements))
	for _, replacement := range replacements {
		start, ok := uniqueSubstring(contextText, replacement.Old)
		if !ok || replacement.New == "" {
			return prepared, errors.New("personal: stored schedule suggestion has an invalid replacement")
		}
		placed = append(placed, placedReplacement{start: start, end: start + len(replacement.Old), value: replacement})
	}
	sort.Slice(placed, func(i, j int) bool { return placed[i].start < placed[j].start })
	var result strings.Builder
	last := 0
	for _, item := range placed {
		if item.start < last {
			return prepared, errors.New("personal: stored schedule suggestion replacements overlap")
		}
		result.WriteString(contextText[last:item.start])
		result.WriteString(item.value.New)
		last = item.end
		prepared.Replacements = append(prepared.Replacements, item.value)
	}
	result.WriteString(contextText[last:])
	prepared.NewContext = result.String()
	return prepared, nil
}

func markSuggestionDismissed(ctx context.Context, tx *sql.Tx, suggestion personal.ScheduleSuggestion, now time.Time) (personal.ScheduleSuggestion, error) {
	result, err := tx.ExecContext(ctx, `UPDATE personal_schedule_context_suggestions SET status='dismissed',updated_at=? WHERE id=? AND status='pending'`, timestamp(now), suggestion.ID)
	if err := requireSuggestionUpdated(result, err); err != nil {
		return personal.ScheduleSuggestion{}, err
	}
	suggestion.Status, suggestion.UpdatedAt = "dismissed", now
	return suggestion, nil
}

func setSuggestionPresented(ctx context.Context, tx *sql.Tx, suggestion personal.ScheduleSuggestion, eventID int64, now time.Time) (personal.ScheduleSuggestion, error) {
	result, err := tx.ExecContext(ctx, `UPDATE personal_schedule_context_suggestions SET presented_event_id=?,updated_at=? WHERE id=? AND status='pending'`, eventID, timestamp(now), suggestion.ID)
	if err := requireSuggestionUpdated(result, err); err != nil {
		return personal.ScheduleSuggestion{}, err
	}
	suggestion.PresentedEventID, suggestion.UpdatedAt = eventID, now
	return suggestion, nil
}

func refreshScheduleSuggestion(ctx context.Context, tx *sql.Tx, eventID int64, schedule personal.Schedule, suggestion personal.ScheduleSuggestion, now time.Time) (personal.ScheduleSuggestionResolution, error) {
	resolution := personal.ScheduleSuggestionResolution{Status: "stale", Suggestion: suggestion, Schedule: &schedule}
	if schedule.Status == personal.ScheduleCancelled {
		updated, err := setSuggestionPresented(ctx, tx, suggestion, eventID, now)
		if err != nil {
			return resolution, err
		}
		resolution.Suggestion, resolution.NeedsProposal = updated, true
		return resolution, nil
	}
	memories, refs, ok, err := scheduleSuggestionMemoryValues(ctx, tx, suggestion)
	if err != nil {
		return resolution, err
	}
	if !ok {
		updated, err := setSuggestionPresented(ctx, tx, suggestion, eventID, now)
		if err != nil {
			return resolution, err
		}
		resolution.Suggestion, resolution.NeedsProposal = updated, true
		return resolution, nil
	}
	prepared, noChange, needsProposal := prepareRefreshedSuggestion(schedule, suggestion, memories, refs)
	if noChange {
		dismissed, err := markSuggestionDismissed(ctx, tx, suggestion, now)
		if err != nil {
			return resolution, err
		}
		resolution.Status, resolution.Suggestion = "no_change", dismissed
		return resolution, nil
	}
	if needsProposal {
		updated, err := setSuggestionPresented(ctx, tx, suggestion, eventID, now)
		if err != nil {
			return resolution, err
		}
		resolution.Suggestion, resolution.NeedsProposal = updated, true
		return resolution, nil
	}
	if err := validateScheduleDefinition(schedule.Title, schedule.Goal, prepared.NewContext, schedule.WindowMode, schedule.RequiredSources); err != nil {
		updated, err := setSuggestionPresented(ctx, tx, suggestion, eventID, now)
		if err != nil {
			return resolution, err
		}
		resolution.Suggestion, resolution.NeedsProposal = updated, true
		return resolution, nil
	}
	if prepared.ChangeKey == suggestion.ChangeKey {
		updated, err := updatePendingScheduleSuggestion(ctx, tx, suggestion, suggestion.OriginEventID, suggestion.TaskID,
			schedule.Revision, schedule.Context, prepared, eventID, true, now)
		if err != nil {
			return resolution, err
		}
		resolution.Suggestion = updated
		return resolution, nil
	}
	oldSuggestion, err := markSuggestionDismissed(ctx, tx, suggestion, now)
	if err != nil {
		return resolution, err
	}
	resolution.Suggestion = oldSuggestion
	current, err := findScheduleSuggestion(ctx, tx, schedule.ID, prepared.ChangeKey)
	if err != nil {
		return resolution, err
	}
	if current != nil && current.Status != "pending" {
		resolution.NeedsProposal = true
		return resolution, nil
	}
	if current == nil {
		created, createErr := insertScheduleSuggestion(ctx, tx, personal.ScheduleSuggestion{
			Revision: 1, ScheduleID: schedule.ID, ScheduleRevision: schedule.Revision,
			OriginEventID: suggestion.OriginEventID, TaskID: suggestion.TaskID, ChangeKey: prepared.ChangeKey,
			MemoryRefs: prepared.MemoryRefs, OldContext: schedule.Context, NewContext: prepared.NewContext,
			Replacements: prepared.Replacements, Status: "pending", PresentedEventID: eventID,
			CreatedAt: now, UpdatedAt: now,
		})
		if createErr != nil {
			return resolution, createErr
		}
		current = &created
	} else {
		updated, err := updatePendingScheduleSuggestion(ctx, tx, *current, current.OriginEventID, current.TaskID,
			schedule.Revision, schedule.Context, prepared, eventID, false, now)
		if err != nil {
			return resolution, err
		}
		current = &updated
	}
	resolution.Suggestion = *current
	return resolution, nil
}

func (s *Store) ResolveScheduleContextUpdate(ctx context.Context, eventID int64, key string, suggestionID, expectedRevision int64, decision string, now time.Time) (personal.ScheduleSuggestionResolution, bool, error) {
	if !requestKeyValid(key) || suggestionID <= 0 || expectedRevision <= 0 || now.IsZero() || (decision != "apply" && decision != "keep") {
		return personal.ScheduleSuggestionResolution{}, false, errors.New("personal: suggestion ID, revision, request key, timestamp and apply/keep decision are required")
	}
	payload, err := json.Marshal(scheduleSuggestionResolutionRequest{SuggestionID: suggestionID, ExpectedRevision: expectedRevision, Decision: decision})
	if err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	var replay personal.ScheduleSuggestionResolution
	replayedScheduleID, replayed, err := previousScheduleSuggestionRequest(ctx, tx, eventID, key, "resolve_schedule_context_update", string(payload), &replay)
	if err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	if replayed {
		if replay.Suggestion.ScheduleID != replayedScheduleID {
			return personal.ScheduleSuggestionResolution{}, false, errors.New("personal: saved schedule suggestion resolution has an inconsistent schedule ID")
		}
		if err := commitStoreOutcome(ctx, tx, "schedule.suggestion.resolve", "replayed", replay,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", replayedScheduleID), zap.Int64("suggestion_id", suggestionID)); err != nil {
			return personal.ScheduleSuggestionResolution{}, false, err
		}
		return replay, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	suggestion, err := scanScheduleSuggestion(tx.QueryRowContext(ctx, `SELECT `+suggestionColumns+` FROM personal_schedule_context_suggestions WHERE id=?`, suggestionID))
	if errors.Is(err, sql.ErrNoRows) {
		return personal.ScheduleSuggestionResolution{}, false, errors.New("personal: schedule suggestion does not exist")
	}
	if err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	if suggestion.Revision != expectedRevision {
		return personal.ScheduleSuggestionResolution{}, false, fmt.Errorf("personal: schedule suggestion revision is %d, expected %d", suggestion.Revision, expectedRevision)
	}
	if suggestion.Status != "pending" {
		return personal.ScheduleSuggestionResolution{}, false, fmt.Errorf("personal: schedule suggestion is already %s", suggestion.Status)
	}
	schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, suggestion.ScheduleID))
	if err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	now = now.UTC()
	resolution := personal.ScheduleSuggestionResolution{Suggestion: *suggestion, Schedule: schedule}
	if decision == "keep" {
		resolution.Status = "dismissed"
		resolution.Suggestion, err = markSuggestionDismissed(ctx, tx, *suggestion, now)
	} else {
		if err := scheduleSuggestionDisplayed(ctx, tx, eventID, *suggestion); err != nil {
			return personal.ScheduleSuggestionResolution{}, false, err
		}
		var valid bool
		valid, err = currentSuggestionIsValid(ctx, tx, *schedule, *suggestion)
		if err != nil {
			return personal.ScheduleSuggestionResolution{}, false, err
		}
		if !valid {
			resolution, err = refreshScheduleSuggestion(ctx, tx, eventID, *schedule, *suggestion, now)
		} else {
			contextText := suggestion.NewContext
			updated, updateErr := s.updateScheduleTx(ctx, tx, eventID, suggestion.ScheduleID, schedule.Revision,
				personal.SchedulePatch{Context: &contextText}, now)
			if updateErr != nil {
				return personal.ScheduleSuggestionResolution{}, false, updateErr
			}
			result, err := tx.ExecContext(ctx, `UPDATE personal_schedule_context_suggestions SET status='applied',updated_at=? WHERE id=? AND status='pending'`, timestamp(now), suggestion.ID)
			if err := requireSuggestionUpdated(result, err); err != nil {
				return personal.ScheduleSuggestionResolution{}, false, err
			}
			suggestion.Status, suggestion.UpdatedAt = "applied", now
			resolution = personal.ScheduleSuggestionResolution{Status: "applied", Suggestion: *suggestion, Schedule: &updated}
		}
	}
	if err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	if err := insertScheduleSuggestionRequest(ctx, tx, eventID, key, "resolve_schedule_context_update", string(payload), resolution.Suggestion.ScheduleID, resolution); err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.suggestion.resolve", resolution.Status, resolution,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", resolution.Suggestion.ScheduleID), zap.Int64("suggestion_id", resolution.Suggestion.ID)); err != nil {
		return personal.ScheduleSuggestionResolution{}, false, err
	}
	return resolution, false, nil
}
