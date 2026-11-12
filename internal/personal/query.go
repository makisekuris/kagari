package personal

import (
	"errors"
	"time"

	"kagari/internal/personal/internal/textclip"
	"kagari/internal/textpage"
)

var ErrRecordNotFound = errors.New("record not found")

type TaskQuery struct {
	ListPage
	Query       string     `json:"query,omitempty"`
	UpdatedFrom *time.Time `json:"updated_from,omitempty"`
	UpdatedTo   *time.Time `json:"updated_to,omitempty"`
}

type TaskPage struct {
	QueryKey      string `json:"query_key"`
	Tasks         []Task `json:"tasks"`
	SnapshotMaxID int64  `json:"snapshot_max_id"`
	NextBeforeID  int64  `json:"next_before_id,omitempty"`
	HasMore       bool   `json:"has_more"`
}

type RunPage struct {
	QueryKey      string `json:"query_key"`
	Runs          []Run  `json:"runs"`
	SnapshotMaxID int64  `json:"snapshot_max_id"`
	NextBeforeID  int64  `json:"next_before_id,omitempty"`
	HasMore       bool   `json:"has_more"`
}

type RecordRequest struct {
	Kind    string `json:"kind"`
	ID      int64  `json:"id"`
	Section string `json:"section,omitempty"`
	textpage.Request
}

type RecordText struct {
	Kind     string        `json:"kind"`
	ID       int64         `json:"id"`
	Section  string        `json:"section"`
	Revision int64         `json:"revision,omitempty"`
	Page     textpage.Page `json:"page"`
}

// TaskCard and RunCard return bounded records safe for tool responses.
func TaskCard(task Task) Task {
	task.Goal = textclip.Truncate(task.Goal, 240)
	task.Context = ""
	task.Summary = textclip.Truncate(task.Summary, 400)
	task.Attachments = nil
	return task
}

func RunCard(run Run) Run {
	run.Input = ""
	run.Result = boundedTaskResult(run.Result)
	run.Error = textclip.Truncate(run.Error, 400)
	run.ContextManifest = ""
	return run
}
