package personal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"kagari/internal/channel"
	"kagari/internal/domain"
)

type RunKind string

const (
	RunTask         RunKind = "task"
	RunMemoryReview RunKind = "memory_review"
)

type TaskStatus string

const (
	TaskQueued         TaskStatus = "queued"
	TaskRunning        TaskStatus = "running"
	TaskCompleted      TaskStatus = "completed"
	TaskFailed         TaskStatus = "failed"
	TaskCancelled      TaskStatus = "cancelled"
	TaskAwaitingReview TaskStatus = "awaiting_review"
	TaskWaitingInput   TaskStatus = "waiting_input"
	TaskWaitingBudget  TaskStatus = "waiting_budget"
)

type RunStatus string

const MaxTaskChars = 32_000

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

type Task struct {
	SourceRunIDs []int64              `json:"source_run_ids,omitempty"`
	MemoryRefs   []string             `json:"memory_refs,omitempty"`
	ID           int64                `json:"id"`
	Title        string               `json:"title"`
	Goal         string               `json:"goal"`
	Attachments  []channel.Attachment `json:"attachments,omitempty"`
	Revision     int64                `json:"revision"`
	Status       TaskStatus           `json:"status"`
	Summary      string               `json:"summary"`
	Context      string               `json:"context"`
	BudgetMS     int64                `json:"budget_ms"`
	UsedMS       int64                `json:"used_ms"`
	UpdatedAt    time.Time            `json:"updated_at"`
}

// TaskInput freezes the goal and relevant context for one execution.
type TaskInput struct {
	InputEvents  []TaskInputReference `json:"input_events,omitempty"`
	SourceRunIDs []int64              `json:"source_run_ids,omitempty"`
	MemoryRefs   []string             `json:"memory_refs,omitempty"`
	Trigger      *ScheduleTrigger     `json:"trigger,omitempty"`
	Goal         string               `json:"goal"`
	Context      string               `json:"context"`
	Step         string               `json:"step,omitempty"`
	Previous     string               `json:"previous,omitempty"`
	Attachments  []channel.Attachment `json:"attachments,omitempty"`
}

// DecodeTaskInput validates the persisted JSON contract for one execution.
func DecodeTaskInput(raw string) (TaskInput, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input TaskInput
	if err := decoder.Decode(&input); err != nil {
		return TaskInput{}, fmt.Errorf("personal: decode task input: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return TaskInput{}, errors.New("personal: task input contains trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return TaskInput{}, fmt.Errorf("personal: decode trailing task input: %w", err)
	}
	if strings.TrimSpace(input.Goal) == "" {
		return TaskInput{}, errors.New("personal: task goal is required")
	}
	return input, nil
}

type Run struct {
	ContextManifest string    `json:"context_manifest,omitempty"`
	ID              int64     `json:"id"`
	TaskID          int64     `json:"task_id,omitempty"`
	Revision        int64     `json:"revision,omitempty"`
	EventID         int64     `json:"event_id"`
	Kind            RunKind   `json:"kind"`
	Input           string    `json:"input"`
	Status          RunStatus `json:"status"`
	Result          string    `json:"result"`
	Error           string    `json:"error"`
	ElapsedMS       int64     `json:"elapsed_ms"`
	RemainingMS     int64     `json:"-"` // Computed from the task budget when this run is claimed.
}

type StageResult struct {
	Progress        []ProgressUpdate     `json:"progress,omitempty"`
	AppliedInputIDs []int64              `json:"applied_input_ids,omitempty"`
	Summary         string               `json:"summary"`
	Sources         []domain.SourceRef   `json:"sources,omitempty"`
	Files           []string             `json:"files,omitempty"`
	Attachments     []channel.Attachment `json:"attachments,omitempty"`
}
