package service

import (
	"context"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

// Repository is the persistence used by event and background workers.
type Repository interface {
	Recover(context.Context) error
	ClaimEvent(context.Context) (*personal.Event, error)
	TaskCallbackState(context.Context, int64) (personal.TaskCallbackState, error)
	History(context.Context, int64, int) ([]personal.Message, error)
	CompleteEvent(context.Context, int64, personal.TurnOutput, []string, ...personal.RenderedDelivery) error
	QueueInterimReply(context.Context, int64, string, []string) error
	EventDeliveries(context.Context, int64) ([]personal.DeliveryIntent, error)
	CanDeferReply(context.Context, int64) (bool, error)
	PendingInputs(context.Context, int64) ([]personal.Event, error)
	PendingTaskInputs(context.Context, int64) ([]personal.Event, error)
	ReferencedTasks(context.Context, int64) ([]personal.Task, error)
	Tasks(context.Context, int) ([]personal.Task, error)
	Run(context.Context, int64) (*personal.Run, error)
	Task(context.Context, int64) (*personal.Task, error)
	ContinueTask(context.Context, int64, string, int64, int64, string, bool) (personal.Task, personal.Run, error)
	SettleTask(context.Context, int64, int64, int64, personal.TaskSettlement) (personal.Task, error)
	AccountEvent(context.Context, int64, time.Duration) error
	MemoryReviewInput(context.Context, int64) (personal.MemoryReviewInput, error)
	FinishMemoryReview(context.Context, int64, personal.MemoryReview, string) error
	Memories(context.Context, string, int) ([]personal.Memory, error)
	ClaimRun(context.Context, personal.RunKind) (*personal.Run, error)
	AccountRun(context.Context, int64, time.Duration) (time.Duration, error)
	Progress(context.Context, int64, string) error
	FinishStage(context.Context, int64, personal.StageResult, string) error
	ClaimDelivery(context.Context) (*personal.Outgoing, error)
	UpdateDelivery(context.Context, int64, string, channel.Receipt, string, time.Time) error
	ScheduleForTask(context.Context, int64) (*personal.ScheduleContext, error)
	DispatchDueSchedule(context.Context, time.Time) (*personal.ScheduleOccurrence, error)
	DispatchDueReminder(context.Context, time.Time) (bool, error)
	EventTimingReceipts(context.Context, int64) ([]personal.TimingReceipt, error)
	EventMemoryReceipts(context.Context, int64) ([]personal.MemoryReceipt, error)
	BeginScheduledRun(context.Context, int64, time.Time) (*personal.Run, error)
	ScheduleSuggestions(context.Context, int64) ([]personal.ScheduleSuggestion, error)
	ReplyScheduleSuggestions(context.Context, int64) ([]personal.ScheduleSuggestion, error)
	RepliedMessage(context.Context, int64) (*personal.Message, error)
}
