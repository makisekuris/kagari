package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/textpage"
)

func TestQueryTasksSnapshotCursorExcludesNewMatches(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "tasks.db"), time.Minute)
	event := openUserEvent(t, s, 1, 1, "Find old tasks")
	oldIDs := make(map[int64]bool, 51)
	for i := range 51 {
		task, _, err := s.CreateTask(ctx, event.ID, fmt.Sprintf("old-%d", i), fmt.Sprintf("needle old %02d", i), personal.TaskInput{Goal: "needle"})
		if err != nil {
			t.Fatal(err)
		}
		oldIDs[task.ID] = true
	}

	first, err := s.QueryTasks(ctx, personal.ReadScope{EventID: event.ID}, personal.TaskQuery{ListPage: personal.ListPage{Limit: 50}, Query: "needle"})
	if err != nil || len(first.Tasks) != 50 || !first.HasMore || first.SnapshotMaxID == 0 || first.NextBeforeID == 0 {
		t.Fatalf("first page=%+v err=%v", first, err)
	}
	late, _, err := s.CreateTask(ctx, event.ID, "late", "needle inserted after page one", personal.TaskInput{Goal: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.QueryTasks(ctx, personal.ReadScope{EventID: event.ID}, personal.TaskQuery{
		ListPage: personal.ListPage{Limit: 50, BeforeID: first.NextBeforeID, SnapshotMaxID: first.SnapshotMaxID, QueryKey: first.QueryKey},
		Query:    "needle",
	})
	if err != nil || len(second.Tasks) != 1 || second.HasMore || second.SnapshotMaxID != first.SnapshotMaxID || second.QueryKey != first.QueryKey {
		t.Fatalf("second page=%+v err=%v", second, err)
	}
	seen := make(map[int64]bool, 51)
	for _, task := range append(first.Tasks, second.Tasks...) {
		if !oldIDs[task.ID] || task.ID == late.ID || seen[task.ID] || !strings.Contains(task.Title, "needle") {
			t.Fatalf("unexpected task in snapshot results: %+v", task)
		}
		seen[task.ID] = true
	}
	if len(seen) != len(oldIDs) || late.ID <= first.SnapshotMaxID {
		t.Fatalf("snapshot returned %d/%d old tasks; late id=%d snapshot=%d", len(seen), len(oldIDs), late.ID, first.SnapshotMaxID)
	}
	finishEvent(t, s, event, personal.TurnOutput{}, nil)
}

func TestSavedSourceSurvivesRecoveryOnTheSameRun(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "recovery.db"), time.Minute)
	seq := int64(10)
	run := acceptanceRunningTask(t, s, &seq, "recovery", personal.TaskInput{Goal: "Keep evidence across recovery"})
	source := domain.Source{URL: "https://example.org/recovery", Title: "Saved before recovery", Status: "ok", FetchedAt: time.Now().UTC(), Content: "persisted evidence"}
	ref, err := s.SaveSource(ctx, run.ID, "before-recovery", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || reclaimed == nil || reclaimed.ID != run.ID || reclaimed.Status != personal.RunRunning {
		t.Fatalf("reclaimed run=%+v err=%v, want run %d", reclaimed, err, run.ID)
	}
	got, err := s.ExpandSource(ctx, ref.ID)
	if err != nil || got.ID != ref.ID || got.Content != source.Content || got.URL != source.URL {
		t.Fatalf("source after recovery=%+v err=%v", got, err)
	}
}

func TestSchemaThreeDatabaseIsRejectedWithoutChangingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema-three.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE personal_schema(version INTEGER PRIMARY KEY); INSERT INTO personal_schema VALUES(3); CREATE TABLE personal_legacy_data(value TEXT); INSERT INTO personal_legacy_data VALUES('keep me')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path, time.Minute); err == nil || !strings.Contains(err.Error(), "unsupported database schema version 3") {
		t.Fatalf("NewStore error=%v, want schema version 3 rejection", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("opening the schema 3 database changed its bytes")
	}
}

