package service

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"kagari/internal/personal"
)

const maxMemoryReceiptValueRunes = 80

// Recover the durable outcomes even when the model fails after a tool commit.
func (r *Runtime) committedActionFallback(ctx context.Context, event personal.Event, fallback personal.TurnOutput) (personal.TurnOutput, error) {
	if event.Kind != personal.EventUser {
		return fallback, nil
	}
	timingReceipts, err := r.Store.EventTimingReceipts(ctx, event.ID)
	if err != nil {
		return fallback, err
	}
	memoryReceipts, err := r.Store.EventMemoryReceipts(ctx, event.ID)
	if err != nil {
		return fallback, err
	}
	if len(timingReceipts) == 0 && len(memoryReceipts) == 0 {
		return fallback, nil
	}
	var lines []string
	for _, receipt := range memoryReceipts {
		if receipt.Deleted {
			lines = append(lines, fmt.Sprintf("长期记忆「%s」已遗忘（版本 %d）。", receipt.Key, receipt.Version))
			continue
		}
		if utf8.RuneCountInString(receipt.Value) > maxMemoryReceiptValueRunes {
			lines = append(lines, fmt.Sprintf("长期记忆「%s」已更新（版本 %d）。", receipt.Key, receipt.Version))
			continue
		}
		lines = append(lines, fmt.Sprintf("长期记忆「%s」已更新为“%s”（版本 %d）。", receipt.Key, receipt.Value, receipt.Version))
	}
	for _, receipt := range timingReceipts {
		label := "计划"
		if receipt.Reminder {
			label = "计时提醒"
		}
		state := map[string]string{"active": "已启用", "paused": "已暂停", "cancelled": "已取消", "completed": "已结束"}[receipt.Status]
		if state == "" {
			state = receipt.Status
		}
		line := fmt.Sprintf("%s #%d「%s」%s", label, receipt.ID, receipt.Title, state)
		if receipt.Next != nil {
			zone := receipt.Timezone
			if zone == "" {
				zone = r.Timezone
			}
			loc := time.UTC
			if zone != "" {
				if configured, err := time.LoadLocation(zone); err == nil {
					loc = configured
				}
			}
			line += "，下次触发时间是 " + receipt.Next.In(loc).Format("2006-01-02 15:04") + "（" + loc.String() + "）"
		}
		lines = append(lines, line+"。")
	}
	out := personal.TurnOutput{Text: strings.Join(lines, "\n") + "\n回复中断，以上已提交操作已保存。", Purpose: personal.DeliveryControl}
	if len(memoryReceipts) == 0 && len(timingReceipts) == 1 && !timingReceipts[0].Reminder {
		out.ScheduleID = timingReceipts[0].ID
	}
	return out, nil
}

func (r *Runtime) committedActionSupplement(ctx context.Context, event personal.Event, output personal.TurnOutput) (personal.TurnOutput, error) {
	recovered, err := r.committedActionFallback(ctx, event, personal.TurnOutput{})
	if err != nil || strings.TrimSpace(recovered.Text) == "" {
		return output, err
	}
	text := recovered.Text
	var attachments []string
	var taskIDs []int64
	if output.SupplementalReply != nil {
		if existing := strings.TrimSpace(output.SupplementalReply.Draft.Text); existing != "" {
			text += "\n\n" + existing
		}
		attachments = append(attachments, output.SupplementalReply.Draft.Attachments...)
		taskIDs = append(taskIDs, output.SupplementalReply.TaskIDs...)
	}
	if feedback := strings.TrimSpace(output.RequiredFeedback); feedback != "" {
		text += "\n\n" + feedback
	}
	taskIDs = append(taskIDs, output.TaskIDs...)
	scheduleID := recovered.ScheduleID
	if output.SupplementalReply != nil || len(taskIDs) > 0 || strings.TrimSpace(output.RequiredFeedback) != "" {
		scheduleID = 0
	}
	output.SupplementalReply = &personal.SupplementalReply{
		Draft:      personal.DeliveryDraft{Text: text, Attachments: attachments},
		TaskIDs:    taskIDs,
		ScheduleID: scheduleID,
		Purpose:    personal.DeliveryControl,
	}
	output.RequiredFeedback = ""
	output.CommittedActionsNeedReceipt = false
	return output, nil
}
