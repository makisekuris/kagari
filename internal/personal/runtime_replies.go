package personal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"kagari/internal/domain"
	"kagari/internal/reader"
)

const replyWaitDelay = 30 * time.Second

func (r *Runtime) waitingReply(ctx context.Context) (bool, error) {
	return r.Store.QueueWaitingReply(ctx, time.Now(), replyWaitDelay)
}

func taskBudgetText(task *Task) string {
	return fmt.Sprintf("「%s」本轮执行时间已用完。已有成果已保存，需要你授权后才能继续。\n\n%s", task.Title, clip(task.Summary, 6000))
}

func taskClosingText(task *Task, sources []domain.SourceRef) string {
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
	seen := make(map[string]bool)
	for _, url := range reader.ExtractURLs(text) {
		seen[url] = true
	}
	var missing []string
	for _, source := range sources {
		if source.Status == "ok" && source.URL != "" && !seen[source.URL] {
			missing = append(missing, source.URL)
			seen[source.URL] = true
		}
	}
	if len(missing) > 0 {
		text += "\n\n本阶段来源：\n" + strings.Join(missing, "\n")
	}
	return text
}
