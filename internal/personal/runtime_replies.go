package personal

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const replyWaitDelay = 30 * time.Second

func (r *Runtime) waitingReply(ctx context.Context) (bool, error) {
	return r.Store.QueueWaitingReply(ctx, time.Now(), replyWaitDelay)
}

func taskBudgetText(task *Task) string {
	return fmt.Sprintf("「%s」本轮执行时间已用完。已有成果已保存，需要你授权后才能继续。\n\n%s", task.Title, clip(task.Summary, 6000))
}

func taskClosingText(task *Task) string {
	status := "已经处理完了"
	switch task.Status {
	case TaskFailed:
		status = "这次未能完成"
	case TaskWaitingInput:
		status = "需要你补充信息后才能继续"
	}
	text := fmt.Sprintf("「%s」%s。", task.Title, status)
	if strings.TrimSpace(task.Summary) != "" {
		text += "\n\n" + clip(task.Summary, 6000)
	}
	return text
}
