package personal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"kagari/internal/textpage"
)

type EvidenceRef struct {
	Kind string `json:"kind" jsonschema:"enum=material,enum=attachment,enum=run_result"`
	ID   string `json:"id"`
}
type ProgressUpdate struct {
	ID          string        `json:"id"`
	Description string        `json:"description"`
	Status      string        `json:"status" jsonschema:"description=Use reported for a model-reported accomplishment; this does not independently verify completion.,enum=reported,enum=unresolved,enum=failed,enum=superseded"`
	Evidence    []EvidenceRef `json:"evidence,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	Supersedes  string        `json:"supersedes,omitempty"`
}

var errInvalidProgress = errors.New("invalid_progress")

func invalidProgressInput(message string) error {
	return fmt.Errorf("%w: %s", errInvalidProgress, message)
}

func validateProgressUpdates(updates []ProgressUpdate) error {
	if len(updates) > 50 {
		return invalidProgressInput("at most 50 entries per stage")
	}
	seen := map[string]bool{}
	for _, u := range updates {
		if u.ID == "" || runeLen(u.ID) > 128 || seen[u.ID] || strings.TrimSpace(u.Description) == "" || runeLen(u.Description) > 2000 || runeLen(u.Reason) > 2000 || len(u.Evidence) > 50 {
			return invalidProgressInput("unique nonempty ID (max 128), description (max 2000), reason (max 2000), and at most 50 evidence references required")
		}
		seen[u.ID] = true
		if u.Status != "reported" && u.Status != "unresolved" && u.Status != "failed" && u.Status != "superseded" {
			return invalidProgressInput("status must be reported, unresolved, failed, or superseded")
		}
		if (u.Status == "failed" || u.Status == "superseded") && strings.TrimSpace(u.Reason) == "" {
			return invalidProgressInput("failed or superseded entries require a reason")
		}
		for _, ref := range u.Evidence {
			if ref.ID == "" || runeLen(ref.ID) > 128 || (ref.Kind != "material" && ref.Kind != "attachment" && ref.Kind != "run_result") {
				return invalidProgressInput("evidence needs a valid kind and nonempty ID (max 128)")
			}
		}
	}
	return nil
}

type ProgressEntry struct {
	ProgressUpdate
	Seq           int64    `json:"seq"`
	TaskID        int64    `json:"task_id"`
	TaskRevision  int64    `json:"task_revision"`
	Version       int64    `json:"version"`
	RunID         int64    `json:"run_id"`
	EventID       int64    `json:"event_id,omitempty"`
	OmittedFields []string `json:"omitted_fields,omitempty"`
}
type ProgressQuery struct {
	ListPage
	History bool `json:"history,omitempty"`
}
type TaskProgress struct {
	FormatVersion int             `json:"format_version"`
	Version       int64           `json:"version"`
	QueryKey      string          `json:"query_key"`
	SnapshotMaxID int64           `json:"snapshot_max_id"`
	NextBeforeID  int64           `json:"next_before_id,omitempty"`
	HasMore       bool            `json:"has_more"`
	Entries       []ProgressEntry `json:"entries"`
}
type ProgressText struct {
	TaskID int64         `json:"task_id"`
	Seq    int64         `json:"seq"`
	Page   textpage.Page `json:"page"`
}
type ProgressRepository interface {
	TaskProgress(context.Context, ReadScope, int64, ProgressQuery) (TaskProgress, error)
	ReadProgress(context.Context, ReadScope, int64, int64, textpage.Request) (ProgressText, error)
	CorrectProgress(context.Context, int64, int64, int64, int64, []ProgressUpdate) (TaskProgress, error)
	ValidateProgressSubmission(context.Context, Run, []ProgressUpdate, map[string]bool) error
}

const progressSchema = `
CREATE TABLE IF NOT EXISTS personal_progress (
 seq INTEGER PRIMARY KEY, task_id INTEGER NOT NULL REFERENCES personal_tasks(id), task_revision INTEGER NOT NULL,
 version INTEGER NOT NULL, entry_id TEXT NOT NULL, run_id INTEGER NOT NULL REFERENCES personal_runs(id),
 event_id INTEGER REFERENCES personal_events(id), update_json TEXT NOT NULL CHECK(json_valid(update_json)),
 UNIQUE(task_id,task_revision,entry_id,version)
);
CREATE INDEX IF NOT EXISTS personal_progress_task ON personal_progress(task_id,task_revision,version);
CREATE INDEX IF NOT EXISTS personal_progress_cursor ON personal_progress(task_id,seq DESC);
CREATE INDEX IF NOT EXISTS personal_progress_latest ON personal_progress(task_id,task_revision,entry_id,seq DESC);
`
