package assistant

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/personal"
)

type scheduleToolReply struct {
	OK          bool                          `json:"ok"`
	Error       string                        `json:"error,omitempty"`
	Replayed    bool                          `json:"replayed,omitempty"`
	Schedule    *personal.Schedule            `json:"schedule,omitempty"`
	Schedules   []personal.Schedule           `json:"schedules,omitempty"`
	Occurrences []personal.ScheduleOccurrence `json:"occurrences,omitempty"`
	Suggestions []personal.ScheduleSuggestion `json:"suggestions,omitempty"`
}

type updateScheduleArgs struct {
	ScheduleID       int64                  `json:"schedule_id"`
	ExpectedRevision int64                  `json:"expected_revision"`
	Patch            personal.SchedulePatch `json:"patch"`
}

type cancelScheduleArgs struct {
	ScheduleID       int64 `json:"schedule_id"`
	ExpectedRevision int64 `json:"expected_revision"`
}

type listSchedulesArgs struct {
	IncludeCancelled bool  `json:"include_cancelled,omitempty"`
	BeforeID         int64 `json:"before_id,omitempty"`
	Limit            int   `json:"limit,omitempty"`
}

type getScheduleArgs struct {
	ScheduleID         int64 `json:"schedule_id"`
	BeforeOccurrenceID int64 `json:"before_occurrence_id,omitempty"`
	Limit              int   `json:"limit,omitempty"`
}

func scheduleToolError(err error) scheduleToolReply {
	return scheduleToolReply{OK: false, Error: err.Error()}
}

func schedulePageLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, fmt.Errorf("limit cannot be negative")
	}
	if limit == 0 {
		return personal.DefaultSchedulePageSize, nil
	}
	if limit > personal.MaxSchedulePageSize {
		return personal.MaxSchedulePageSize, nil
	}
	return limit, nil
}

func (p *personalTools) buildScheduleTools() ([]tool.BaseTool, error) {
	create, err := utils.InferTool("create_schedule", "登记五段 cron 定时计划；timezone 留空时使用个人默认时区（未配置时为 Asia/Shanghai）。只允许直接用户事件，首次触发严格晚于登记时刻。", func(ctx context.Context, input personal.ScheduleInput) (scheduleToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return scheduleToolError(err), nil
		}
		p.mustNotify = true
		if input.Timezone == "" {
			input.Timezone = p.timezone
			if input.Timezone == "" {
				input.Timezone = personal.DefaultScheduleTimezone
			}
		}
		key, err := p.requestKey("create_schedule", input)
		if err != nil {
			return scheduleToolError(err), nil
		}
		schedule, replayed, err := p.store.CreateSchedule(ctx, p.event.ID, key, input, time.Now().UTC())
		if err != nil {
			return scheduleToolError(err), nil
		}
		p.addScheduleID(schedule.ID, true)
		return scheduleToolReply{OK: true, Replayed: replayed, Schedule: &schedule}, nil
	})
	if err != nil {
		return nil, err
	}

	list, err := utils.InferTool("list_schedules", "按计划 ID 倒序列出计划；默认 20 条，最多 100 条，可用 before_id 继续向前翻页。", func(ctx context.Context, args listSchedulesArgs) (scheduleToolReply, error) {
		if args.BeforeID < 0 {
			return scheduleToolError(fmt.Errorf("before_id cannot be negative")), nil
		}
		limit, err := schedulePageLimit(args.Limit)
		if err != nil {
			return scheduleToolError(err), nil
		}
		schedules, err := p.store.Schedules(ctx, args.IncludeCancelled, args.BeforeID, limit)
		if err != nil {
			return scheduleToolError(err), nil
		}
		return scheduleToolReply{OK: true, Schedules: schedules}, nil
	})
	if err != nil {
		return nil, err
	}

	get, err := utils.InferTool("get_schedule", "查看计划定义、最近触发记录和待处理背景更新建议；触发记录包含对应 Task 当前状态。默认 20 条，最多 100 条，可用 before_occurrence_id 继续向前翻页。", func(ctx context.Context, args getScheduleArgs) (scheduleToolReply, error) {
		if args.ScheduleID <= 0 {
			return scheduleToolError(fmt.Errorf("schedule_id must be positive")), nil
		}
		if args.BeforeOccurrenceID < 0 {
			return scheduleToolError(fmt.Errorf("before_occurrence_id cannot be negative")), nil
		}
		limit, err := schedulePageLimit(args.Limit)
		if err != nil {
			return scheduleToolError(err), nil
		}
		schedule, err := p.store.Schedule(ctx, args.ScheduleID)
		if err != nil {
			return scheduleToolError(err), nil
		}
		if schedule == nil {
			return scheduleToolError(fmt.Errorf("schedule not found")), nil
		}
		occurrences, err := p.store.ScheduleOccurrences(ctx, args.ScheduleID, args.BeforeOccurrenceID, limit)
		if err != nil {
			return scheduleToolError(err), nil
		}
		suggestions, err := p.store.ScheduleSuggestions(ctx, args.ScheduleID)
		if err != nil {
			return scheduleToolError(err), nil
		}
		p.addScheduleID(schedule.ID, false)
		return scheduleToolReply{OK: true, Schedule: schedule, Occurrences: occurrences, Suggestions: suggestions}, nil
	})
	if err != nil {
		return nil, err
	}

	update, err := utils.InferTool("update_schedule", "按当前 revision 修改计划；仅提供需要改变的 patch 字段。只允许直接用户事件。", func(ctx context.Context, args updateScheduleArgs) (scheduleToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return scheduleToolError(err), nil
		}
		p.mustNotify = true
		if args.ScheduleID <= 0 || args.ExpectedRevision <= 0 {
			return scheduleToolError(fmt.Errorf("schedule_id and expected_revision must be positive")), nil
		}
		key, err := p.requestKey("update_schedule", args)
		if err != nil {
			return scheduleToolError(err), nil
		}
		schedule, replayed, err := p.store.UpdateSchedule(ctx, p.event.ID, key, args.ScheduleID, args.ExpectedRevision, args.Patch, time.Now().UTC())
		if err != nil {
			return scheduleToolError(err), nil
		}
		p.addScheduleID(schedule.ID, true)
		return scheduleToolReply{OK: true, Replayed: replayed, Schedule: &schedule}, nil
	})
	if err != nil {
		return nil, err
	}

	cancel, err := utils.InferTool("cancel_schedule", "按当前 revision 永久取消计划；只允许直接用户事件。", func(ctx context.Context, args cancelScheduleArgs) (scheduleToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return scheduleToolError(err), nil
		}
		p.mustNotify = true
		if args.ScheduleID <= 0 || args.ExpectedRevision <= 0 {
			return scheduleToolError(fmt.Errorf("schedule_id and expected_revision must be positive")), nil
		}
		key, err := p.requestKey("cancel_schedule", args)
		if err != nil {
			return scheduleToolError(err), nil
		}
		schedule, replayed, err := p.store.CancelSchedule(ctx, p.event.ID, key, args.ScheduleID, args.ExpectedRevision, time.Now().UTC())
		if err != nil {
			return scheduleToolError(err), nil
		}
		p.addScheduleID(schedule.ID, true)
		return scheduleToolReply{OK: true, Replayed: replayed, Schedule: &schedule}, nil
	})
	if err != nil {
		return nil, err
	}

	return []tool.BaseTool{create, list, get, update, cancel}, nil
}