func TestFinishStageStoresCanonicalAuthorizedSourceReferences(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "canonical.db"), time.Minute)
	seq := int64(20)
	current := acceptanceRunningTask(t, s, &seq, "canonical-current", personal.TaskInput{Goal: "Use only my saved source"})
	foreign := acceptanceRunningTask(t, s, &seq, "canonical-foreign", personal.TaskInput{Goal: "Separate source"})
	fetchedAt := time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC)
	currentRef, err := s.SaveSource(ctx, current.ID, "current-source", domain.Source{
		URL: "https://example.org/canonical", Title: "Canonical title", Status: "incomplete", Truncated: true,
		FetchedAt: fetchedAt, Content: "canonical body",
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignRef, err := s.SaveSource(ctx, foreign.ID, "foreign-source", domain.Source{
		URL: "https://example.org/foreign", Title: "Foreign title", Status: "ok", FetchedAt: fetchedAt, Content: "foreign body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, current.ID, personal.StageResult{Summary: "should roll back", Sources: []domain.SourceRef{foreignRef}}, ""); err == nil {
		t.Fatal("FinishStage accepted a material owned by another run")
	}
	unchanged, err := s.Run(ctx, current.ID)
	if err != nil || unchanged.Status != personal.RunRunning {
		t.Fatalf("unauthorized source changed current run: run=%+v err=%v", unchanged, err)
	}

	forged := currentRef
	forged.RunID = foreign.ID
	forged.URL = "https://attacker.invalid/"
	forged.Title = "forged title"
	forged.Status = "failed"
	forged.FetchedAt = time.Time{}
	forged.SourceTruncated = false
	forged.TotalRunes = 999
	if err := s.FinishStage(ctx, current.ID, personal.StageResult{Summary: "finished", Sources: []domain.SourceRef{forged}}, ""); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Run(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result personal.StageResult
	if err := json.Unmarshal([]byte(saved.Result), &result); err != nil {
		t.Fatal(err)
	}
	want := currentRef
	if len(result.Sources) != 1 || !reflect.DeepEqual(result.Sources[0], want) {
		t.Fatalf("stored source ref=%+v, want canonical %+v", result.Sources, want)
	}
}

func TestTaskReferencesInheritClearAndStayNarrowAcrossCallbackContinue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, filepath.Join(t.TempDir(), "references.db"), time.Minute)
	seq := int64(100)
	externalA, refA := acceptanceCompletedSource(t, s, &seq, "external-a")
	externalB, refB := acceptanceCompletedSource(t, s, &seq, "external-b")

	memoryEvent := acceptanceOpenEvent(t, s, &seq, "Remember a concise style")
	if _, err := s.WriteMemory(ctx, memoryEvent.ID, "remember-style", personal.MemoryChange{
		Operation: "put", Key: "answer_style", Value: "concise", Evidence: memoryEvent.Text,
	}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, memoryEvent, personal.TurnOutput{}, nil)

	createEvent := acceptanceOpenEvent(t, s, &seq, "Create the target task")
	task, _, err := s.CreateTask(ctx, createEvent.ID, "target", "target", personal.TaskInput{
		Goal: "Use associated evidence", SourceRunIDs: []int64{externalA.ID}, MemoryRefs: []string{"answer_style"},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, createEvent, personal.TurnOutput{}, nil)

	reviseEvent := acceptanceOpenEvent(t, s, &seq, "Revise without changing evidence references")
	inherited, inheritedRun, err := s.ReviseTask(ctx, reviseEvent.ID, "inherit", task.ID, task.Revision, personal.TaskInput{Goal: "Updated goal"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, reviseEvent, personal.TurnOutput{}, nil)
	if !reflect.DeepEqual(inherited.SourceRunIDs, []int64{externalA.ID}) || !reflect.DeepEqual(inherited.MemoryRefs, []string{"answer_style"}) {
		t.Fatalf("revision did not inherit references: %+v", inherited)
	}
	input := decodeAcceptanceTaskInput(t, inheritedRun.Input)
	if !reflect.DeepEqual(input.SourceRunIDs, []int64{externalA.ID}) || !reflect.DeepEqual(input.MemoryRefs, []string{"answer_style"}) {
		t.Fatalf("new revision input did not inherit references: %+v", input)
	}

	claimed, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimed == nil || claimed.ID != inheritedRun.ID {
		t.Fatalf("ClaimRun()=(%+v,%v), want %d", claimed, err, inheritedRun.ID)
	}
	if err := s.FinishStage(ctx, claimed.ID, personal.StageResult{Summary: "first review"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask || callback.TaskID != task.ID {
		t.Fatalf("task callback=(%+v,%v)", callback, err)
	}
	continued, continuedRun, err := s.ContinueTask(ctx, callback.ID, "continue-in-callback", task.ID, inherited.Revision, "Continue the same goal", false)
	if err != nil {
		t.Fatal(err)
	}
	continuedInput := decodeAcceptanceTaskInput(t, continuedRun.Input)
	if !reflect.DeepEqual(continued.SourceRunIDs, []int64{externalA.ID}) || !reflect.DeepEqual(continuedInput.SourceRunIDs, []int64{externalA.ID}) || !reflect.DeepEqual(continuedInput.MemoryRefs, []string{"answer_style"}) {
		t.Fatalf("callback widened or lost references: task=%+v input=%+v", continued, continuedInput)
	}
	finishEvent(t, s, *callback, personal.TurnOutput{}, nil)
	continuedActive, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || continuedActive == nil || continuedActive.ID != continuedRun.ID {
		t.Fatalf("continued ClaimRun()=(%+v,%v), want %d", continuedActive, err, continuedRun.ID)
	}
	if _, err := s.ReadMaterial(ctx, personal.ReadScope{RunID: continuedActive.ID}, refB.ID, "body", textpage.Request{}); err == nil {
		t.Fatalf("callback continuation gained access to unassociated run %d material %s", externalB.ID, refB.ID)
	}
	if _, err := s.ReadMaterial(ctx, personal.ReadScope{RunID: continuedActive.ID}, refA.ID, "body", textpage.Request{}); err != nil {
		t.Fatalf("inherited material access was lost: %v", err)
	}

	clearEvent := acceptanceOpenEvent(t, s, &seq, "Clear evidence references")
	cleared, _, err := s.ReviseTask(ctx, clearEvent.ID, "clear", task.ID, continued.Revision, personal.TaskInput{
		Goal: "Updated goal without external context", SourceRunIDs: []int64{}, MemoryRefs: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, clearEvent, personal.TurnOutput{}, nil)
	persisted, err := s.Task(ctx, task.ID)
	if err != nil || persisted == nil || len(persisted.SourceRunIDs) != 0 || len(persisted.MemoryRefs) != 0 || len(cleared.SourceRunIDs) != 0 || len(cleared.MemoryRefs) != 0 {
		t.Fatalf("explicit empty reference lists were not persisted: returned=%+v stored=%+v err=%v", cleared, persisted, err)
	}
}

func acceptanceOpenEvent(t *testing.T, s *Store, seq *int64, text string) personal.Event {
	t.Helper()
	value := *seq
	*seq = value + 1
	return openUserEvent(t, s, value, value, text)
}

func acceptanceRunningTask(t *testing.T, s *Store, seq *int64, key string, input personal.TaskInput) personal.Run {
	t.Helper()
	ctx := context.Background()
	event := acceptanceOpenEvent(t, s, seq, "Start "+key)
	if _, _, err := s.CreateTask(ctx, event.ID, key, key, input); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{}, nil)
	run, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatalf("ClaimRun()=(%+v,%v)", run, err)
	}
	return *run
}

func acceptanceCompletedSource(t *testing.T, s *Store, seq *int64, key string) (personal.Run, domain.SourceRef) {
	t.Helper()
	ctx := context.Background()
	run := acceptanceRunningTask(t, s, seq, key, personal.TaskInput{Goal: "Persist source " + key})
	ref, err := s.SaveSource(ctx, run.ID, key, domain.Source{
		URL: "https://example.org/" + key, Title: key, Status: "ok", FetchedAt: time.Now().UTC(), Content: "evidence for " + key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStage(ctx, run.ID, personal.StageResult{Summary: "Saved " + key, Sources: []domain.SourceRef{ref}}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != personal.EventTask {
		t.Fatalf("source callback=(%+v,%v)", callback, err)
	}
	finishEvent(t, s, *callback, personal.TurnOutput{}, nil)
	return run, ref
}

func decodeAcceptanceTaskInput(t *testing.T, raw string) personal.TaskInput {
	t.Helper()
	var input personal.TaskInput
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatal(err)
	}
	return input
}
