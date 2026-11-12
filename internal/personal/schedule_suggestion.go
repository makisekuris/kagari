package personal

import "time"

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
