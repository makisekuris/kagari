package personal

import (
	"context"
	"kagari/internal/domain"
	"kagari/internal/textpage"
	"time"
)

// ReadScope is bound by the runtime, never supplied by a model.
type ReadScope struct{ EventID, RunID int64 }
type ListPage struct {
	QueryKey      string `json:"query_key,omitempty"`
	BeforeID      int64  `json:"before_id,omitempty"`
	SnapshotMaxID int64  `json:"snapshot_max_id,omitempty"`
	Limit         int    `json:"limit,omitempty"`
}
type Material struct {
	Seq           int64             `json:"seq"`
	ID            string            `json:"id"`
	EventID       int64             `json:"event_id"`
	RunID         int64             `json:"run_id,omitempty"`
	TaskID        int64             `json:"task_id,omitempty"`
	Kind          string            `json:"kind"`
	Version       string            `json:"version"`
	CollectedAt   time.Time         `json:"collected_at"`
	Source        *domain.SourceRef `json:"source,omitempty"`
	OmittedFields []string          `json:"omitted_fields,omitempty"`
}
type MaterialPage struct {
	QueryKey      string     `json:"query_key"`
	Items         []Material `json:"items"`
	SnapshotMaxID int64      `json:"snapshot_max_id"`
	NextBeforeID  int64      `json:"next_before_id,omitempty"`
	HasMore       bool       `json:"has_more"`
}
type MaterialText struct {
	Material Material      `json:"material"`
	Page     textpage.Page `json:"page"`
}
type SearchPage struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit,omitempty"`
}
type TextMatch struct {
	Offset int    `json:"offset"`
	End    int    `json:"end"`
	Text   string `json:"text"`
}
type MatchPage struct {
	Material   Material    `json:"material"`
	Matches    []TextMatch `json:"matches"`
	NextOffset int         `json:"next_offset"`
	HasMore    bool        `json:"has_more"`
}
type ReadCheckpointUpdate struct {
	ResourceKind   string `json:"resource_kind"`
	ResourceID     string `json:"resource_id"`
	Version        string `json:"version"`
	ExpectedOffset int    `json:"expected_offset"`
	NextOffset     int    `json:"next_offset"`
	Note           string `json:"note"`
}
type ReadCheckpoint struct {
	ReadCheckpointUpdate
	TaskID   int64 `json:"task_id"`
	Revision int64 `json:"revision"`
	RunID    int64 `json:"run_id"`
}
type MaterialRepository interface {
	SaveToolRecord(context.Context, ReadScope, string, string, []string) (string, error)
	SaveSource(context.Context, int64, string, domain.Source) (domain.SourceRef, error)
	ListMaterials(context.Context, ReadScope, int64, ListPage) (MaterialPage, error)
	ReadMaterial(context.Context, ReadScope, string, string, textpage.Request) (MaterialText, error)
	SearchMaterial(context.Context, ReadScope, string, string, SearchPage) (MatchPage, error)
	SaveReadCheckpoint(context.Context, int64, ReadCheckpointUpdate) (ReadCheckpoint, error)
}

const materialSchema = `
CREATE TABLE IF NOT EXISTS personal_materials (
 seq INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE,
 event_id INTEGER NOT NULL REFERENCES personal_events(id),
 run_id INTEGER REFERENCES personal_runs(id), task_id INTEGER REFERENCES personal_tasks(id),
 kind TEXT NOT NULL CHECK(kind IN ('source','tool_record')), capture_key TEXT NOT NULL,
 body TEXT NOT NULL, metadata TEXT NOT NULL CHECK(json_valid(metadata)),
 content_sha256 TEXT NOT NULL CHECK(length(content_sha256)=64),
 byte_count INTEGER NOT NULL CHECK(byte_count>=0 AND byte_count<=41943040),
 char_count INTEGER NOT NULL CHECK(char_count>=0 AND char_count<=10485760), collected_at TEXT NOT NULL,
 CHECK(byte_count=length(CAST(body AS BLOB))), CHECK(kind<>'source' OR (run_id IS NOT NULL AND task_id IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS personal_materials_capture ON personal_materials(event_id,COALESCE(run_id,0),capture_key);
CREATE INDEX IF NOT EXISTS personal_materials_run ON personal_materials(run_id,seq);
CREATE INDEX IF NOT EXISTS personal_materials_task ON personal_materials(task_id,run_id);
CREATE TABLE IF NOT EXISTS personal_task_run_refs (
 task_id INTEGER NOT NULL REFERENCES personal_tasks(id), revision INTEGER NOT NULL CHECK(revision>0),
 source_run_id INTEGER NOT NULL REFERENCES personal_runs(id), authorized_event_id INTEGER NOT NULL REFERENCES personal_events(id),
 PRIMARY KEY(task_id,revision,source_run_id)
);
CREATE TABLE IF NOT EXISTS personal_read_checkpoints (
 task_id INTEGER NOT NULL REFERENCES personal_tasks(id), revision INTEGER NOT NULL CHECK(revision>0),
 resource_kind TEXT NOT NULL CHECK(resource_kind IN ('material','attachment','file')), resource_id TEXT NOT NULL,
 version TEXT NOT NULL, next_offset INTEGER NOT NULL CHECK(next_offset>=0), note TEXT NOT NULL,
 run_id INTEGER NOT NULL REFERENCES personal_runs(id), updated_at TEXT NOT NULL,
 PRIMARY KEY(task_id,revision,resource_kind,resource_id,version)
);
`
