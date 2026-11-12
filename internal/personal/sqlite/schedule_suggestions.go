package sqlite

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
	"unicode/utf8"

	"kagari/internal/personal"
)

const scheduleSuggestionSchema = `
CREATE TABLE IF NOT EXISTS personal_schedule_context_suggestions (
	id INTEGER PRIMARY KEY,
	revision INTEGER NOT NULL CHECK(revision>0),
	schedule_id INTEGER NOT NULL REFERENCES personal_schedules(id),
	schedule_revision INTEGER NOT NULL CHECK(schedule_revision>0),
	origin_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	task_id INTEGER NOT NULL REFERENCES personal_tasks(id),
	change_key TEXT NOT NULL,
	memory_refs_json TEXT NOT NULL,
	old_context TEXT NOT NULL,
	new_context TEXT NOT NULL,
	replacements_json TEXT NOT NULL,
	status TEXT NOT NULL CHECK(status IN ('pending','applied','dismissed')),
	notified_event_id INTEGER REFERENCES personal_events(id),
	presented_event_id INTEGER REFERENCES personal_events(id),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(schedule_id,change_key)
);
`

const maxScheduleSuggestionItems = 8

const suggestionColumns = `id,revision,schedule_id,schedule_revision,origin_event_id,task_id,change_key,memory_refs_json,old_context,new_context,replacements_json,status,notified_event_id,presented_event_id,created_at,updated_at`

// Suggestion request identity and callback ownership.
func scanScheduleSuggestion(row scanner) (*personal.ScheduleSuggestion, error) {
	var suggestion personal.ScheduleSuggestion
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
		suggestion.MemoryRefs = []personal.ScheduleMemoryRef{}
	}
	if suggestion.Replacements == nil {
		suggestion.Replacements = []personal.ContextReplacement{}
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
	TaskID   int64                            `json:"task_id"`
	Proposal personal.ScheduleContextProposal `json:"proposal"`
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
	Schedule   personal.Schedule
}

func currentScheduleCallback(ctx context.Context, tx *sql.Tx, eventID, taskID int64) (scheduleCallback, error) {
	var callback scheduleCallback
	var kind personal.EventKind
	var status personal.EventStatus
	var actualTaskID, runID, eventScheduleID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT kind,status,task_id,run_id,schedule_id FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &status, &actualTaskID, &runID, &eventScheduleID); err != nil {
		return callback, err
	}
	if kind != personal.EventTask || status != personal.EventRunning || !actualTaskID.Valid || actualTaskID.Int64 != taskID || !runID.Valid {
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
	if schedule.Status == personal.ScheduleCancelled || schedule.ScopeRevision != occurrenceScopeRevision {
		return callback, errors.New("personal: schedule suggestion callback belongs to an outdated schedule definition")
	}
	return scheduleCallback{TaskID: taskID, Revision: revision, ScheduleID: occurrenceScheduleID, Schedule: *schedule}, nil
}

