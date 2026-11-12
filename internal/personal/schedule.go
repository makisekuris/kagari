package personal

import (
	"time"

	"kagari/internal/channel"
)

type ScheduleStatus string
type WindowMode string

const (
	DefaultSchedulePageSize = 20
	MaxSchedulePageSize     = 100
)

const (
	ScheduleActive     ScheduleStatus = "active"
	SchedulePaused     ScheduleStatus = "paused"
	ScheduleCancelled  ScheduleStatus = "cancelled"
	WindowNone         WindowMode     = "none"
	WindowSinceSuccess WindowMode     = "since_success"
)

type SourceRequirement struct {
	ID    string `json:"id" jsonschema:"description=Stable identifier used to match source assessment results; put web page URLs in the separate url field."`
	Label string `json:"label"`
	URL   string `json:"url,omitempty" jsonschema:"description=HTTP(S) address for web sources; may be empty for non-web sources."`
}

type ScheduleInput struct {
	Title            string              `json:"title"`
	Goal             string              `json:"goal"`
	Context          string              `json:"context"`
	Attachments      []string            `json:"attachments,omitempty"`
	Cron             string              `json:"cron"`
	Timezone         string              `json:"timezone"`
	WindowMode       WindowMode          `json:"window_mode"`
	RequiredSources  []SourceRequirement `json:"required_sources,omitempty"`
	InitialCheckFrom *time.Time          `json:"initial_check_from,omitempty"`
	TaskBudgetMS     *int64              `json:"task_budget_ms,omitempty"`
}

type SchedulePatch struct {
	Title            *string              `json:"title,omitempty"`
	Goal             *string              `json:"goal,omitempty"`
	Context          *string              `json:"context,omitempty"`
	Attachments      *[]string            `json:"attachments,omitempty"`
	Cron             *string              `json:"cron,omitempty"`
	Timezone         *string              `json:"timezone,omitempty"`
	WindowMode       *WindowMode          `json:"window_mode,omitempty"`
	RequiredSources  *[]SourceRequirement `json:"required_sources,omitempty"`
	InitialCheckFrom *time.Time           `json:"initial_check_from,omitempty"`
	TaskBudgetMS     *int64               `json:"task_budget_ms,omitempty"`
	Status           *ScheduleStatus      `json:"status,omitempty"`
}

// ScheduleDefinition is copied into each occurrence and never changed afterward.
type ScheduleDefinition struct {
	Title           string               `json:"title"`
	Goal            string               `json:"goal"`
	Context         string               `json:"context"`
	Attachments     []channel.Attachment `json:"attachments,omitempty"`
	Cron            string               `json:"cron"`
	Timezone        string               `json:"timezone"`
	WindowMode      WindowMode           `json:"window_mode"`
	RequiredSources []SourceRequirement  `json:"required_sources,omitempty"`
	TaskBudgetMS    int64                `json:"task_budget_ms"`
}

type Schedule struct {
	ID            int64 `json:"id"`
	Revision      int64 `json:"revision"`
	ScopeRevision int64 `json:"scope_revision"`
	ScheduleDefinition
	Status            ScheduleStatus `json:"status"`
	NextRunAt         *time.Time     `json:"next_run_at,omitempty"`
	PauseReason       string         `json:"pause_reason,omitempty"`
	BaselineAt        *time.Time     `json:"baseline_at,omitempty"`
	CoveredThrough    *time.Time     `json:"covered_through,omitempty"`
	CreatedEventID    int64          `json:"created_event_id"`
	AuthorizedEventID int64          `json:"authorized_event_id"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

type CheckWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type ScheduleTrigger struct {
	ScheduleID       int64               `json:"schedule_id"`
	ScheduleRevision int64               `json:"schedule_revision"`
	ScopeRevision    int64               `json:"scope_revision"`
	OccurrenceID     int64               `json:"occurrence_id"`
	ScheduledAt      time.Time           `json:"scheduled_at"`
	EnqueuedAt       time.Time           `json:"enqueued_at"`
	StartedAt        *time.Time          `json:"started_at,omitempty"`
	Window           *CheckWindow        `json:"window,omitempty"`
	Timezone         string              `json:"timezone"`
	RequiredSources  []SourceRequirement `json:"required_sources,omitempty"`
	CoverageEligible bool                `json:"coverage_eligible"`
}

type SourceAssessment struct {
	ID       string `json:"id"`
	Complete bool   `json:"complete"`
	Evidence string `json:"evidence"`
}

type CheckAssessment struct {
	Complete        bool               `json:"complete"`
	RequiredSources []SourceAssessment `json:"required_sources"`
	Limitations     []string           `json:"limitations,omitempty"`
}

type ScheduleOccurrence struct {
	ID                   int64              `json:"id"`
	ScheduleID           int64              `json:"schedule_id"`
	ScheduleRevision     int64              `json:"schedule_revision"`
	ScopeRevision        int64              `json:"scope_revision"`
	ScheduledAt          time.Time          `json:"scheduled_at"`
	CheckedAt            time.Time          `json:"checked_at"`
	CoalescedUntil       *time.Time         `json:"coalesced_until,omitempty"`
	Outcome              string             `json:"outcome"`
	Reason               string             `json:"reason,omitempty"`
	TaskID               int64              `json:"task_id,omitempty"`
	EventID              int64              `json:"event_id,omitempty"`
	Definition           ScheduleDefinition `json:"definition"`
	StartedAt            *time.Time         `json:"started_at,omitempty"`
	Window               *CheckWindow       `json:"window,omitempty"`
	CoverageEligible     bool               `json:"coverage_eligible"`
	CheckResult          *CheckAssessment   `json:"check_result,omitempty"`
	CreditedTaskRevision int64              `json:"credited_task_revision,omitempty"`
	CreditedAt           *time.Time         `json:"credited_at,omitempty"`
	TaskStatus           TaskStatus         `json:"task_status,omitempty"`
	TaskSummary          string             `json:"task_summary,omitempty"`
	DeliveryStates       map[string]int     `json:"delivery_states,omitempty"`
}

type ScheduleContext struct {
	Schedule   Schedule           `json:"schedule"`
	Occurrence ScheduleOccurrence `json:"occurrence"`
}
