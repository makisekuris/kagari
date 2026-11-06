package personal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

const maxScheduleSuggestionItems = 8

const suggestionColumns = `id,revision,schedule_id,schedule_revision,origin_event_id,task_id,change_key,memory_refs_json,old_context,new_context,replacements_json,status,notified_event_id,presented_event_id,created_at,updated_at`

func scanScheduleSuggestion(row scanner) (*ScheduleSuggestion, error) {
	var suggestion ScheduleSuggestion
	var refs, replacements, created, updated string
	var notifiedEventID, presentedEventID sql.NullInt64
	if err := row.Scan(&suggestion.ID, &suggestion.Revision, &suggestion.ScheduleID, &suggestion.ScheduleRevision,
		&suggestion.OriginEventID, &suggestion.TaskID, &suggestion.ChangeKey, &refs, &suggestion.OldContext,
		&suggestion.NewContext, &replacements, &suggestion.Status, &notifiedEventID, &presentedEventID, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(refs), &suggestion.MemoryRefs); err != nil {
		return nil, fmt.Errorf("personal: decode schedule suggestion memory references: %w", err)
	}
	if err := json.Unmarshal([]byte(replacements), &suggestion.Replacements); err != nil {
		return nil, fmt.Errorf("personal: decode schedule suggestion replacements: %w", err)
	}
	if suggestion.MemoryRefs == nil {
		suggestion.MemoryRefs = []ScheduleMemoryRef{}
	}
	if suggestion.Replacements == nil {
		suggestion.Replacements = []ContextReplacement{}
	}
	suggestion.NotifiedEventID = notifiedEventID.Int64
	suggestion.PresentedEventID = presentedEventID.Int64
	var err error
	if suggestion.CreatedAt, err = parseTimestamp(created); err != nil {
		return nil, err
	}
	if suggestion.UpdatedAt, err = parseTimestamp(updated); err != nil {
		return nil, err
	}
	return &suggestion, nil
}

type scheduleSuggestionProposalRequest struct {
	TaskID   int64                   `json:"task_id"`
	Proposal ScheduleContextProposal `json:"proposal"`
}

type scheduleSuggestionResolutionRequest struct {
	SuggestionID     int64  `json:"suggestion_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Decision         string `json:"decision"`
}

func previousScheduleSuggestionRequest(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string, result any) (int64, bool, error) {
	var storedOperation, storedPayload, resultJSON string
	var scheduleID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT operation,payload,schedule_id,result_json FROM personal_requests WHERE event_id=? AND request_key=?`, eventID, key).
		Scan(&storedOperation, &storedPayload, &scheduleID, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if storedOperation != operation || storedPayload != payload {
		return 0, false, errors.New("personal: request key reused with different input")
	}
	if !scheduleID.Valid || resultJSON == "" {
		return 0, false, errors.New("personal: saved schedule suggestion request has no result")
	}
	if err := json.Unmarshal([]byte(resultJSON), result); err != nil {
		return 0, false, fmt.Errorf("personal: decode saved schedule suggestion request result: %w", err)
	}
	return scheduleID.Int64, true, nil
}

