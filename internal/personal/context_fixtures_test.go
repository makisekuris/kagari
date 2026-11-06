package personal

import (
	"context"
	"fmt"
	"kagari/internal/domain"
	"kagari/internal/textpage"
	"testing"
)

func (r *toolRepo) SaveSource(_ context.Context, runID int64, key string, src domain.Source) (domain.SourceRef, error) {
	if r.sources == nil {
		r.sources = map[string]domain.Source{}
	}
	id := "m_fixture_" + key
	src.ID = id
	r.sources[id] = src
	return domain.SourceRef{ID: id, RunID: runID, URL: src.URL, Title: src.Title, Status: src.Status, FetchedAt: src.FetchedAt, SourceTruncated: src.Truncated, TotalRunes: runeLen(src.Content)}, nil
}
func (r *toolRepo) SaveContextManifest(context.Context, int64, ContextManifest) error { return nil }
func (r *toolRepo) ReadCheckpoints(context.Context, int64) ([]ReadCheckpoint, error)  { return nil, nil }
func (r *toolRepo) RunMemory(ctx context.Context, _ int64, key string) (*Memory, error) {
	return r.Memory(ctx, key)
}
func (r *toolRepo) TaskProgress(context.Context, ReadScope, int64, ProgressQuery) (TaskProgress, error) {
	return TaskProgress{FormatVersion: 1, Entries: []ProgressEntry{}}, nil
}
func (r *toolRepo) ReadProgress(context.Context, ReadScope, int64, int64, textpage.Request) (ProgressText, error) {
	return ProgressText{}, nil
}
func (r *toolRepo) QueryTasks(ctx context.Context, _ ReadScope, _ TaskQuery) (TaskPage, error) {
	tasks, err := r.Tasks(ctx, 50)
	return TaskPage{Tasks: tasks}, err
}
func (r *toolRepo) QueryRuns(ctx context.Context, _ ReadScope, id int64, _ ListPage) (RunPage, error) {
	runs, err := r.Runs(ctx, id, 50)
	return RunPage{Runs: runs}, err
}
func fixtureSource(t *testing.T, r *toolRepo, ref domain.SourceRef) domain.Source {
	t.Helper()
	src, ok := r.sources[ref.ID]
	if !ok {
		t.Fatal(fmt.Sprintf("missing source %s", ref.ID))
	}
	return src
}
