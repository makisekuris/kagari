package personal

import (
	"context"
)

type TurnInput struct {
	RepliedMessage    *Message             `json:"replied_message,omitempty"`
	PendingInputs     []Event              `json:"pending_inputs,omitempty"`
	PendingTaskInputs []Event              `json:"pending_task_inputs,omitempty"`
	ReferencedTaskIDs []int64              `json:"referenced_task_ids,omitempty"`
	ReplySuggestions  []ScheduleSuggestion `json:"reply_suggestions,omitempty"`
	Suggestions       []ScheduleSuggestion `json:"suggestions,omitempty"`
	Schedule          *ScheduleContext     `json:"schedule,omitempty"`
	Event             Event                `json:"event"`
	History           []Message            `json:"history"`
	Tasks             []Task               `json:"tasks"`
	Memories          []Memory             `json:"memories"`
	CurrentRun        *Run                 `json:"current_run,omitempty"`
}

type ReplyKind string

const (
	ReplySilent  ReplyKind = "silent"
	ReplyReceipt ReplyKind = "receipt"
	ReplyContent ReplyKind = "content"
)

type TurnOutput struct {
	ScheduleID  int64
	Text        string
	Attachments []string
	TaskID      int64
	Silent      bool
	ReplyKind   ReplyKind `json:"-"`
}

type ThinkFunc func(context.Context, TurnInput) (TurnOutput, error)
type ExecuteFunc func(context.Context, Run, func(string) error) (StageResult, error)
type ReviewMemoryFunc func(context.Context, MemoryReviewInput) (MemoryReview, error)
