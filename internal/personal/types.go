// Package personal implements one owner's conversation, background tasks and durable memory.
package personal

import (
	"context"
	"time"

	"kagari/internal/domain"
)

type EventKind string

const (
	EventUser     EventKind = "user"
	EventMaterial EventKind = "material"
	EventTask     EventKind = "task"
)

type EventStatus string

const (
	EventQueued    EventStatus = "queued"
	EventRunning   EventStatus = "running"
	EventCompleted EventStatus = "completed"
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

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

type Input struct {
	MessageID int64  `json:"message_id"`
	ReplyToID int64  `json:"reply_to_id,omitempty"`
	Text      string `json:"text"`
	Material  bool   `json:"material,omitempty"`
}

type Event struct {
	ID          int64     `json:"id"`
	Kind        EventKind `json:"kind"`
	Text        string    `json:"text"`
	TaskID      int64     `json:"task_id,omitempty"`
	RunID       int64     `json:"run_id,omitempty"`
	MessageID   int64     `json:"message_id,omitempty"`
	ReplyToID   int64     `json:"reply_to_id,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Message struct {
	EventID int64  `json:"event_id"`
	Role    string `json:"role"`
	Text    string `json:"text"`
	TaskID  int64  `json:"task_id,omitempty"`
}

type Task struct {
	ID        int64      `json:"id"`
	Title     string     `json:"title"`
	Goal      string     `json:"goal"`
	Revision  int64      `json:"revision"`
	Status    TaskStatus `json:"status"`
	Summary   string     `json:"summary"`
	Context   string     `json:"context"`
	BudgetMS  int64      `json:"budget_ms"`
	UsedMS    int64      `json:"used_ms"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// TaskInput freezes the goal and relevant context for one execution.
type TaskInput struct {
	Goal     string `json:"goal"`
	Context  string `json:"context"`
	Step     string `json:"step,omitempty"`
	Previous string `json:"previous,omitempty"`
}

type Run struct {
	ID          int64     `json:"id"`
	TaskID      int64     `json:"task_id,omitempty"`
	Revision    int64     `json:"revision,omitempty"`
	EventID     int64     `json:"event_id"`
	Kind        RunKind   `json:"kind"`
	Input       string    `json:"input"`
	Status      RunStatus `json:"status"`
	Result      string    `json:"result"`
	Error       string    `json:"error"`
	ElapsedMS   int64     `json:"elapsed_ms"`
	RemainingMS int64     `json:"-"` // Computed from the task budget when this run is claimed.
}

type StageResult struct {
	Summary string          `json:"summary"`
	Sources []domain.Source `json:"sources,omitempty"`
}

// Deleted entries retain a tombstone so an older memory review cannot resurrect them.
type Memory struct {
	Key           string    `json:"key"`
	Value         string    `json:"value"`
	Version       int64     `json:"version"`
	SourceEventID int64     `json:"source_event_id"`
	Deleted       bool      `json:"deleted"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type MemoryChange struct {
	Operation       string `json:"operation"` // put or forget
	Key             string `json:"key"`
	Value           string `json:"value"`
	ExpectedVersion int64  `json:"expected_version"` // 0 creates a key that has never existed.
	Evidence        string `json:"evidence"`         // Exact supporting quote from the source user event.
}

type MemoryReviewInput struct {
	Event    Event    `json:"event"`
	Memories []Memory `json:"memories"` // Includes tombstones to preserve identity and versions.
}

type MemoryReview struct {
	Changes  []MemoryChange `json:"changes"`
	Question string         `json:"question"` // Only when clarification is necessary.
}

type Outgoing struct {
	ID       int64
	EventID  int64
	TaskID   int64
	Text     string
	Attempts int
}

type TurnInput struct {
	Event      Event     `json:"event"`
	History    []Message `json:"history"`
	Tasks      []Task    `json:"tasks"`
	Memories   []Memory  `json:"memories"`
	CurrentRun *Run      `json:"current_run,omitempty"`
}

type TurnOutput struct {
	Text   string
	TaskID int64
	Silent bool
}

type ThinkFunc func(context.Context, TurnInput) (TurnOutput, error)
type ExecuteFunc func(context.Context, Run, func(string) error) (StageResult, error)
type ReviewMemoryFunc func(context.Context, MemoryReviewInput) (MemoryReview, error)

// Repository is the shared contract between the conversation, workers and transport.
// Task callbacks may continue or settle their own current task, never change its goal.
type Repository interface {
	BindOwner(context.Context, int64) error
	Recover(context.Context) error
	Offset(context.Context) (int64, error)
	AcceptInput(context.Context, int64, *Input) (int64, error)
	ClaimEvent(context.Context) (*Event, error)
	History(context.Context, int64, int) ([]Message, error)
	CompleteEvent(context.Context, int64, TurnOutput, []string) error
	CreateTask(ctx context.Context, eventID int64, requestKey, title string, input TaskInput) (Task, Run, error)
	ReviseTask(ctx context.Context, eventID int64, requestKey string, taskID, expectedRevision int64, input TaskInput) (Task, Run, error)
	CancelTask(ctx context.Context, eventID, taskID, expectedRevision int64) (Task, error)
	ContinueTask(ctx context.Context, eventID int64, requestKey string, taskID, expectedRevision int64, step string, extendBudget bool) (Task, Run, error)
	SettleTask(ctx context.Context, eventID, taskID, expectedRevision int64, status TaskStatus, summary string) (Task, error)
	Task(context.Context, int64) (*Task, error)
	Tasks(context.Context, int) ([]Task, error)
	Run(context.Context, int64) (*Run, error)
	Runs(ctx context.Context, taskID int64, limit int) ([]Run, error)
	ClaimRun(context.Context, RunKind) (*Run, error)
	Progress(context.Context, int64, string) error
	FinishStage(ctx context.Context, runID int64, result StageResult, failure string) error
	AccountRun(ctx context.Context, runID int64, elapsed time.Duration) (time.Duration, error)
	AccountEvent(ctx context.Context, eventID int64, elapsed time.Duration) error
	WriteMemory(ctx context.Context, eventID int64, requestKey string, change MemoryChange) (Memory, error)
	MemoryReviewInput(ctx context.Context, runID int64) (MemoryReviewInput, error)
	FinishMemoryReview(ctx context.Context, runID int64, review MemoryReview, failure string) error
	Memory(context.Context, string) (*Memory, error)
	Memories(context.Context, string, int) ([]Memory, error)
	MemoryHistory(ctx context.Context, key string, limit int) ([]Memory, error)
	ClaimDelivery(context.Context) (*Outgoing, error)
	UpdateDelivery(context.Context, int64, string, int64, string, time.Time) error
}
