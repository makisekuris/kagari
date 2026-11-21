package assistant

import (
	"context"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/personal"
)

type cancelReminderArgs struct {
	ReminderID int64 `json:"reminder_id"`
}

func (p *personalTools) reminderTools() ([]tool.BaseTool, error) {
	create, err := utils.InferTool("create_reminder", "到点直接发送固定文字。倒计时用 steps，每步 after_minutes 从上一步计时，第一步从 start_at（省略即现在）计时。一次 30+10 用两步 30、10，只有要求循环才设 repeat=true。按日历发固定提醒用五段 cron、timezone 和一个 after_minutes=0 的步骤，省略 start_at，repeat=false。", func(ctx context.Context, input personal.ReminderInput) (scheduleToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return scheduleToolError(err), nil
		}
		p.mustNotify = true
		if input.Cron != "" && input.Timezone == "" {
			input.Timezone = p.reminderTimezone()
		}
		// Relative time starts at receipt of the owner's request, including time
		// the event spent waiting for the conversation worker or model.
		if input.Cron == "" && input.StartAt == nil && !p.event.CreatedAt.IsZero() {
			start := p.event.CreatedAt.UTC()
			input.StartAt = &start
		}
		key, err := p.requestKey("create_reminder", input)
		if err != nil {
			return scheduleToolError(err), nil
		}
		reminder, replayed, err := p.store.CreateReminder(ctx, p.event.ID, key, input, time.Now().UTC())
		if err != nil {
			return scheduleToolError(err), nil
		}
		p.recordSupplementalWork(personal.DeliveryControl)
		return p.reminderResult(reminder, replayed), nil
	})
	if err != nil {
		return nil, err
	}
	cancel, err := utils.InferTool("cancel_reminder", "按计时提醒编号停止后续阶段。用 list_schedules 查询计时提醒的 reminder ID。", func(ctx context.Context, args cancelReminderArgs) (scheduleToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return scheduleToolError(err), nil
		}
		p.mustNotify = true
		key, err := p.requestKey("cancel_reminder", args)
		if err != nil {
			return scheduleToolError(err), nil
		}
		reminder, replayed, err := p.store.CancelReminder(ctx, p.event.ID, key, args.ReminderID, time.Now().UTC())
		if err != nil {
			return scheduleToolError(err), nil
		}
		p.recordSupplementalWork(personal.DeliveryControl)
		return p.reminderResult(reminder, replayed), nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{create, cancel}, nil
}

func (p *personalTools) reminderResult(reminder personal.Reminder, replayed bool) scheduleToolReply {
	zone := reminder.Timezone
	if zone == "" {
		zone = p.reminderTimezone()
	}
	return scheduleToolReply{OK: true, Reminder: &reminder, Replayed: replayed, ReminderTimezone: zone}
}

func (p *personalTools) reminderTimezone() string {
	if p.timezone != "" {
		return p.timezone
	}
	return personal.DefaultScheduleTimezone
}
