package personal

import (
	"context"
	"time"
)

type ScheduleMemoryRef struct {
	Key      string `json:"key"`
	Version  int64  `json:"version"`
	Evidence string `json:"evidence"`
}

type ContextReplacement struct {
	Old       string `json:"old"`
	New       string `json:"new"`
	MemoryKey string `json:"memory_key"`
}

type ScheduleContextProposal struct {
	ScheduleRevision int64                `json:"schedule_revision"`
	MemoryRefs       []ScheduleMemoryRef  `json:"memory_refs"`
	Replacements     []ContextReplacement `json:"replacements"`
}

type ScheduleSuggestion struct {
	ID               int64                `json:"id"`
	Revision         int64                `json:"revision"`
	ScheduleID       int64                `json:"schedule_id"`
	ScheduleRevision int64                `json:"schedule_revision"`
	OriginEventID    int64                `json:"origin_event_id"`
	TaskID           int64                `json:"task_id"`
	ChangeKey        string               `json:"change_key"`
	MemoryRefs       []ScheduleMemoryRef  `json:"memory_refs"`
	OldContext       string               `json:"old_context"`
	NewContext       string               `json:"new_context"`
	Replacements     []ContextReplacement `json:"replacements"`
	Status           string               `json:"status"`
	PresentedEventID int64                `json:"presented_event_id,omitempty"`
	NotifiedEventID  int64                `json:"notified_event_id,omitempty"`
	CreatedAt        time.Time            `json:"created_at"`
	UpdatedAt        time.Time            `json:"updated_at"`
}

type ScheduleSuggestionResolution struct {
	Status        string             `json:"status"`
	Suggestion    ScheduleSuggestion `json:"suggestion"`
	Schedule      *Schedule          `json:"schedule,omitempty"`
	NeedsProposal bool               `json:"needs_proposal,omitempty"`
}

type ScheduleSuggestionRepository interface {
	ProposeScheduleContextUpdate(context.Context, int64, string, int64, ScheduleContextProposal, time.Time) (ScheduleSuggestion, bool, error)
	ResolveScheduleContextUpdate(context.Context, int64, string, int64, int64, string, time.Time) (ScheduleSuggestionResolution, bool, error)
	ScheduleSuggestions(context.Context, int64) ([]ScheduleSuggestion, error)
	ReplyScheduleSuggestions(context.Context, int64) ([]ScheduleSuggestion, error)
}

const scheduleSuggestionSchema = `
CREATE TABLE IF NOT EXISTS personal_schedule_context_suggestions (
	id INTEGER PRIMARY KEY,
	revision INTEGER NOT NULL CHECK(revision>0),
	schedule_id INTEGER NOT NULL REFERENCES personal_schedules(id),
	schedule_revision INTEGER NOT NULL CHECK(schedule_revision>0),
	origin_event_id INTEGER NOT NULL REFERENCES personal_events(id),
	task_id INTEGER NOT NULL REFERENCES personal_tasks(id),
	change_key TEXT NOT NULL,
	memory_refs_json TEXT NOT NULL,
	old_context TEXT NOT NULL,
	new_context TEXT NOT NULL,
	replacements_json TEXT NOT NULL,
	status TEXT NOT NULL CHECK(status IN ('pending','applied','dismissed')),
	notified_event_id INTEGER REFERENCES personal_events(id),
	presented_event_id INTEGER REFERENCES personal_events(id),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(schedule_id,change_key)
);
`
