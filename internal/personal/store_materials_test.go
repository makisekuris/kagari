package personal

import (
	"context"
	"encoding/json"
	"kagari/internal/domain"
	"kagari/internal/textpage"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaterialPersistencePagingAndCaptureConflict(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "context.db")
	s := testStore(t, path, time.Minute)
	run := createRunningTask(t, s, "material", TaskInput{Goal: "Read evidence"})
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
	scope := ReadScope{RunID: run.ID}
	page, err := s.ListMaterials(ctx, scope, run.ID, ListPage{Limit: 1})
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
	first := createRunningTask(t, s, "first", TaskInput{Goal: "First"})
	ref, err := s.SaveSource(ctx, first.ID, "source", domain.Source{Content: "evidence", Status: "ok", FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishStage(ctx, first.ID, StageResult{Summary: "first"}, ""); err != nil {
		t.Fatal(err)
	}
	// Complete the first callback so the next direct-user event can be claimed.
	callback, err := s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatal(err)
	}
	finishEvent(t, s, *callback, TurnOutput{}, nil)
	event := openUserEvent(t, s, 20, 20, "Use first evidence")
	_, second, err := s.CreateTask(ctx, event.ID, "second", "Second", TaskInput{Goal: "Second", SourceRunIDs: []int64{first.ID}})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{}, nil)
	claimed, err := s.ClaimRun(ctx, RunTask)
	if err != nil || claimed.ID != second.ID {
		t.Fatal(err)
	}
	if _, err = s.ReadMaterial(ctx, ReadScope{RunID: second.ID}, ref.ID, "body", textpage.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadRecord(ctx, ReadScope{RunID: second.ID}, RecordRequest{Kind: "run", ID: first.ID, Section: "input"}); err == nil {
		t.Fatal("external input leaked")
	}
	if _, err = s.QueryRuns(ctx, ReadScope{RunID: second.ID}, first.TaskID, ListPage{}); err == nil {
		t.Fatal("external task history leaked")
	}
	if err = s.FinishStage(ctx, second.ID, StageResult{Summary: "second"}, ""); err != nil {
		t.Fatal(err)
	}
	callback, err = s.ClaimEvent(ctx)
	if err != nil || callback == nil {
		t.Fatal(err)
	}
	finishEvent(t, s, *callback, TurnOutput{}, nil)
	event = openUserEvent(t, s, 30, 30, "Use second only")
	_, third, err := s.CreateTask(ctx, event.ID, "third", "Third", TaskInput{Goal: "Third", SourceRunIDs: []int64{second.ID}})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{}, nil)
	claimed, err = s.ClaimRun(ctx, RunTask)
	if err != nil || claimed.ID != third.ID {
		t.Fatal(err)
	}
	if _, err = s.ReadMaterial(ctx, ReadScope{RunID: third.ID}, ref.ID, "body", textpage.Request{}); err == nil {
		t.Fatal("transitive authorization leaked")
	}
}

func TestMaterialSearchOverlapAndCheckpointCAS(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "search", TaskInput{Goal: "Read"})
	ref, err := s.SaveSource(ctx, run.ID, "source", domain.Source{Content: "中aaaa🙂", Status: "ok", FetchedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.SearchMaterial(ctx, ReadScope{RunID: run.ID}, ref.ID, "aaa", SearchPage{Limit: 1})
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Offset != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	next, err := s.SearchMaterial(ctx, ReadScope{RunID: run.ID}, ref.ID, "aaa", SearchPage{Offset: page.NextOffset})
	if err != nil || len(next.Matches) != 1 || next.Matches[0].Offset != 2 {
		t.Fatalf("%+v %v", next, err)
	}
	ledger := newReadLedger()
	p, err := s.ReadMaterial(ctx, ReadScope{RunID: run.ID}, ref.ID, "body", textpage.Request{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	ledger.observe("material", ref.ID, p.Page)
	u := ReadCheckpointUpdate{ResourceKind: "material", ResourceID: ref.ID, Version: p.Page.Version, NextOffset: 3, Note: "read prefix"}
	if ledger.covered(u) {
		t.Fatal("ledger crossed unread span")
	}
	u.NextOffset = 2
	if !ledger.covered(u) {
		t.Fatal("returned prefix not recorded")
	}
	if _, err = s.SaveReadCheckpoint(ctx, run.ID, u); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveReadCheckpoint(ctx, run.ID, u); err != nil {
		t.Fatal(err)
	}
	u.NextOffset = 3
	if _, err = s.SaveReadCheckpoint(ctx, run.ID, u); err == nil {
		t.Fatal("stale checkpoint overwrote state")
	}
}

func TestToolRecordRejectsOversizedMetadata(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	run := createRunningTask(t, s, "large tool metadata", TaskInput{Goal: "Read evidence"})
	_, err := s.SaveToolRecord(ctx, ReadScope{RunID: run.ID}, "round", "[]", []string{strings.Repeat("x", 64<<10)})
	if err == nil || !strings.Contains(err.Error(), "material_too_large") {
		t.Fatalf("oversized metadata error = %v", err)
	}
}
