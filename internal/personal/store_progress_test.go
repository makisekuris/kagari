package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"kagari/internal/channel"
	"kagari/internal/domain"
	"kagari/internal/textpage"
)

func TestProgressAccumulatesAcrossStagesAndCorrectionUsesCAS(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "progress", TaskInput{Goal: "Compare findings"})
	finish := func(run Run, id, status, description string) {
		t.Helper()
		err := s.FinishStage(ctx, run.ID, StageResult{Summary: description, Progress: []ProgressUpdate{{ID: id, Status: status, Description: description, Evidence: []EvidenceRef{{Kind: "run_result", ID: strconv.FormatInt(run.ID, 10)}}}}}, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	finish(run, "first", "reported", "First result")
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatal(err)
	}
	_, next, err := s.ContinueTask(ctx, callback.ID, "second", run.TaskID, run.Revision, "Continue", false)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, *callback, TurnOutput{}, nil)
	claimed, err := s.ClaimRun(ctx, RunTask)
	if err != nil || claimed.ID != next.ID {
		t.Fatal(err)
	}
	finish(*claimed, "open", "unresolved", "Unresolved edge")
	callback, err = s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatal(err)
	}
	p, err := s.TaskProgress(ctx, ReadScope{EventID: callback.ID}, run.TaskID, ProgressQuery{})
	if err != nil || len(p.Entries) != 2 || p.Version != 2 {
		t.Fatalf("progress=%+v %v", p, err)
	}
	updates := []ProgressUpdate{{ID: "first", Description: "First result needs checking", Status: "superseded", Reason: "Later evidence conflicts"}}
	if _, err = s.CorrectProgress(ctx, callback.ID, run.TaskID, run.Revision, 1, updates); err == nil {
		t.Fatal("stale progress overwrite accepted")
	}
	corrected, err := s.CorrectProgress(ctx, callback.ID, run.TaskID, run.Revision, p.Version, updates)
	if err != nil || len(corrected.Entries) != 2 || corrected.Version != 3 {
		t.Fatalf("correction=%+v %v", corrected, err)
	}
	firstPage, err := s.TaskProgress(ctx, ReadScope{EventID: callback.ID}, run.TaskID, ProgressQuery{ListPage: ListPage{Limit: 1}})
	if err != nil || len(firstPage.Entries) != 1 || !firstPage.HasMore || firstPage.Entries[0].ID != "first" {
		t.Fatalf("first current-progress page=%+v err=%v", firstPage, err)
	}
	if _, err := s.CorrectProgress(ctx, callback.ID, run.TaskID, run.Revision, corrected.Version, []ProgressUpdate{{
		ID: "open", Description: "Updated after page one", Status: "reported",
	}}); err != nil {
		t.Fatal(err)
	}
	secondPage, err := s.TaskProgress(ctx, ReadScope{EventID: callback.ID}, run.TaskID, ProgressQuery{ListPage: ListPage{
		Limit: 1, BeforeID: firstPage.NextBeforeID, SnapshotMaxID: firstPage.SnapshotMaxID, QueryKey: firstPage.QueryKey,
	}})
	if err != nil || len(secondPage.Entries) != 1 || secondPage.HasMore || secondPage.Entries[0].ID != "open" || secondPage.Entries[0].Seq > firstPage.SnapshotMaxID {
		t.Fatalf("second snapshot page=%+v err=%v", secondPage, err)
	}
	historyFirst, err := s.TaskProgress(ctx, ReadScope{EventID: callback.ID}, run.TaskID, ProgressQuery{ListPage: ListPage{Limit: 2}, History: true})
	if err != nil || len(historyFirst.Entries) != 2 || !historyFirst.HasMore || historyFirst.Entries[0].Seq <= historyFirst.Entries[1].Seq {
		t.Fatalf("first progress-history page=%+v err=%v", historyFirst, err)
	}
	historySecond, err := s.TaskProgress(ctx, ReadScope{EventID: callback.ID}, run.TaskID, ProgressQuery{ListPage: ListPage{
		Limit: 2, BeforeID: historyFirst.NextBeforeID, SnapshotMaxID: historyFirst.SnapshotMaxID, QueryKey: historyFirst.QueryKey,
	}, History: true})
	if err != nil || len(historySecond.Entries) != 2 || historySecond.HasMore {
		t.Fatalf("second progress-history page=%+v err=%v", historySecond, err)
	}
	seenSeq := map[int64]bool{}
	for _, entry := range append(historyFirst.Entries, historySecond.Entries...) {
		if seenSeq[entry.Seq] {
			t.Fatalf("duplicate history row seq %d", entry.Seq)
		}
		seenSeq[entry.Seq] = true
	}
	if len(seenSeq) != 4 {
		t.Fatalf("history returned %d revisions, want 4", len(seenSeq))
	}
	var rows int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM personal_progress WHERE task_id=?`, run.TaskID).Scan(&rows); err != nil || rows != 4 {
		t.Fatalf("history rows=%d %v", rows, err)
	}
	// Replaying a terminal FinishStage cannot apply its older delta again.
	finish(run, "first", "reported", "Old replay")
	after, err := s.TaskProgress(ctx, ReadScope{EventID: callback.ID}, run.TaskID, ProgressQuery{})
	if err != nil || after.Version != 4 {
		t.Fatalf("late replay changed progress: %+v %v", after, err)
	}
}

func TestProgressFailureRollsBackRunResultAndTask(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "rollback", TaskInput{Goal: "Keep atomic state"})
	err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Must not persist", Progress: []ProgressUpdate{{ID: "bad", Description: "Unowned evidence", Status: "reported", Evidence: []EvidenceRef{{Kind: "attachment", ID: "not-associated"}}}}}, "")
	if err == nil {
		t.Fatal("invalid evidence accepted")
	}
	saved, err := s.Run(ctx, run.ID)
	if err != nil || saved.Status != RunRunning || saved.Result != "" {
		t.Fatalf("result partially persisted: %+v %v", saved, err)
	}
}

func TestProgressCorrectionRequiresAuthorizedSourceMaterial(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	seq := int64(500)
	external, ref := acceptanceCompletedSource(t, s, &seq, "progress-source")
	unassociated, unassociatedRef := acceptanceCompletedSource(t, s, &seq, "unassociated-source")
	targetEvent := acceptanceOpenEvent(t, s, &seq, "Create task with selected source")
	task, _, err := s.CreateTask(ctx, targetEvent.ID, "progress-target", "progress target", TaskInput{
		Goal: "Use selected evidence", SourceRunIDs: []int64{external.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, targetEvent, TurnOutput{}, nil)
	run, err := s.ClaimRun(ctx, RunTask)
	if err != nil || run == nil || run.TaskID != task.ID {
		t.Fatalf("ClaimRun()=(%+v,%v)", run, err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Read selected evidence", Progress: []ProgressUpdate{{
		ID: "source-check", Description: "Source captured", Status: "reported", Evidence: []EvidenceRef{{Kind: "material", ID: ref.ID}},
	}}}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil || callback.Kind != EventTask {
		t.Fatalf("task callback=(%+v,%v)", callback, err)
	}
	finishEvent(t, s, *callback, TurnOutput{}, nil)
	correctionEvent := acceptanceOpenEvent(t, s, &seq, "Correct source progress")
	updated, err := s.CorrectProgress(ctx, correctionEvent.ID, task.ID, run.Revision, 1, []ProgressUpdate{{
		ID: "source-check", Description: "Selected source remains relevant", Status: "reported", Evidence: []EvidenceRef{{Kind: "material", ID: ref.ID}},
	}})
	if err != nil || updated.Version != 2 {
		t.Fatalf("authorized source correction=%+v err=%v", updated, err)
	}
	if _, err := s.CorrectProgress(ctx, correctionEvent.ID, task.ID, run.Revision, updated.Version, []ProgressUpdate{{
		ID: "unassociated-check", Description: "Unselected source", Status: "reported", Evidence: []EvidenceRef{{Kind: "material", ID: unassociatedRef.ID}},
	}}); err == nil {
		t.Fatalf("correction accepted material from unassociated run %d", unassociated.ID)
	}
	finishEvent(t, s, correctionEvent, TurnOutput{}, nil)
}

func TestProgressAttachmentEvidenceMustRemainAvailable(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	file, err := s.SaveAttachment(ctx, "evidence.txt", "text/plain", strings.NewReader("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	run := createRunningTask(t, s, "attachment-progress", TaskInput{Goal: "Check attachment evidence", Attachments: []channel.Attachment{file}})
	update := ProgressUpdate{ID: "attachment-check", Description: "Read attachment", Status: "reported", Evidence: []EvidenceRef{{Kind: "attachment", ID: file.ID}}}
	if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Read attachment", Progress: []ProgressUpdate{update}}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatalf("task callback=(%+v,%v)", callback, err)
	}
	if err := s.DeleteAttachment(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.CorrectProgress(ctx, callback.ID, run.TaskID, run.Revision, 1, []ProgressUpdate{update})
	if !errors.Is(err, ErrAttachmentUnavailable) {
		t.Fatalf("deleted attachment evidence error=%v, want ErrAttachmentUnavailable", err)
	}
}

func TestSubmitProgressMergesConcurrentCallsAndOverwritesByID(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "progress-submit", TaskInput{Goal: "Accumulate progress"})
	delta := []ProgressUpdate{}
	tools, err := progressTools(s, ReadScope{RunID: run.ID}, &delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	submit := findProgressTool(t, tools, "submit_progress")
	const parallel = 8
	start := make(chan struct{})
	results := make(chan error, parallel)
	var wg sync.WaitGroup
	for i := range parallel {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			raw, err := json.Marshal(struct {
				Updates []ProgressUpdate `json:"updates"`
			}{Updates: []ProgressUpdate{{ID: fmt.Sprintf("entry-%d", i), Description: "Parallel report", Status: "reported"}}})
			if err != nil {
				results <- err
				return
			}
			_, err = submit.InvokableRun(ctx, string(raw))
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(delta) != parallel {
		t.Fatalf("concurrent submissions kept %d updates, want %d", len(delta), parallel)
	}
	raw, err := json.Marshal(struct {
		Updates []ProgressUpdate `json:"updates"`
	}{Updates: []ProgressUpdate{{ID: "entry-0", Description: "Corrected later", Status: "unresolved"}, {ID: "entry-8", Description: "New report", Status: "reported"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := submit.InvokableRun(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
	if len(delta) != parallel+1 {
		t.Fatalf("merged submissions kept %d updates, want %d", len(delta), parallel+1)
	}
	for _, update := range delta {
		if update.ID == "entry-0" && (update.Description != "Corrected later" || update.Status != "unresolved") {
			t.Fatalf("later update did not replace entry-0: %+v", update)
		}
	}
}

func TestSubmitProgressRejectsInvalidStatusBeforeStageFinishes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "invalid-progress", TaskInput{Goal: "Report progress"})
	var updates []ProgressUpdate
	tools, err := progressTools(s, ReadScope{RunID: run.ID}, &updates, nil)
	if err != nil {
		t.Fatal(err)
	}
	submit := findProgressTool(t, tools, "submit_progress")
	raw, err := submit.InvokableRun(ctx, `{"updates":[{"id":"done","description":"Found evidence","status":"completed"}]}`)
	if err != nil || !strings.Contains(raw, "invalid_progress") || len(updates) != 0 {
		t.Fatalf("invalid submission staged=%+v reply=%s err=%v", updates, raw, err)
	}
	raw, err = submit.InvokableRun(ctx, `{"updates":[{"id":"done","description":"Found evidence","status":"reported"}]}`)
	if err != nil || strings.Contains(raw, `"error"`) || len(updates) != 1 {
		t.Fatalf("corrected submission: %s %v", raw, err)
	}
	if err = s.FinishStage(ctx, run.ID, StageResult{Summary: "Found evidence", Progress: updates}, ""); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitProgressReferencesAreCorrectableAndSupersedeEarlierBatchItems(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	seq := int64(700)
	_, externalSource := acceptanceCompletedSource(t, s, &seq, "external-progress-evidence")
	run := acceptanceRunningTask(t, s, &seq, "progress-reference-validation", TaskInput{Goal: "Validate progress references"})
	toolRecord, err := s.SaveToolRecord(ctx, ReadScope{RunID: run.ID}, "progress-record", "[]", nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.SaveAttachment(ctx, "progress-evidence.txt", "text/plain", strings.NewReader("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	allowedAttachments := map[string]bool{file.ID: true}
	var delta []ProgressUpdate
	tools, err := progressTools(s, ReadScope{RunID: run.ID}, &delta, allowedAttachments)
	if err != nil {
		t.Fatal(err)
	}
	submit := findProgressTool(t, tools, "submit_progress")
	submitUpdates := func(updates ...ProgressUpdate) (progressSubmissionReply, error) {
		t.Helper()
		args, err := json.Marshal(struct {
			Updates []ProgressUpdate `json:"updates"`
		}{Updates: updates})
		if err != nil {
			return progressSubmissionReply{}, err
		}
		raw, err := submit.InvokableRun(ctx, string(args))
		if err != nil {
			return progressSubmissionReply{}, err
		}
		var reply progressSubmissionReply
		if err := json.Unmarshal([]byte(raw), &reply); err != nil {
			return progressSubmissionReply{}, err
		}
		return reply, nil
	}
	base := ProgressUpdate{ID: "base", Description: "Initial finding", Status: "reported"}
	if reply, err := submitUpdates(base); err != nil || reply.Error != "" || reply.Submitted != 1 {
		t.Fatalf("initial progress submission=%+v err=%v", reply, err)
	}
	var memoryRunID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_runs WHERE task_id IS NULL AND kind='memory_review' LIMIT 1`).Scan(&memoryRunID); err != nil {
		t.Fatal(err)
	}
	invalid := []ProgressUpdate{
		{ID: "missing-material", Description: "Missing source", Status: "reported", Evidence: []EvidenceRef{{Kind: "material", ID: "m_missing-source"}}},
		{ID: "tool-record", Description: "Tool output is not source evidence", Status: "reported", Evidence: []EvidenceRef{{Kind: "material", ID: toolRecord}}},
		{ID: "unassociated-material", Description: "External source is not selected", Status: "reported", Evidence: []EvidenceRef{{Kind: "material", ID: externalSource.ID}}},
		{ID: "malformed-run", Description: "Malformed run reference", Status: "reported", Evidence: []EvidenceRef{{Kind: "run_result", ID: "not-a-run"}}},
		{ID: "memory-run", Description: "Memory runs are not task evidence", Status: "reported", Evidence: []EvidenceRef{{Kind: "run_result", ID: strconv.FormatInt(memoryRunID, 10)}}},
		{ID: "missing-supersedes", Description: "References no existing finding", Status: "reported", Supersedes: "does-not-exist"},
	}
	for _, update := range invalid {
		before := append([]ProgressUpdate(nil), delta...)
		reply, err := submitUpdates(update)
		if err != nil || !strings.Contains(reply.Error, "invalid_progress") {
			t.Fatalf("invalid submission %q reply=%+v err=%v", update.ID, reply, err)
		}
		if !reflect.DeepEqual(delta, before) {
			t.Fatalf("invalid submission %q changed staged delta: before=%+v after=%+v", update.ID, before, delta)
		}
	}
	validSource, err := s.SaveSource(ctx, run.ID, "valid-progress-source", domain.Source{Content: "selected source", Status: "ok", FetchedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	completed := ProgressUpdate{
		ID: "completed", Description: "Finding backed by current evidence", Status: "reported", Supersedes: "base",
		Evidence: []EvidenceRef{{Kind: "material", ID: validSource.ID}, {Kind: "attachment", ID: file.ID}, {Kind: "run_result", ID: strconv.FormatInt(run.ID, 10)}},
	}
	if reply, err := submitUpdates(completed); err != nil || reply.Error != "" || reply.Submitted != 1 {
		t.Fatalf("corrected progress submission=%+v err=%v", reply, err)
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Progress references validated", Progress: delta, Attachments: []channel.Attachment{file}}, ""); err != nil {
		t.Fatalf("FinishStage rejected corrected progress: %v", err)
	}
	var saved int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_progress WHERE task_id=?`, run.TaskID).Scan(&saved); err != nil || saved != 2 {
		t.Fatalf("saved progress rows=%d err=%v", saved, err)
	}
}

func TestLargeCumulativeProgressDoesNotBlockStageCheckpoint(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "large-progress", TaskInput{Goal: "Preserve cumulative detail"})
	var updates []ProgressUpdate
	for i := 0; i < 17; i++ {
		updates = append(updates, ProgressUpdate{ID: fmt.Sprint(i), Status: "reported", Description: strings.Repeat("x", 2000)})
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Seventeen findings", Progress: updates}, ""); err != nil {
		t.Fatal(err)
	}
	var checkpoint string
	if err := s.db.QueryRowContext(ctx, `SELECT checkpoint FROM personal_tasks WHERE id=?`, run.TaskID).Scan(&checkpoint); err != nil || len(checkpoint) > maxTaskChars {
		t.Fatalf("checkpoint length=%d err=%v", len(checkpoint), err)
	}
	var saved int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_progress WHERE task_id=?`, run.TaskID).Scan(&saved); err != nil || saved != 17 {
		t.Fatalf("saved entries=%d err=%v", saved, err)
	}
}