func insertScheduleSuggestionRequest(ctx context.Context, tx *sql.Tx, eventID int64, key, operation, payload string, scheduleID int64, result any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO personal_requests(event_id,request_key,operation,payload,schedule_id,result_json) VALUES(?,?,?,?,?,?)`,
		eventID, key, operation, payload, scheduleID, string(encoded))
	return err
}

func requireSuggestionUpdated(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("personal: schedule suggestion changed during the request")
	}
	return nil
}

type scheduleCallback struct {
	TaskID     int64
	Revision   int64
	ScheduleID int64
	Schedule   Schedule
}

func currentScheduleCallback(ctx context.Context, tx *sql.Tx, eventID, taskID int64) (scheduleCallback, error) {
	var callback scheduleCallback
	var kind EventKind
	var status EventStatus
	var actualTaskID, runID, eventScheduleID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,task_id,run_id,schedule_id FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &status, &actualTaskID, &runID, &eventScheduleID); err != nil {
		return callback, err
	}
	if kind != EventTask || status != EventRunning || !actualTaskID.Valid || actualTaskID.Int64 != taskID || !runID.Valid {
		return callback, errors.New("personal: schedule suggestion requires the current task callback")
	}
	var runTaskID, revision int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id,revision FROM personal_runs WHERE id=? AND kind='task'`, runID.Int64).
		Scan(&runTaskID, &revision); err != nil {
		return callback, err
	}
	if runTaskID != taskID {
		return callback, errors.New("personal: schedule suggestion callback belongs to another task")
	}
	if err := requireCallback(ctx, tx, eventID, taskID, revision); err != nil {
		return callback, err
	}
	var occurrenceScheduleID, occurrenceScopeRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT schedule_id,scope_revision FROM personal_schedule_occurrences WHERE task_id=?`, taskID).
		Scan(&occurrenceScheduleID, &occurrenceScopeRevision); err != nil {
		return callback, err
	}
	if !eventScheduleID.Valid || eventScheduleID.Int64 != occurrenceScheduleID {
		return callback, errors.New("personal: task callback does not match its scheduled occurrence")
	}
	schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, occurrenceScheduleID))
	if err != nil {
		return callback, err
	}
	if schedule.Status == ScheduleCancelled || schedule.ScopeRevision != occurrenceScopeRevision {
		return callback, errors.New("personal: schedule suggestion callback belongs to an outdated schedule definition")
	}
	return scheduleCallback{TaskID: taskID, Revision: revision, ScheduleID: occurrenceScheduleID, Schedule: *schedule}, nil
}

func requireScheduleTaskEventOwner(ctx context.Context, tx *sql.Tx, eventID, taskID int64) error {
	var kind EventKind
	var eventTaskID, runID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT kind,task_id,run_id FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &eventTaskID, &runID); err != nil {
		return err
	}
	if kind != EventTask || !eventTaskID.Valid || eventTaskID.Int64 != taskID || !runID.Valid {
		return errors.New("personal: schedule suggestion request does not belong to this task callback")
	}
	var runTaskID int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM personal_runs WHERE id=? AND kind='task'`, runID.Int64).
		Scan(&runTaskID); err != nil {
		return err
	}
	if runTaskID != taskID {
		return errors.New("personal: schedule suggestion request belongs to another task run")
	}
	return nil
}

type preparedScheduleSuggestion struct {
	MemoryRefs   []ScheduleMemoryRef
	Replacements []ContextReplacement
	NewContext   string
	ChangeKey    string
}

type placedReplacement struct {
	start int
	end   int
	value ContextReplacement
}