func requireScheduleTaskEventOwner(ctx context.Context, tx *sql.Tx, eventID, taskID int64) error {
	var kind personal.EventKind
	var eventTaskID, runID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT kind,task_id,run_id FROM personal_events WHERE id=?`, eventID).
		Scan(&kind, &eventTaskID, &runID); err != nil {
		return err
	}
	if kind != personal.EventTask || !eventTaskID.Valid || eventTaskID.Int64 != taskID || !runID.Valid {
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
	MemoryRefs   []personal.ScheduleMemoryRef
	Replacements []personal.ContextReplacement
	NewContext   string
	ChangeKey    string
}

type placedReplacement struct {
	start int
	end   int
	value personal.ContextReplacement
}

// Suggestion calculation and context replacement validation.
func prepareScheduleSuggestion(ctx context.Context, tx *sql.Tx, schedule personal.Schedule, proposal personal.ScheduleContextProposal) (preparedScheduleSuggestion, error) {
	var prepared preparedScheduleSuggestion
	if proposal.ScheduleRevision <= 0 || proposal.ScheduleRevision != schedule.Revision {
		return prepared, errors.New("personal: schedule suggestion requires the current schedule revision")
	}
	if len(proposal.MemoryRefs) == 0 || len(proposal.MemoryRefs) > maxScheduleSuggestionItems || len(proposal.Replacements) == 0 || len(proposal.Replacements) > maxScheduleSuggestionItems {
		return prepared, fmt.Errorf("personal: a schedule suggestion must contain 1..%d memory references and replacements", maxScheduleSuggestionItems)
	}
	memories := make(map[string]personal.Memory, len(proposal.MemoryRefs))
	usedRefs := make(map[string]bool, len(proposal.MemoryRefs))
	for _, ref := range proposal.MemoryRefs {
		if !validMemoryKey(ref.Key) || ref.Version <= 0 || strings.TrimSpace(ref.Evidence) == "" || utf8.RuneCountInString(ref.Evidence) > maxMemoryValueChars {
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
		if utf8.RuneCountInString(memory.Value) > maxMemoryValueChars {
			return prepared, fmt.Errorf("personal: schedule suggestion memory %q exceeds the value limit", ref.Key)
		}
		memories[ref.Key] = *memory
	}

	placed := make([]placedReplacement, 0, len(proposal.Replacements))
	for _, replacement := range proposal.Replacements {
		if strings.TrimSpace(replacement.Old) == "" || utf8.RuneCountInString(replacement.Old) > personal.MaxTaskChars {
			return prepared, errors.New("personal: schedule suggestion old text is required and too long")
		}
		_, exists := memories[replacement.MemoryKey]
		if !exists {
			return prepared, fmt.Errorf("personal: replacement refers to unlisted memory key %q", replacement.MemoryKey)
		}
		if strings.TrimSpace(replacement.New) == "" || utf8.RuneCountInString(replacement.New) > personal.MaxTaskChars {
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
	prepared.MemoryRefs = make([]personal.ScheduleMemoryRef, 0, len(memories))
	for _, ref := range proposal.MemoryRefs {
		prepared.MemoryRefs = append(prepared.MemoryRefs, ref)
	}
	sort.Slice(prepared.MemoryRefs, func(i, j int) bool { return prepared.MemoryRefs[i].Key < prepared.MemoryRefs[j].Key })
	prepared.Replacements = make([]personal.ContextReplacement, 0, len(placed))
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

func scheduleSuggestionChangeKey(replacements []personal.ContextReplacement, memories map[string]personal.Memory) string {
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

func scheduleSuggestionMemoryValues(ctx context.Context, tx *sql.Tx, suggestion personal.ScheduleSuggestion) (map[string]personal.Memory, []personal.ScheduleMemoryRef, bool, error) {
	memories := make(map[string]personal.Memory, len(suggestion.MemoryRefs))
	refs := make([]personal.ScheduleMemoryRef, 0, len(suggestion.MemoryRefs))
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
		if memory.Deleted || memory.Value == "" || utf8.RuneCountInString(memory.Value) > maxMemoryValueChars {
			return nil, nil, false, nil
		}
		evidence := ref.Evidence
		if evidence == "" || !strings.Contains(memory.Value, evidence) {
			evidence = memory.Value
		}
		memories[ref.Key] = *memory
		refs = append(refs, personal.ScheduleMemoryRef{Key: ref.Key, Version: memory.Version, Evidence: evidence})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
	return memories, refs, true, nil
}

func prepareRefreshedSuggestion(schedule personal.Schedule, suggestion personal.ScheduleSuggestion, memories map[string]personal.Memory, refs []personal.ScheduleMemoryRef) (preparedScheduleSuggestion, bool, bool) {
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
		if strings.TrimSpace(newText) == "" || utf8.RuneCountInString(newText) > personal.MaxTaskChars {
			return prepared, false, true
		}
		start, unique := uniqueSubstring(schedule.Context, replacement.Old)
		if unique {
			hasLocatedOld = true
			allOldMissing = false
			allOldNewPresent = false
			positions = append(positions, placedReplacement{start: start, end: start + len(replacement.Old), value: personal.ContextReplacement{Old: replacement.Old, New: newText, MemoryKey: replacement.MemoryKey}})
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
	filteredRefs := make([]personal.ScheduleMemoryRef, 0, len(refs))
	filteredMemories := make(map[string]personal.Memory, len(used))
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
	if utf8.RuneCountInString(prepared.NewContext) > personal.MaxTaskChars {
		return prepared, false, true
	}
	prepared.ChangeKey = scheduleSuggestionChangeKey(prepared.Replacements, filteredMemories)
	return prepared, false, false
}

// Suggestion persistence and user decisions.
func updatePendingScheduleSuggestion(ctx context.Context, tx *sql.Tx, current personal.ScheduleSuggestion, originEventID, taskID, scheduleRevision int64, oldContext string, prepared preparedScheduleSuggestion, presentedEventID int64, bump bool, now time.Time) (personal.ScheduleSuggestion, error) {
	revision := current.Revision
	changed := current.ScheduleRevision != scheduleRevision || current.OldContext != oldContext || current.NewContext != prepared.NewContext ||
		!slices.Equal(current.MemoryRefs, prepared.MemoryRefs) || !slices.Equal(current.Replacements, prepared.Replacements)
	if bump || changed {
		if revision == math.MaxInt64 {
			return personal.ScheduleSuggestion{}, errors.New("personal: schedule suggestion revision exhausted")
		}
		revision++
	}
	refs, err := json.Marshal(prepared.MemoryRefs)
	if err != nil {
		return personal.ScheduleSuggestion{}, err
	}
	replacements, err := json.Marshal(prepared.Replacements)
	if err != nil {
		return personal.ScheduleSuggestion{}, err
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
		return personal.ScheduleSuggestion{}, err
	}
	if err := requireSuggestionUpdated(result, nil); err != nil {
		return personal.ScheduleSuggestion{}, err
	}
	updated, err := scanScheduleSuggestion(tx.QueryRowContext(ctx, `SELECT `+suggestionColumns+` FROM personal_schedule_context_suggestions WHERE id=?`, current.ID))
	if err != nil {
		return personal.ScheduleSuggestion{}, err
	}
	return *updated, nil
}

func insertScheduleSuggestion(ctx context.Context, tx *sql.Tx, suggestion personal.ScheduleSuggestion) (personal.ScheduleSuggestion, error) {
	refs, err := json.Marshal(suggestion.MemoryRefs)
	if err != nil {
		return personal.ScheduleSuggestion{}, err
	}
	replacements, err := json.Marshal(suggestion.Replacements)
	if err != nil {
		return personal.ScheduleSuggestion{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO personal_schedule_context_suggestions(revision,schedule_id,schedule_revision,origin_event_id,task_id,change_key,memory_refs_json,old_context,new_context,replacements_json,status,presented_event_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,'pending',?,?,?)`, suggestion.Revision, suggestion.ScheduleID, suggestion.ScheduleRevision,
		suggestion.OriginEventID, suggestion.TaskID, suggestion.ChangeKey, string(refs), suggestion.OldContext, suggestion.NewContext,
		string(replacements), nullableID(suggestion.PresentedEventID), timestamp(suggestion.CreatedAt), timestamp(suggestion.UpdatedAt))
	if err != nil {
		return personal.ScheduleSuggestion{}, err
	}
	suggestion.ID, err = result.LastInsertId()
	return suggestion, err
}

func findScheduleSuggestion(ctx context.Context, tx *sql.Tx, scheduleID int64, changeKey string) (*personal.ScheduleSuggestion, error) {
	suggestion, err := scanScheduleSuggestion(tx.QueryRowContext(ctx, `SELECT `+suggestionColumns+` FROM personal_schedule_context_suggestions WHERE schedule_id=? AND change_key=?`, scheduleID, changeKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return suggestion, err
}
