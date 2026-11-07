package personal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"kagari/internal/inputbudget"
	"kagari/internal/logging"
)

func TestLivePersonalTaskInputPagingAndAcknowledgment(t *testing.T) {
	t.Setenv("KAGARI_MODEL_CONTEXT_WINDOW", "262144")
	engine := livePersonalEngine(t)
	if engine.Config.Model.ContextWindow != 262144 {
		t.Fatalf("controlled test context window=%d, want 262144", engine.Config.Model.ContextWindow)
	}
	engine.Config.Agent.MaxIterations = 12
	engine.Config.Agent.Timeout = 6 * time.Minute

	limits, err := inputbudget.NewLimits(engine.Config.Model.ContextWindow, int64(engine.Config.Model.MaxOutputTokens))
	if err != nil {
		t.Fatal("invalid test context limits")
	}
	var budgetMu sync.Mutex
	var budgetRecords []inputbudget.ModelRecord
	observedModel, err := inputbudget.NewAgenticModel(engine.Model, inputbudget.ModelConfig{
		Limits: limits,
		Observe: func(record inputbudget.ModelRecord) {
			budgetMu.Lock()
			budgetRecords = append(budgetRecords, record)
			budgetMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal("cannot add test-only context accounting")
	}
	engine.Model = observedModel
	t.Logf("test limits: context_window=%d max_output_tokens=%d count_basis=%s", limits.Window, limits.Output, inputbudget.CountBasis)

	logs, observed := observer.New(zapcore.InfoLevel)
	ctx, cancel := context.WithTimeout(logging.WithLogger(context.Background(), zap.New(logs)), 8*time.Minute)
	defer cancel()
	store := testStore(t, filepath.Join(t.TempDir(), "task-input-context.db"), 15*time.Minute)

	memoryEvent := openUserEvent(t, store, 900, 1900, "请为虚构项目 MICA 记住：交付摘要保持简洁。")
	memory, err := store.WriteMemory(ctx, memoryEvent.ID, "live-mica-memory", MemoryChange{
		Operation: "put", Key: "fiction.mica.concise_reports", Value: "虚构项目 MICA 的交付摘要应保持简洁。", Evidence: memoryEvent.Text,
	})
	if err != nil {
		t.Fatal("cannot seed fictional run memory")
	}
	finishEvent(t, store, memoryEvent, TurnOutput{Text: "Saved fictional test memory."}, nil)

	firstCode := liveInputCode(t)
	middleCode := liveInputCode(t)
	lastCode := liveInputCode(t)
	material := liveLongTaskInput(firstCode, middleCode, lastCode)
	event := openUserEvent(t, store, 901, 1901, material)
	task, initialRun, err := store.CreateTask(ctx, event.ID, "live-mica-task", "Verify fictional MICA record", TaskInput{
		Goal:       "Read the complete assigned MICA record and report its three verification codes exactly.",
		Context:    "All material is synthetic test data. Do not use external sources.",
		Step:       "Use get_task_input for the assigned event. Read offset 0 and follow every next_offset until has_more=false, using the returned version on later pages. Find MICA_FIRST_CODE, MICA_MIDDLE_CODE, and MICA_FINAL_CODE. After all pages are read, submit one progress update with status exactly `reported`, a non-empty ID and description containing all three exact code values, and no evidence references. The only valid statuses are reported, unresolved, failed, and superseded. Then call report_task_inputs for the event. The final summary must contain the three exact code values and say the full input was read. Do not copy the filler text.",
		MemoryRefs: []string{memory.Key},
	})
	if err != nil {
		t.Fatal("cannot create isolated fictional task")
	}
	if _, err := store.AddTaskInputs(ctx, event.ID, "live-mica-input", task.ID, task.Revision, []int64{event.ID}); err != nil {
		t.Fatal("cannot attach fictional input to the task")
	}
	finishEvent(t, store, event, TurnOutput{TaskID: task.ID}, nil)
	queued, err := store.Run(ctx, initialRun.ID)
	if err != nil || queued == nil {
		t.Fatal("cannot inspect queued test run")
	}
	var frozen TaskInput
	if err := json.Unmarshal([]byte(queued.Input), &frozen); err != nil {
		t.Fatal("cannot read frozen test input")
	}
	if len(frozen.MemoryRefs) != 1 || frozen.MemoryRefs[0] != memory.Key || len(frozen.InputEvents) != 1 || frozen.InputEvents[0].EventID != event.ID || frozen.InputEvents[0].TextRunes != runeLen(material) {
		t.Fatalf("test run did not freeze explicit memory/input references: memory_refs=%d input_events=%d text_runes=%d", len(frozen.MemoryRefs), len(frozen.InputEvents), func() int {
			if len(frozen.InputEvents) == 0 {
				return 0
			}
			return frozen.InputEvents[0].TextRunes
		}())
	}
	if strings.Contains(queued.Input, firstCode) || strings.Contains(queued.Input, middleCode) || strings.Contains(queued.Input, lastCode) {
		t.Fatal("frozen Run.Input copied the long material body")
	}

	run, err := store.ClaimRun(ctx, RunTask)
	if err != nil || run == nil || run.ID != initialRun.ID {
		t.Fatal("cannot claim isolated fictional task run")
	}
	traceStore := &liveTaskInputPageTraceStore{Store: store}
	execute := TaskExecutor(engine, traceStore, t.TempDir())
	started := time.Now()
	result, executeErr := execute(ctx, *run, func(string) error { return nil })
	elapsed := time.Since(started).Round(time.Millisecond)
	if executeErr != nil {
		t.Fatalf("live task stage failed (details withheld): %s", logging.ErrorReason(executeErr))
	}
	pageReads := traceStore.count()
	t.Logf("task stage duration=%s input_page_reads=%d input_runes=%d", elapsed, pageReads, runeLen(material))
	if pageReads < 2 {
		t.Fatalf("long assigned input was not paged: %d reads", pageReads)
	}
	for _, code := range []string{firstCode, middleCode, lastCode} {
		if !strings.Contains(result.Summary, code) {
			t.Fatal("final summary omitted a code from the complete synthetic input")
		}
		found := false
		for _, update := range result.Progress {
			if strings.Contains(update.Description, code) {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("structured progress omitted a code from the complete synthetic input")
		}
	}
	if len(result.Progress) == 0 {
		t.Fatal("model did not submit structured progress")
	}
	if len(result.AppliedInputIDs) != 1 || result.AppliedInputIDs[0] != event.ID {
		t.Fatalf("model did not report the assigned input as processed: reported=%d", len(result.AppliedInputIDs))
	}

	if err := store.FinishStage(ctx, run.ID, result, ""); err != nil {
		for _, update := range result.Progress {
			t.Logf("submitted progress fields: id=%q status=%q description_runes=%d evidence_refs=%d", update.ID, update.Status, runeLen(update.Description), len(update.Evidence))
		}
		t.Fatalf("cannot persist live stage result: %v", err)
	}
	var inputStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM personal_task_inputs WHERE task_id=? AND event_id=?`, task.ID, event.ID).Scan(&inputStatus); err != nil || inputStatus != "applied" {
		t.Fatalf("input was not committed as applied: status=%q error=%v", inputStatus, err)
	}
	var progressCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_progress WHERE task_id=? AND run_id=?`, task.ID, run.ID).Scan(&progressCount); err != nil || progressCount == 0 {
		t.Fatalf("reported progress was not committed: entries=%d error=%v", progressCount, err)
	}
	var manifestJSON string
	if err := store.db.QueryRowContext(ctx, `SELECT context_manifest FROM personal_runs WHERE id=?`, run.ID).Scan(&manifestJSON); err != nil {
		t.Fatal("cannot inspect assembled context manifest")
	}
	var manifest ContextManifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		t.Fatal("cannot decode assembled context manifest")
	}
	selectedMemory := false
	for _, selection := range manifest.Selections {
		if selection.Kind == "memory" && selection.ID == memory.Key && selection.IncludedBytes > 0 {
			selectedMemory = true
		}
	}
	if !selectedMemory {
		t.Fatal("explicit memory_refs was not included in the context manifest")
	}
	stored, err := store.Run(ctx, run.ID)
	if err != nil || stored == nil || stored.Status != RunCompleted {
		t.Fatal("live stage result was not persisted as completed")
	}
	var persisted StageResult
	if err := json.Unmarshal([]byte(stored.Result), &persisted); err != nil || len(persisted.AppliedInputIDs) != 1 || persisted.AppliedInputIDs[0] != event.ID {
		t.Fatal("persisted stage result omitted the applied input report")
	}

	budgetMu.Lock()
	records := append([]inputbudget.ModelRecord(nil), budgetRecords...)
	budgetMu.Unlock()
	if len(records) == 0 {
		t.Fatal("context estimator did not report any model calls")
	}
	for _, record := range records {
		t.Logf("model accounting outcome=%s estimate_bytes=%d hard_limit=%d input_tokens=%d output_tokens=%d", record.Outcome, record.Estimate.TotalBytes, record.HardLimit, record.InputTokens, record.OutputTokens)
	}
	for _, entry := range observed.FilterMessage("context.assembled").All() {
		fields := entry.ContextMap()
		t.Logf("assembled context estimate_bytes=%v hard_limit=%v count_basis=%v", fields["estimated_input_bytes"], fields["hard_limit"], fields["count_basis"])
	}
	summaryCalls := 0
	for _, entry := range observed.FilterMessage("context.reduced").All() {
		if phase, _ := entry.ContextMap()["phase"].(string); phase == "summary_model" {
			summaryCalls++
		}
	}
	t.Logf("context summary calls=%d", summaryCalls)
	t.Logf("PASS: explicit memory_refs, paged synthetic input, progress, applied report; context_window=%d pages=%d duration=%s summary_calls=%d", limits.Window, pageReads, elapsed, summaryCalls)
}

func liveInputCode(t *testing.T) string {
	t.Helper()
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal("cannot generate fictional input marker")
	}
	return hex.EncodeToString(raw[:])
}

func liveLongTaskInput(firstCode, middleCode, lastCode string) string {
	runes := make([]rune, 0, 9000)
	appendMarker := func(offset int, marker string) {
		for len(runes) < offset {
			runes = append(runes, '記')
		}
		runes = append(runes, []rune(marker)...)
	}
	appendMarker(900, "MICA_FIRST_CODE="+firstCode)
	appendMarker(4200, "MICA_MIDDLE_CODE="+middleCode)
	appendMarker(8200, "MICA_FINAL_CODE="+lastCode)
	for len(runes) < 9000 {
		runes = append(runes, '記')
	}
	return string(runes)
}

type liveTaskInputPageTraceStore struct {
	*Store
	mu    sync.Mutex
	reads []int64
}

func (s *liveTaskInputPageTraceStore) ReadTaskInput(ctx context.Context, runID, eventID int64) (Event, error) {
	event, err := s.Store.ReadTaskInput(ctx, runID, eventID)
	if err == nil {
		s.mu.Lock()
		s.reads = append(s.reads, eventID)
		s.mu.Unlock()
	}
	return event, err
}

func (s *liveTaskInputPageTraceStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reads)
}
