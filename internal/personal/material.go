package personal

import (
	"regexp"
	"time"

	"kagari/internal/domain"
	"kagari/internal/textpage"
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

// ToolRecord is the serialized tool round and the metadata needed to inspect it.
type ToolRecord struct {
	Body        string
	Tools       []string
	ToolCallIDs []string
}

var materialIDPattern = regexp.MustCompile(`\bm_[A-Za-z0-9_]+`)

func MaterialReferences(text string) []string {
	return materialIDPattern.FindAllString(text, -1)
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