func prepareScheduleSuggestion(ctx context.Context, tx *sql.Tx, schedule Schedule, proposal ScheduleContextProposal) (preparedScheduleSuggestion, error) {
	var prepared preparedScheduleSuggestion
	if proposal.ScheduleRevision <= 0 || proposal.ScheduleRevision != schedule.Revision {
		return prepared, errors.New("personal: schedule suggestion requires the current schedule revision")
	}
	if len(proposal.MemoryRefs) == 0 || len(proposal.MemoryRefs) > maxScheduleSuggestionItems || len(proposal.Replacements) == 0 || len(proposal.Replacements) > maxScheduleSuggestionItems {
		return prepared, fmt.Errorf("personal: a schedule suggestion must contain 1..%d memory references and replacements", maxScheduleSuggestionItems)
	}
	memories := make(map[string]Memory, len(proposal.MemoryRefs))
	usedRefs := make(map[string]bool, len(proposal.MemoryRefs))
	for _, ref := range proposal.MemoryRefs {
		if !validMemoryKey(ref.Key) || ref.Version <= 0 || strings.TrimSpace(ref.Evidence) == "" || runeLen(ref.Evidence) > maxMemoryValueChars {
			return prepared, errors.New("personal: schedule suggestion contains an invalid memory reference")
		}
		if _, exists := memories[ref.Key]; exists {
			return prepared, fmt.Errorf("personal: duplicate schedule suggestion memory key %q", ref.Key)
		}
		memory, err := scanMemory(tx.QueryRowContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memories WHERE key=?`, ref.Key))
		if errors.Is(err, sql.ErrNoRows) {
			return prepared, fmt.Errorf("personal: schedule suggestion memory %q does not exist", ref.Key)
		}
		if err != nil {
			return prepared, err
		}
		if memory.Deleted || memory.Version != ref.Version {
			return prepared, fmt.Errorf("personal: schedule suggestion memory %q is stale", ref.Key)
		}
		if !strings.Contains(memory.Value, ref.Evidence) {
			return prepared, fmt.Errorf("personal: suggestion evidence for %q must be an exact quote from the current memory value, not the callback event", ref.Key)
		}
		if runeLen(memory.Value) > maxMemoryValueChars {
			return prepared, fmt.Errorf("personal: schedule suggestion memory %q exceeds the value limit", ref.Key)
		}
		memories[ref.Key] = *memory
	}

	placed := make([]placedReplacement, 0, len(proposal.Replacements))
	for _, replacement := range proposal.Replacements {
		if strings.TrimSpace(replacement.Old) == "" || runeLen(replacement.Old) > maxTaskChars {
			return prepared, errors.New("personal: schedule suggestion old text is required and too long")
		}
		_, exists := memories[replacement.MemoryKey]
		if !exists {
			return prepared, fmt.Errorf("personal: replacement refers to unlisted memory key %q", replacement.MemoryKey)
		}
		if strings.TrimSpace(replacement.New) == "" || runeLen(replacement.New) > maxTaskChars {
			return prepared, fmt.Errorf("personal: replacement for %q must be non-empty and within the text limit", replacement.MemoryKey)
		}
		start, ok := uniqueSubstring(schedule.Context, replacement.Old)
		if !ok {
			return prepared, errors.New("personal: schedule suggestion old text must occur exactly once in the current context")
		}
		usedRefs[replacement.MemoryKey] = true
		placed = append(placed, placedReplacement{start: start, end: start + len(replacement.Old), value: replacement})
	}
	for key := range memories {
		if !usedRefs[key] {
			return prepared, fmt.Errorf("personal: memory reference %q is not used by a replacement", key)
		}
	}
	sort.Slice(placed, func(i, j int) bool { return placed[i].start < placed[j].start })
	for i := 1; i < len(placed); i++ {
		if placed[i].start < placed[i-1].end {
			return prepared, errors.New("personal: schedule suggestion replacements must not overlap")
		}
	}
	prepared.MemoryRefs = make([]ScheduleMemoryRef, 0, len(memories))
	for _, ref := range proposal.MemoryRefs {
		prepared.MemoryRefs = append(prepared.MemoryRefs, ref)
	}
	sort.Slice(prepared.MemoryRefs, func(i, j int) bool { return prepared.MemoryRefs[i].Key < prepared.MemoryRefs[j].Key })
	prepared.Replacements = make([]ContextReplacement, 0, len(placed))
	var newContext strings.Builder
	last := 0
	for _, item := range placed {
		newContext.WriteString(schedule.Context[last:item.start])
		newContext.WriteString(item.value.New)
		last = item.end
		prepared.Replacements = append(prepared.Replacements, item.value)
	}
	newContext.WriteString(schedule.Context[last:])
	prepared.NewContext = newContext.String()
	if prepared.NewContext == schedule.Context {
		return prepared, errors.New("personal: schedule suggestion does not change the current context")
	}
	if err := validateScheduleDefinition(schedule.Title, schedule.Goal, prepared.NewContext, schedule.WindowMode, schedule.RequiredSources); err != nil {
		return prepared, err
	}
	prepared.ChangeKey = scheduleSuggestionChangeKey(prepared.Replacements, memories)
	return prepared, nil
}

func uniqueSubstring(text, fragment string) (int, bool) {
	if fragment == "" {
		return 0, false
	}
	first, last := strings.Index(text, fragment), strings.LastIndex(text, fragment)
	return first, first >= 0 && first == last
}

func scheduleSuggestionChangeKey(replacements []ContextReplacement, memories map[string]Memory) string {
	type change struct {
		MemoryKey string `json:"memory_key"`
		Old       string `json:"old"`
		NewFact   string `json:"new_fact"`
	}
	changes := make([]change, 0, len(replacements))
	for _, replacement := range replacements {
		changes = append(changes, change{MemoryKey: replacement.MemoryKey, Old: replacement.Old, NewFact: memories[replacement.MemoryKey].Value})
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].MemoryKey != changes[j].MemoryKey {
			return changes[i].MemoryKey < changes[j].MemoryKey
		}
		if changes[i].Old != changes[j].Old {
			return changes[i].Old < changes[j].Old
		}
		return changes[i].NewFact < changes[j].NewFact
	})
	encoded, _ := json.Marshal(changes)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func scheduleSuggestionMemoryValues(ctx context.Context, tx *sql.Tx, suggestion ScheduleSuggestion) (map[string]Memory, []ScheduleMemoryRef, bool, error) {
	memories := make(map[string]Memory, len(suggestion.MemoryRefs))
	refs := make([]ScheduleMemoryRef, 0, len(suggestion.MemoryRefs))
	for _, ref := range suggestion.MemoryRefs {
		if !validMemoryKey(ref.Key) {
			return nil, nil, false, errors.New("personal: stored schedule suggestion has an invalid memory key")
		}
		if _, duplicate := memories[ref.Key]; duplicate {
			return nil, nil, false, errors.New("personal: stored schedule suggestion has duplicate memory references")
		}
		memory, err := scanMemory(tx.QueryRowContext(ctx, `SELECT key,value,version,source_event_id,deleted,updated_at FROM personal_memories WHERE key=?`, ref.Key))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, false, nil
		}
		if err != nil {
			return nil, nil, false, err
		}
		if memory.Deleted || memory.Value == "" || runeLen(memory.Value) > maxMemoryValueChars {
			return nil, nil, false, nil
		}
		evidence := ref.Evidence
		if evidence == "" || !strings.Contains(memory.Value, evidence) {
			evidence = memory.Value
		}
		memories[ref.Key] = *memory
		refs = append(refs, ScheduleMemoryRef{Key: ref.Key, Version: memory.Version, Evidence: evidence})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
	return memories, refs, true, nil
}

func prepareRefreshedSuggestion(schedule Schedule, suggestion ScheduleSuggestion, memories map[string]Memory, refs []ScheduleMemoryRef) (preparedScheduleSuggestion, bool, bool) {
	var prepared preparedScheduleSuggestion
	allOldMissing, allOldNewPresent := true, true
	hasLocatedOld, hasMissingOld := false, false
	originalVersions := make(map[string]int64, len(suggestion.MemoryRefs))
	for _, ref := range suggestion.MemoryRefs {
		originalVersions[ref.Key] = ref.Version
	}
	positions := make([]placedReplacement, 0, len(suggestion.Replacements))
	for _, replacement := range suggestion.Replacements {
		memory, ok := memories[replacement.MemoryKey]
		if !ok {
			return prepared, false, true
		}
		newText := replacement.New
		if originalVersions[replacement.MemoryKey] != memory.Version {
			newText = memory.Value
		}
		if strings.TrimSpace(newText) == "" || runeLen(newText) > maxTaskChars {
			return prepared, false, true
		}
		start, unique := uniqueSubstring(schedule.Context, replacement.Old)
		if unique {
			hasLocatedOld = true
			allOldMissing = false
			allOldNewPresent = false
			positions = append(positions, placedReplacement{start: start, end: start + len(replacement.Old), value: ContextReplacement{Old: replacement.Old, New: newText, MemoryKey: replacement.MemoryKey}})
			continue
		}
		if strings.Contains(schedule.Context, replacement.Old) {
			return prepared, false, true
		}
		hasMissingOld = true
		if _, ok := uniqueSubstring(schedule.Context, newText); !ok {
			allOldNewPresent = false
		}
	}
	if allOldMissing && allOldNewPresent {
		return prepared, true, false
	}
	if allOldMissing {
		return prepared, false, true
	}
	if hasLocatedOld && hasMissingOld {
		return prepared, false, true
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i].start < positions[j].start })
	for i := 1; i < len(positions); i++ {
		if positions[i].start < positions[i-1].end {
			return prepared, false, true
		}
	}
	used := make(map[string]bool, len(positions))
	var newContext strings.Builder
	last := 0
	for _, position := range positions {
		newContext.WriteString(schedule.Context[last:position.start])
		if position.value.Old == position.value.New {
			newContext.WriteString(position.value.Old)
		} else {
			newContext.WriteString(position.value.New)
			prepared.Replacements = append(prepared.Replacements, position.value)
			used[position.value.MemoryKey] = true
		}
		last = position.end
	}
	newContext.WriteString(schedule.Context[last:])
	prepared.NewContext = newContext.String()
	if len(prepared.Replacements) == 0 || prepared.NewContext == schedule.Context {
		return prepared, true, false
	}
	filteredRefs := make([]ScheduleMemoryRef, 0, len(refs))
	filteredMemories := make(map[string]Memory, len(used))
	for _, ref := range refs {
		if used[ref.Key] {
			filteredRefs = append(filteredRefs, ref)
			filteredMemories[ref.Key] = memories[ref.Key]
		}
	}
	prepared.MemoryRefs = filteredRefs
	if len(prepared.MemoryRefs) == 0 {
		return prepared, false, true
	}
	if runeLen(prepared.NewContext) > maxTaskChars {
		return prepared, false, true
	}
	prepared.ChangeKey = scheduleSuggestionChangeKey(prepared.Replacements, filteredMemories)
	return prepared, false, false
}

func updatePendingScheduleSuggestion(ctx context.Context, tx *sql.Tx, current ScheduleSuggestion, originEventID, taskID, scheduleRevision int64, oldContext string, prepared preparedScheduleSuggestion, presentedEventID int64, bump bool, now time.Time) (ScheduleSuggestion, error) {
	revision := current.Revision
	changed := current.ScheduleRevision != scheduleRevision || current.OldContext != oldContext || current.NewContext != prepared.NewContext ||
		!slices.Equal(current.MemoryRefs, prepared.MemoryRefs) || !slices.Equal(current.Replacements, prepared.Replacements)
	if bump || changed {
		if revision == math.MaxInt64 {
			return ScheduleSuggestion{}, errors.New("personal: schedule suggestion revision exhausted")
		}
		revision++
	}
	refs, err := json.Marshal(prepared.MemoryRefs)
	if err != nil {
		return ScheduleSuggestion{}, err
	}
	replacements, err := json.Marshal(prepared.Replacements)
	if err != nil {
		return ScheduleSuggestion{}, err
	}
	presented := current.PresentedEventID
	if presentedEventID > 0 {
		presented = presentedEventID
	} else if changed && current.NotifiedEventID == 0 {
		presented = 0
	}
	result, err := tx.ExecContext(ctx, `UPDATE personal_schedule_context_suggestions SET revision=?,schedule_revision=?,origin_event_id=?,task_id=?,memory_refs_json=?,old_context=?,new_context=?,replacements_json=?,presented_event_id=?,updated_at=? WHERE id=? AND status='pending'`,
		revision, scheduleRevision, originEventID, taskID, string(refs), oldContext, prepared.NewContext, string(replacements), nullableID(presented), timestamp(now), current.ID)
	if err != nil {
		return ScheduleSuggestion{}, err
	}
	if err := requireSuggestionUpdated(result, nil); err != nil {
		return ScheduleSuggestion{}, err
	}
	updated, err := scanScheduleSuggestion(tx.QueryRowContext(ctx, `SELECT `+suggestionColumns+` FROM personal_schedule_context_suggestions WHERE id=?`, current.ID))
	if err != nil {
		return ScheduleSuggestion{}, err
	}
	return *updated, nil
}

func insertScheduleSuggestion(ctx context.Context, tx *sql.Tx, suggestion ScheduleSuggestion) (ScheduleSuggestion, error) {
	refs, err := json.Marshal(suggestion.MemoryRefs)
	if err != nil {
		return ScheduleSuggestion{}, err
	}
	replacements, err := json.Marshal(suggestion.Replacements)
	if err != nil {
		return ScheduleSuggestion{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO personal_schedule_context_suggestions(revision,schedule_id,schedule_revision,origin_event_id,task_id,change_key,memory_refs_json,old_context,new_context,replacements_json,status,presented_event_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,'pending',?,?,?)`, suggestion.Revision, suggestion.ScheduleID, suggestion.ScheduleRevision,
		suggestion.OriginEventID, suggestion.TaskID, suggestion.ChangeKey, string(refs), suggestion.OldContext, suggestion.NewContext,
		string(replacements), nullableID(suggestion.PresentedEventID), timestamp(suggestion.CreatedAt), timestamp(suggestion.UpdatedAt))
	if err != nil {
		return ScheduleSuggestion{}, err
	}
	suggestion.ID, err = result.LastInsertId()
	return suggestion, err
}

func findScheduleSuggestion(ctx context.Context, tx *sql.Tx, scheduleID int64, changeKey string) (*ScheduleSuggestion, error) {
	suggestion, err := scanScheduleSuggestion(tx.QueryRowContext(ctx, `SELECT `+suggestionColumns+` FROM personal_schedule_context_suggestions WHERE schedule_id=? AND change_key=?`, scheduleID, changeKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return suggestion, err
}

func (s *Store) ProposeScheduleContextUpdate(ctx context.Context, eventID int64, key string, taskID int64, proposal ScheduleContextProposal, now time.Time) (ScheduleSuggestion, bool, error) {
	if !requestKeyValid(key) || taskID <= 0 || now.IsZero() {
		return ScheduleSuggestion{}, false, errors.New("personal: schedule suggestion requires a request key, task ID and timestamp")
	}
	payload, err := json.Marshal(scheduleSuggestionProposalRequest{TaskID: taskID, Proposal: proposal})
	if err != nil {
		return ScheduleSuggestion{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScheduleSuggestion{}, false, err
	}
	defer tx.Rollback()
	if err := requireScheduleTaskEventOwner(ctx, tx, eventID, taskID); err != nil {
		return ScheduleSuggestion{}, false, err
	}
	var replay ScheduleSuggestion
	replayedScheduleID, replayed, err := previousScheduleSuggestionRequest(ctx, tx, eventID, key, "propose_schedule_context_update", string(payload), &replay)
	if err != nil {
		return ScheduleSuggestion{}, false, err
	}
	if replayed {
		if replay.ScheduleID != replayedScheduleID {
			return ScheduleSuggestion{}, false, errors.New("personal: saved schedule suggestion request has an inconsistent schedule ID")
		}
		if err := commitStoreOutcome(ctx, tx, "schedule.suggestion.propose", "replayed", replay,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", replay.ScheduleID), zap.Int64("task_id", taskID), zap.Int64("suggestion_id", replay.ID)); err != nil {
			return ScheduleSuggestion{}, false, err
		}
		return replay, true, nil
	}
	callback, err := currentScheduleCallback(ctx, tx, eventID, taskID)
	if err != nil {
		return ScheduleSuggestion{}, false, err
	}
	prepared, err := prepareScheduleSuggestion(ctx, tx, callback.Schedule, proposal)
	if err != nil {
		return ScheduleSuggestion{}, false, err
	}
	now = now.UTC()
	suggestion, err := findScheduleSuggestion(ctx, tx, callback.ScheduleID, prepared.ChangeKey)
	if err != nil {
		return ScheduleSuggestion{}, false, err
	}
	outcome := "created"
	if suggestion == nil {
		created, createErr := insertScheduleSuggestion(ctx, tx, ScheduleSuggestion{
			Revision: 1, ScheduleID: callback.ScheduleID, ScheduleRevision: callback.Schedule.Revision,
			OriginEventID: eventID, TaskID: taskID, ChangeKey: prepared.ChangeKey, MemoryRefs: prepared.MemoryRefs,
			OldContext: callback.Schedule.Context, NewContext: prepared.NewContext, Replacements: prepared.Replacements,
			Status: "pending", CreatedAt: now, UpdatedAt: now,
		})
		if createErr != nil {
			return ScheduleSuggestion{}, false, createErr
		}
		suggestion = &created
	} else if suggestion.Status != "pending" || suggestion.NotifiedEventID != 0 {
		outcome = "deduplicated"
	} else {
		outcome = "transferred"
		updated, err := updatePendingScheduleSuggestion(ctx, tx, *suggestion, eventID, taskID, callback.Schedule.Revision,
			callback.Schedule.Context, prepared, 0, false, now)
		if err != nil {
			return ScheduleSuggestion{}, false, err
		}
		suggestion = &updated
	}
	if err := insertScheduleSuggestionRequest(ctx, tx, eventID, key, "propose_schedule_context_update", string(payload), callback.ScheduleID, *suggestion); err != nil {
		return ScheduleSuggestion{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.suggestion.propose", outcome, *suggestion,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", callback.ScheduleID), zap.Int64("task_id", taskID), zap.Int64("suggestion_id", suggestion.ID)); err != nil {
		return ScheduleSuggestion{}, false, err
	}
	return *suggestion, false, nil
}

func (s *Store) ScheduleSuggestions(ctx context.Context, scheduleID int64) ([]ScheduleSuggestion, error) {
	if scheduleID <= 0 {
		return nil, errors.New("personal: schedule ID must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+suggestionColumns+` FROM personal_schedule_context_suggestions WHERE schedule_id=? AND status='pending' ORDER BY id DESC`, scheduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	suggestions := make([]ScheduleSuggestion, 0)
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

func (s *Store) ReplyScheduleSuggestions(ctx context.Context, outboxID int64) ([]ScheduleSuggestion, error) {
	if outboxID <= 0 {
		return nil, errors.New("personal: outbox ID must be positive")
	}
	columns := "s." + strings.ReplaceAll(suggestionColumns, ",", ",s.")
	suggestion, err := scanScheduleSuggestion(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM personal_outbox d
		JOIN personal_schedule_context_suggestions s ON d.suggestion_id=s.id AND s.presented_event_id=d.event_id
		WHERE d.id=? AND d.status='sent'`, outboxID))
	if errors.Is(err, sql.ErrNoRows) {
		return []ScheduleSuggestion{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []ScheduleSuggestion{*suggestion}, nil
}

func scheduleSuggestionDisplayed(ctx context.Context, tx *sql.Tx, eventID int64, suggestion ScheduleSuggestion) error {
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

func currentSuggestionIsValid(ctx context.Context, tx *sql.Tx, schedule Schedule, suggestion ScheduleSuggestion) (bool, error) {
	if schedule.Status == ScheduleCancelled || schedule.Revision != suggestion.ScheduleRevision || schedule.Context != suggestion.OldContext {
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

func rebuildStoredScheduleSuggestion(contextText string, replacements []ContextReplacement) (preparedScheduleSuggestion, error) {
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

func markSuggestionDismissed(ctx context.Context, tx *sql.Tx, suggestion ScheduleSuggestion, now time.Time) (ScheduleSuggestion, error) {
	result, err := tx.ExecContext(ctx, `UPDATE personal_schedule_context_suggestions SET status='dismissed',updated_at=? WHERE id=? AND status='pending'`, timestamp(now), suggestion.ID)
	if err := requireSuggestionUpdated(result, err); err != nil {
		return ScheduleSuggestion{}, err
	}
	suggestion.Status, suggestion.UpdatedAt = "dismissed", now
	return suggestion, nil
}

func setSuggestionPresented(ctx context.Context, tx *sql.Tx, suggestion ScheduleSuggestion, eventID int64, now time.Time) (ScheduleSuggestion, error) {
	result, err := tx.ExecContext(ctx, `UPDATE personal_schedule_context_suggestions SET presented_event_id=?,updated_at=? WHERE id=? AND status='pending'`, eventID, timestamp(now), suggestion.ID)
	if err := requireSuggestionUpdated(result, err); err != nil {
		return ScheduleSuggestion{}, err
	}
	suggestion.PresentedEventID, suggestion.UpdatedAt = eventID, now
	return suggestion, nil
}

func refreshScheduleSuggestion(ctx context.Context, tx *sql.Tx, eventID int64, schedule Schedule, suggestion ScheduleSuggestion, now time.Time) (ScheduleSuggestionResolution, error) {
	resolution := ScheduleSuggestionResolution{Status: "stale", Suggestion: suggestion, Schedule: &schedule}
	if schedule.Status == ScheduleCancelled {
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
		created, createErr := insertScheduleSuggestion(ctx, tx, ScheduleSuggestion{
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

func (s *Store) ResolveScheduleContextUpdate(ctx context.Context, eventID int64, key string, suggestionID, expectedRevision int64, decision string, now time.Time) (ScheduleSuggestionResolution, bool, error) {
	if !requestKeyValid(key) || suggestionID <= 0 || expectedRevision <= 0 || now.IsZero() || (decision != "apply" && decision != "keep") {
		return ScheduleSuggestionResolution{}, false, errors.New("personal: suggestion ID, revision, request key, timestamp and apply/keep decision are required")
	}
	payload, err := json.Marshal(scheduleSuggestionResolutionRequest{SuggestionID: suggestionID, ExpectedRevision: expectedRevision, Decision: decision})
	if err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, true); err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	var replay ScheduleSuggestionResolution
	replayedScheduleID, replayed, err := previousScheduleSuggestionRequest(ctx, tx, eventID, key, "resolve_schedule_context_update", string(payload), &replay)
	if err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	if replayed {
		if replay.Suggestion.ScheduleID != replayedScheduleID {
			return ScheduleSuggestionResolution{}, false, errors.New("personal: saved schedule suggestion resolution has an inconsistent schedule ID")
		}
		if err := commitStoreOutcome(ctx, tx, "schedule.suggestion.resolve", "replayed", replay,
			zap.Int64("event_id", eventID), zap.Int64("schedule_id", replayedScheduleID), zap.Int64("suggestion_id", suggestionID)); err != nil {
			return ScheduleSuggestionResolution{}, false, err
		}
		return replay, true, nil
	}
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	suggestion, err := scanScheduleSuggestion(tx.QueryRowContext(ctx, `SELECT `+suggestionColumns+` FROM personal_schedule_context_suggestions WHERE id=?`, suggestionID))
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleSuggestionResolution{}, false, errors.New("personal: schedule suggestion does not exist")
	}
	if err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	if suggestion.Revision != expectedRevision {
		return ScheduleSuggestionResolution{}, false, fmt.Errorf("personal: schedule suggestion revision is %d, expected %d", suggestion.Revision, expectedRevision)
	}
	if suggestion.Status != "pending" {
		return ScheduleSuggestionResolution{}, false, fmt.Errorf("personal: schedule suggestion is already %s", suggestion.Status)
	}
	schedule, err := scanSchedule(tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM personal_schedules WHERE id=?`, suggestion.ScheduleID))
	if err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	now = now.UTC()
	resolution := ScheduleSuggestionResolution{Suggestion: *suggestion, Schedule: schedule}
	if decision == "keep" {
		resolution.Status = "dismissed"
		resolution.Suggestion, err = markSuggestionDismissed(ctx, tx, *suggestion, now)
	} else {
		if err := scheduleSuggestionDisplayed(ctx, tx, eventID, *suggestion); err != nil {
			return ScheduleSuggestionResolution{}, false, err
		}
		var valid bool
		valid, err = currentSuggestionIsValid(ctx, tx, *schedule, *suggestion)
		if err != nil {
			return ScheduleSuggestionResolution{}, false, err
		}
		if !valid {
			resolution, err = refreshScheduleSuggestion(ctx, tx, eventID, *schedule, *suggestion, now)
		} else {
			contextText := suggestion.NewContext
			updated, updateErr := s.updateScheduleTx(ctx, tx, eventID, suggestion.ScheduleID, schedule.Revision,
				SchedulePatch{Context: &contextText}, now)
			if updateErr != nil {
				return ScheduleSuggestionResolution{}, false, updateErr
			}
			result, err := tx.ExecContext(ctx, `UPDATE personal_schedule_context_suggestions SET status='applied',updated_at=? WHERE id=? AND status='pending'`, timestamp(now), suggestion.ID)
			if err := requireSuggestionUpdated(result, err); err != nil {
				return ScheduleSuggestionResolution{}, false, err
			}
			suggestion.Status, suggestion.UpdatedAt = "applied", now
			resolution = ScheduleSuggestionResolution{Status: "applied", Suggestion: *suggestion, Schedule: &updated}
		}
	}
	if err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	if err := insertScheduleSuggestionRequest(ctx, tx, eventID, key, "resolve_schedule_context_update", string(payload), resolution.Suggestion.ScheduleID, resolution); err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	if err := commitStoreOutcome(ctx, tx, "schedule.suggestion.resolve", resolution.Status, resolution,
		zap.Int64("event_id", eventID), zap.Int64("schedule_id", resolution.Suggestion.ScheduleID), zap.Int64("suggestion_id", resolution.Suggestion.ID)); err != nil {
		return ScheduleSuggestionResolution{}, false, err
	}
	return resolution, false, nil
}
