package assistant

import (
	"context"
	"time"

	"kagari/internal/personal"
	"kagari/internal/personal/agentctx"
)

// Repository is the persistence used by the conversation agent and its tools.
type Repository interface {
	agentctx.ReadRepository
	agentctx.MemoryRepository
	agentctx.BudgetRepository
	agentctx.ProgressRepository

	AddTaskInputs(context.Context, int64, string, int64, int64, []int64) (personal.Task, error)
	CanDeferReply(context.Context, int64) (bool, error)
	CreateTask(context.Context, int64, string, string, personal.TaskInput) (personal.Task, personal.Run, error)
	ReviseTask(context.Context, int64, string, int64, int64, personal.TaskInput) (personal.Task, personal.Run, error)
	CancelTask(context.Context, int64, int64, int64) (personal.Task, error)
	ContinueTask(context.Context, int64, string, int64, int64, string, bool) (personal.Task, personal.Run, error)
	SettleTask(context.Context, int64, int64, int64, personal.TaskStatus, string, ...*personal.CheckAssessment) (personal.Task, error)
	QueryTasks(context.Context, personal.ReadScope, personal.TaskQuery) (personal.TaskPage, error)
	Task(context.Context, int64) (*personal.Task, error)
	WriteMemory(context.Context, int64, string, personal.MemoryChange) (personal.Memory, error)
	MemoryHistory(context.Context, string, int) ([]personal.Memory, error)
	CreateSchedule(context.Context, int64, string, personal.ScheduleInput, time.Time) (personal.Schedule, bool, error)
	UpdateSchedule(context.Context, int64, string, int64, int64, personal.SchedulePatch, time.Time) (personal.Schedule, bool, error)
	CancelSchedule(context.Context, int64, string, int64, int64, time.Time) (personal.Schedule, bool, error)
	Schedule(context.Context, int64) (*personal.Schedule, error)
	Schedules(context.Context, bool, int64, int) ([]personal.Schedule, error)
	ScheduleOccurrences(context.Context, int64, int64, int) ([]personal.ScheduleOccurrence, error)
	ScheduleForTask(context.Context, int64) (*personal.ScheduleContext, error)
	ScheduleSuggestions(context.Context, int64) ([]personal.ScheduleSuggestion, error)
	ProposeScheduleContextUpdate(context.Context, int64, string, int64, personal.ScheduleContextProposal, time.Time) (personal.ScheduleSuggestion, bool, error)
	ResolveScheduleContextUpdate(context.Context, int64, string, int64, int64, string, time.Time) (personal.ScheduleSuggestionResolution, bool, error)
}
