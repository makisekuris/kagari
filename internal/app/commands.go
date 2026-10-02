package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"kagari/internal/digest"
	"kagari/internal/domain"
)

func (w *Worker) notice(ctx context.Context, key string, userID, chatID int64, text string) (int64, bool, error) {
	raw, _ := json.Marshal(domain.Command{UserID: userID, ChatID: chatID, Text: text})
	return w.Store.Enqueue(ctx, "notice", key, raw, chatID)
}

func (w *Worker) command(ctx context.Context, c domain.Command) string {
	fields := strings.Fields(c.Text)
	if len(fields) == 0 {
		return help
	}
	cmd := strings.SplitN(fields[0], "@", 2)[0]
	if cmd == "/start" || cmd == "/help" {
		return help
	}
	if cmd == "/weekly" {
		loc, _ := time.LoadLocation(w.Config.Weekly.Timezone)
		start, end := digest.Window(time.Now(), loc)
		if len(fields) == 3 {
			var err error
			start, err = time.ParseInLocation("2006-01-02", fields[1], loc)
			if err != nil {
				return "日期应为 YYYY-MM-DD"
			}
			end, err = time.ParseInLocation("2006-01-02", fields[2], loc)
			if err != nil {
				return "日期应为 YYYY-MM-DD"
			}
		} else if len(fields) != 1 {
			return "用法：/weekly 或 /weekly 开始日期 结束日期（结束日期不包含）"
		}
		target := c.ChatID
		if w.Config.Telegram.TargetChatID != 0 {
			target = w.Config.Telegram.TargetChatID
		}
		id, created, err := w.EnqueueDigest(ctx, domain.DigestRequest{UserID: c.UserID, Start: start, End: end}, target)
		if err != nil {
			return "周报排队失败：请检查日期范围"
		}
		if !created {
			return fmt.Sprintf("此范围已有周报任务 #%d，可用 /status %d 查看。", id, id)
		}
		return fmt.Sprintf("周报已排队，任务 #%d。", id)
	}
	if len(fields) != 2 {
		return help
	}
	id, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || id <= 0 {
		return "任务号应为正整数"
	}
	job, err := w.Store.Job(ctx, id)
	if err != nil {
		return "读取任务失败"
	}
	if job == nil {
		return "任务不存在或不属于你"
	}
	var owner struct {
		UserID int64 `json:"user_id"`
	}
	// 权限依据原始提交者，而不是可以被路由到公共群组的投递目标。
	if json.Unmarshal(job.Payload, &owner) != nil || owner.UserID != c.UserID {
		return "任务不存在或不属于你"
	}
	switch cmd {
	case "/status":
		text, err := w.Status(ctx, id)
		if err != nil {
			return "读取状态失败"
		}
		return text
	case "/retry":
		if err := w.Store.RetryJob(ctx, id); err != nil {
			return "只有处理失败的任务可以重试"
		}
		return "任务已重新排队"
	case "/retry_delivery":
		if err := w.Store.RetryDeliveries(ctx, id); err != nil {
			return "重新投递失败"
		}
		return "失败或结果不确定的消息已重新排队；结果不确定的消息可能重复出现。"
	}
	return help
}

func (w *Worker) Status(ctx context.Context, id int64) (string, error) {
	job, err := w.Store.Job(ctx, id)
	if err != nil {
		return "", err
	}
	if job == nil {
		return "", errors.New("job not found")
	}
	counts, err := w.Store.DeliveryCounts(ctx, id)
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("任务 #%d：%s（%s），已尝试 %d 次", id, job.Status, job.Kind, job.Attempts)
	if job.LastError != "" {
		text += "\n" + job.LastError
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		text += fmt.Sprintf("\n投递 %s：%d", key, counts[key])
	}
	return text, nil
}

const help = "直接发送或转发含链接的消息，附上你的阅读关注点。\n/status 任务号：查看处理和投递状态\n/retry 任务号：重试处理失败的任务\n/retry_delivery 任务号：重发失败或结果不确定的消息（可能重复）\n/weekly：回顾截至现在的一周内容\n/weekly YYYY-MM-DD YYYY-MM-DD：按收录日期回顾，结束日期不包含"
