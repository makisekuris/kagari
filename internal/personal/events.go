package personal

import (
	"errors"
	"time"

	"kagari/internal/channel"
)

type EventKind string

const (
	EventUser     EventKind = "user"
	EventMaterial EventKind = "material"
	EventTask     EventKind = "task"
	EventSchedule EventKind = "schedule"
)

type EventStatus string

const (
	EventQueued    EventStatus = "queued"
	EventRunning   EventStatus = "running"
	EventCompleted EventStatus = "completed"
)

type Input = channel.Input

var ErrAttachmentUnavailable = errors.New("personal: attachment is unavailable")

type Event struct {
	ScheduleID    int64                `json:"schedule_id,omitempty"`
	ReplyOutboxID int64                `json:"reply_outbox_id,omitempty"`
	ID            int64                `json:"id"`
	Kind          EventKind            `json:"kind"`
	Text          string               `json:"text"`
	TaskID        int64                `json:"task_id,omitempty"`
	RunID         int64                `json:"run_id,omitempty"`
	Channel       string               `json:"channel,omitempty"`
	Account       string               `json:"account,omitempty"`
	Sender        string               `json:"sender,omitempty"`
	MessageID     string               `json:"message_id,omitempty"`
	ReplyToID     string               `json:"reply_to_id,omitempty"`
	Attachments   []channel.Attachment `json:"attachments,omitempty"`
	Interrupted   bool                 `json:"interrupted,omitempty"`
	CreatedAt     time.Time            `json:"created_at"`
}

type Message struct {
	OutboxID    int64                `json:"outbox_id,omitempty"`
	HistoryID   int64                `json:"history_id"`
	ScheduleID  int64                `json:"schedule_id,omitempty"`
	EventID     int64                `json:"event_id"`
	Role        string               `json:"role"`
	Text        string               `json:"text"`
	TaskID      int64                `json:"task_id,omitempty"`
	Channel     string               `json:"channel,omitempty"`
	Account     string               `json:"account,omitempty"`
	Sender      string               `json:"sender,omitempty"`
	MessageID   string               `json:"message_id,omitempty"`
	ReplyToID   string               `json:"reply_to_id,omitempty"`
	Attachments []channel.Attachment `json:"attachments,omitempty"`
}

type Outgoing struct {
	ID           int64
	EventID      int64
	TaskID       int64
	Text         string
	AttachmentID string
	Attempts     int
}
