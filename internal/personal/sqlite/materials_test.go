package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/personal/agentctx"
	"kagari/internal/textpage"
)

func TestMaterialPersistencePagingAndCaptureConflict(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "context.db")
	s := testStore(t, path, time.Minute)
	run := createRunningTask(t, s, "material", personal.TaskInput{Goal: "Read evidence"})
	source := domain.Source{URL: "https://example.org", Status: "incomplete", Truncated: true, FetchedAt: time.Now().UTC(), Content: strings.Repeat("中🙂\x00<>&", 3000)}
	ref, err := s.SaveSource(ctx, run.ID, "capture", source)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.SaveSource(ctx, run.ID, "capture", source)
	if err != nil || replay.ID != ref.ID {
		t.Fatalf("replay %v %v", replay, err)
	}
	changed := source
	changed.Content += "changed"
	if _, err = s.SaveSource(ctx, run.ID, "capture", changed); err == nil || !strings.Contains(err.Error(), "capture_conflict") {
		t.Fatalf("capture conflict=%v", err)
	}
	newRef, err := s.SaveSource(ctx, run.ID, "capture2", source)
	if err != nil || newRef.ID == ref.ID {
		t.Fatalf("new observation %v %v", newRef, err)
	}
	scope := personal.ReadScope{RunID: run.ID}
	page, err := s.ListMaterials(ctx, scope, run.ID, personal.ListPage{Limit: 1})
	if err != nil || !page.HasMore || page.Items[0].ID != newRef.ID {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	var got strings.Builder
	offset := 0
	for {
		part, err := s.ReadMaterial(ctx, scope, ref.ID, "body", textpage.Request{Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := textpage.EncodeBounded(&part, textpage.MaxJSONBytes, part.Page.Shrink)
		if err != nil || !json.Valid(raw) || len(raw) > 32<<10 {
			t.Fatalf("encoding=%d %v", len(raw), err)
		}
		if !part.Page.SourceTruncated {
			t.Fatal("lost acquisition truncation")
		}
		got.WriteString(part.Page.Text)
		if !part.Page.HasMore {
			break
		}
		offset = *part.Page.NextOffset
	}
	if got.String() != source.Content {
		t.Fatal("paging lost Unicode/NUL/escaped text")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	full, err := reopened.ExpandSource(ctx, ref.ID)
	if err != nil || full.Content != source.Content {
		t.Fatalf("durable source=%v %v", full, err)
	}
}

func TestMaterialCrossTaskGrantIsNarrowAndNotTransitive(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	first := createRunningTask(t, s, "first", personal.TaskInput{Goal: "First"})
	ref, err := s.SaveSource(ctx, first.ID, "source", domain.Source{Content: "evidence", Status: "ok", FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishStage(ctx, first.ID, personal.StageResult{Summary: "first"}, ""); err != nil {
		t.Fatal(err)
	}
	// Complete the first callback so the next direct-user event can be claimed.
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatal(err)
	}
	finishEvent(t, s, *callback, personal.TurnOutput{}, nil)
	event := openUserEvent(t, s, 20, 20, "Use first evidence")
	_, second, err := s.CreateTask(ctx, event.ID, "second", "Second", personal.TaskInput{Goal: "Second", SourceRunIDs: []int64{first.ID}})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{}, nil)
	claimed, err := s.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimed.ID != second.ID {
		t.Fatal(err)
	}
	if _, err = s.ReadMaterial(ctx, personal.ReadScope{RunID: second.ID}, ref.ID, "body", textpage.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadRecord(ctx, personal.ReadScope{RunID: second.ID}, personal.RecordRequest{Kind: "run", ID: first.ID, Section: "input"}); err == nil {
		t.Fatal("external input leaked")
	}
	if _, err = s.QueryRuns(ctx, personal.ReadScope{RunID: second.ID}, first.TaskID, personal.ListPage{}); err == nil {
		t.Fatal("external task history leaked")
	}
	if err = s.FinishStage(ctx, second.ID, personal.StageResult{Summary: "second"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err = s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatal(err)
	}
	finishEvent(t, s, *callback, personal.TurnOutput{}, nil)
	event = openUserEvent(t, s, 30, 30, "Use second only")
	_, third, err := s.CreateTask(ctx, event.ID, "third", "Third", personal.TaskInput{Goal: "Third", SourceRunIDs: []int64{second.ID}})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{}, nil)
	claimed, err = s.ClaimRun(ctx, personal.RunTask)
	if err != nil || claimed.ID != third.ID {
		t.Fatal(err)
	}
	if _, err = s.ReadMaterial(ctx, personal.ReadScope{RunID: third.ID}, ref.ID, "body", textpage.Request{}); err == nil {
		t.Fatal("transitive authorization leaked")
	}
}

func TestMaterialSearchOverlapAndCheckpointCAS(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "search", personal.TaskInput{Goal: "Read"})
	ref, err := s.SaveSource(ctx, run.ID, "source", domain.Source{Content: "中aaaa🙂", Status: "ok", FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.SearchMaterial(ctx, personal.ReadScope{RunID: run.ID}, ref.ID, "aaa", personal.SearchPage{Limit: 1})
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Offset != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	next, err := s.SearchMaterial(ctx, personal.ReadScope{RunID: run.ID}, ref.ID, "aaa", personal.SearchPage{Offset: page.NextOffset})
	if err != nil || len(next.Matches) != 1 || next.Matches[0].Offset != 2 {
		t.Fatalf("%+v %v", next, err)
	}
	ledger := agentctx.NewReadLedger()
	tools, err := agentctx.ReadTools(agentctx.ReadToolsConfig{Store: s, Scope: personal.ReadScope{RunID: run.ID}, Ledger: ledger})
	if err != nil {
		t.Fatal(err)
	}
	readMaterial := findProgressTool(t, tools, "read_material")
	checkpoint := findProgressTool(t, tools, "checkpoint_reading")
	read := func(offset, limit int, version string) personal.MaterialText {
		t.Helper()
		args, err := json.Marshal(map[string]any{"material_id": ref.ID, "part": "body", "offset": offset, "limit": limit, "version": version})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := readMaterial.InvokableRun(ctx, string(args))
		if err != nil {
			t.Fatal(err)
		}
		var page personal.MaterialText
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	p := read(0, 2, "")
	save := func(update personal.ReadCheckpointUpdate) error {
		args, err := json.Marshal(update)
		if err != nil {
			return err
		}
		_, err = checkpoint.InvokableRun(ctx, string(args))
		return err
	}
	u := personal.ReadCheckpointUpdate{ResourceKind: "material", ResourceID: ref.ID, Version: p.Page.Version, NextOffset: 3, Note: "read prefix"}
	if err := save(u); err == nil {
		t.Fatal("checkpoint crossed unread span")
	}
	u.NextOffset = 2
	if err = save(u); err != nil {
		t.Fatal(err)
	}
	if err = save(u); err != nil {
		t.Fatal(err)
	}
	read(2, 1, p.Page.Version)
	u.NextOffset = 3
	if err = save(u); err == nil {
		t.Fatal("stale checkpoint overwrote state")
	}
}

func TestToolRecordRejectsOversizedMetadata(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "large tool metadata", personal.TaskInput{Goal: "Read evidence"})
	_, err := s.SaveToolRecord(ctx, personal.ReadScope{RunID: run.ID}, "round", personal.ToolRecord{Body: "[]", Tools: []string{strings.Repeat("x", 64<<10)}})
	if err == nil || !strings.Contains(err.Error(), "material_too_large") {
		t.Fatalf("oversized metadata error = %v", err)
	}
}

func TestRepositoryReadMissesUseDomainError(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "missing reads", personal.TaskInput{Goal: "Read evidence"})
	scope := personal.ReadScope{RunID: run.ID}
	missingID := run.ID + 1000
	assertMissing := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, personal.ErrRecordNotFound) {
			t.Errorf("%s error = %v, want ErrRecordNotFound", name, err)
		}
	}

	_, err := s.ReadRecord(ctx, scope, personal.RecordRequest{Kind: "run", ID: missingID, Section: "result"})
	assertMissing("read record", err)
	_, err = s.ReadMaterial(ctx, scope, "m_missing", "body", textpage.Request{})
	assertMissing("read material", err)
	_, err = s.SearchMaterial(ctx, scope, "m_missing", "term", personal.SearchPage{})
	assertMissing("search material", err)
	_, err = s.ListMaterials(ctx, scope, missingID, personal.ListPage{})
	assertMissing("list materials", err)
	_, err = s.QueryRuns(ctx, personal.ReadScope{EventID: missingID}, run.TaskID, personal.ListPage{})
	assertMissing("query runs", err)
	_, err = s.QueryTasks(ctx, personal.ReadScope{EventID: missingID}, personal.TaskQuery{})
	assertMissing("query tasks", err)
	_, err = s.ReadCheckpoints(ctx, missingID)
	assertMissing("read checkpoints", err)
	_, err = s.SaveReadCheckpoint(ctx, missingID, personal.ReadCheckpointUpdate{ResourceKind: "material", ResourceID: "m_missing", Version: "v", NextOffset: 1, Note: "read"})
	assertMissing("save checkpoint", err)
	_, err = s.TaskProgress(ctx, personal.ReadScope{EventID: missingID}, run.TaskID, personal.ProgressQuery{})
	assertMissing("task progress", err)
	_, err = s.ReadProgress(ctx, personal.ReadScope{EventID: missingID}, run.TaskID, 1, textpage.Request{})
	assertMissing("read progress scope", err)
	_, err = s.ReadProgress(ctx, scope, run.TaskID, missingID, textpage.Request{})
	assertMissing("read missing progress", err)
	_, err = s.CorrectProgress(ctx, missingID, run.TaskID, run.Revision, 0, nil)
	assertMissing("correct progress", err)
	err = s.ValidateProgressSubmission(ctx, personal.Run{ID: missingID}, nil, nil)
	assertMissing("validate progress", err)
	_, err = s.SaveToolRecord(ctx, personal.ReadScope{RunID: missingID}, "missing", personal.ToolRecord{Body: "[]"})
	assertMissing("save tool record", err)

	if task, err := s.Task(ctx, missingID); err != nil || task != nil {
		t.Errorf("missing Task() = (%v, %v), want (nil, nil)", task, err)
	}
	if missingRun, err := s.Run(ctx, missingID); err != nil || missingRun != nil {
		t.Errorf("missing Run() = (%v, %v), want (nil, nil)", missingRun, err)
	}

	tools, err := agentctx.ReadTools(agentctx.ReadToolsConfig{Store: s, Scope: scope, Ledger: agentctx.NewReadLedger()})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := findProgressTool(t, agentctx.BoundTools(tools), "read_material").InvokableRun(ctx, `{"material_id":"m_missing","part":"body"}`)
	if err != nil || raw != `{"error":"record_not_found"}` {
		t.Fatalf("missing material tool response = (%q, %v)", raw, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReadMaterial(ctx, scope, "m_missing", "body", textpage.Request{})
	if err == nil || errors.Is(err, personal.ErrRecordNotFound) {
		t.Fatalf("closed database was treated as a missing record: %v", err)
	}
}