func TestProgressCardsFitBudgetAndFullEntryCanBeReadBySequence(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "progress-read", TaskInput{Goal: "Read full progress"})
	evidence := make([]EvidenceRef, 8)
	for i := range evidence {
		evidence[i] = EvidenceRef{Kind: "run_result", ID: strconv.FormatInt(run.ID, 10)}
	}
	if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Saved a detailed result", Progress: []ProgressUpdate{{
		ID: "long-entry", Description: strings.Repeat("long description ", 100), Status: "reported", Evidence: evidence,
	}}}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatalf("task callback=(%+v,%v)", callback, err)
	}
	fullPage, err := s.TaskProgress(ctx, ReadScope{EventID: callback.ID}, run.TaskID, ProgressQuery{})
	if err != nil || len(fullPage.Entries) != 1 {
		t.Fatalf("full progress=%+v err=%v", fullPage, err)
	}

	tools, err := progressTools(s, ReadScope{EventID: callback.ID}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools = boundContextTools(tools)
	budget := textpage.WithMaxBytes(ctx, 1024)
	get := findProgressTool(t, tools, "get_progress")
	getRaw, err := get.InvokableRun(budget, `{"task_id":`+strconv.FormatInt(run.TaskID, 10)+`,"limit":20}`)
	if err != nil || len(getRaw) > textpage.MaxBytes(budget) {
		t.Fatalf("get_progress response bytes=%d err=%v", len(getRaw), err)
	}
	var card TaskProgress
	if err := json.Unmarshal([]byte(getRaw), &card); err != nil || len(card.Entries) != 1 {
		t.Fatalf("progress card=%s err=%v", getRaw, err)
	}
	if card.Entries[0].Description == fullPage.Entries[0].Description || !containsProgressOmitted(card.Entries[0].OmittedFields, "description") || len(card.Entries[0].Evidence) >= len(fullPage.Entries[0].Evidence) || !containsProgressOmitted(card.Entries[0].OmittedFields, "evidence") {
		t.Fatalf("full entry was not clearly marked as omitted: %+v", card.Entries[0])
	}

	read := findProgressTool(t, tools, "read_progress")
	var full strings.Builder
	offset := 0
	version := ""
	for {
		args, err := json.Marshal(map[string]any{"task_id": run.TaskID, "seq": fullPage.Entries[0].Seq, "offset": offset, "limit": 4000, "version": version})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := read.InvokableRun(budget, string(args))
		if err != nil || len(raw) > textpage.MaxBytes(budget) {
			t.Fatalf("read_progress response bytes=%d err=%v", len(raw), err)
		}
		var page ProgressText
		if err := json.Unmarshal([]byte(raw), &page); err != nil || page.Page.Offset != offset {
			t.Fatalf("read_progress page=%s err=%v", raw, err)
		}
		full.WriteString(page.Page.Text)
		version = page.Page.Version
		if !page.Page.HasMore {
			break
		}
		offset = *page.Page.NextOffset
	}
	var expanded ProgressEntry
	if err := json.Unmarshal([]byte(full.String()), &expanded); err != nil {
		t.Fatalf("expanded progress JSON=%q err=%v", full.String(), err)
	}
	if !reflect.DeepEqual(expanded, fullPage.Entries[0]) {
		t.Fatalf("expanded entry=%+v, stored entry=%+v", expanded, fullPage.Entries[0])
	}
}

func findProgressTool(t *testing.T, tools []tool.BaseTool, name string) tool.InvokableTool {
	t.Helper()
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name != name {
			continue
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatalf("%s is not invokable", name)
		}
		return invokable
	}
	t.Fatalf("%s tool not found", name)
	return nil
}

func containsProgressOmitted(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}
