package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"kagari/internal/domain"
	"kagari/internal/render"
	"kagari/internal/store"
)

const archivePageSize = 10

func (w *Worker) archiveCommand(ctx context.Context, c domain.Command, cmd string, fields []string) string {
	if c.UserID <= 0 {
		return "无法读取归档"
	}
	switch cmd {
	case "/archive":
		return w.listArchive(ctx, c.UserID, fields)
	case "/archive_show":
		if len(fields) != 2 {
			return "用法：/archive_show 任务号"
		}
		id, ok := archiveID(fields[1])
		if !ok {
			return "任务号应为正整数"
		}
		return w.showArchive(ctx, c.UserID, id)
	case "/archive_delete":
		if len(fields) != 2 && (len(fields) != 3 || fields[2] != "confirm") {
			return "用法：/archive_delete 任务号；确认时发送 /archive_delete 任务号 confirm"
		}
		id, ok := archiveID(fields[1])
		if !ok {
			return "任务号应为正整数"
		}
		if len(fields) == 2 {
			return w.previewArchiveDelete(ctx, c.UserID, id)
		}
		return w.deleteArchive(ctx, c.UserID, id)
	}
	return help
}

func (w *Worker) listArchive(ctx context.Context, userID int64, fields []string) string {
	if len(fields) > 2 {
		return "用法：/archive [页码]"
	}
	page := int64(1)
	if len(fields) == 2 {
		parsed, err := strconv.ParseInt(fields[1], 10, 64)
		maxInt := int64(int(^uint(0) >> 1))
		if err != nil || parsed < 1 || parsed-1 > maxInt/archivePageSize {
			return "页码应为可用的正整数"
		}
		page = parsed
	}
	entries, err := w.Store.ListArchive(ctx, userID, archivePageSize, int((page-1)*archivePageSize))
	if err != nil {
		return "读取归档失败"
	}
	if len(entries) == 0 {
		return fmt.Sprintf("第 %d 页没有归档。示例：/archive 1；/archive_show 123", page)
	}
	var b strings.Builder
	location := archiveLocation(w.Config.Weekly.Timezone)
	fmt.Fprintf(&b, "已完成归档（第 %d 页）：", page)
	for i, entry := range entries {
		fmt.Fprintf(&b, "\n%d. #%d %s · %s", i+1, entry.JobID, archiveTitle(entry), entry.Submission.ReceivedAt.In(location).Format("2006-01-02"))
	}
	fmt.Fprintf(&b, "\n查看详情：/archive_show %d", entries[0].JobID)
	if len(entries) == archivePageSize {
		fmt.Fprintf(&b, "\n下一页：/archive %d", page+1)
	}
	return b.String()
}

func (w *Worker) showArchive(ctx context.Context, userID, id int64) string {
	entry, err := w.Store.ArchiveEntry(ctx, userID, id)
	if err != nil {
		return "读取归档失败"
	}
	if entry == nil {
		return "归档不存在或不属于你"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "原始提交（任务 #%d，收录于 %s）\n提交文本：\n%s\n转发内容：\n%s\n备注：\n%s\n链接：", id, entry.Submission.ReceivedAt.In(archiveLocation(w.Config.Weekly.Timezone)).Format("2006-01-02"), emptyArchiveField(entry.Submission.Text), emptyArchiveField(entry.Submission.ForwardedText), emptyArchiveField(entry.Submission.Note))
	if len(entry.Submission.URLs) == 0 {
		b.WriteString("\n（无）")
	} else {
		for _, url := range entry.Submission.URLs {
			fmt.Fprintf(&b, "\n• %s", url)
		}
	}
	fmt.Fprintf(&b, "\n\n%s", render.Analysis(entry.Result))
	return b.String()
}

func emptyArchiveField(value string) string {
	if value == "" {
		return "（无）"
	}
	return value
}

func (w *Worker) previewArchiveDelete(ctx context.Context, userID, id int64) string {
	entry, err := w.Store.ArchiveEntry(ctx, userID, id)
	if err != nil {
		return "读取归档失败"
	}
	if entry == nil {
		return "归档不存在或不属于你"
	}
	counts, err := w.Store.DeliveryCounts(ctx, id)
	if err != nil {
		return "读取归档失败"
	}
	deliveryCount := 0
	for _, count := range counts {
		deliveryCount += count
	}
	return fmt.Sprintf("准备删除归档 #%d：%s\n将移除原始提交、分析结果、归档中的来源依据和 %d 条投递记录；该条目不会进入新生成的周报，已生成的周报快照和共享来源缓存会保留，已发送的 Telegram 消息不会撤回。\n确认请原样发送：/archive_delete %d confirm", id, archiveTitle(*entry), deliveryCount, id)
}

func (w *Worker) deleteArchive(ctx context.Context, userID, id int64) string {
	entry, err := w.Store.ArchiveEntry(ctx, userID, id)
	if err != nil {
		return "删除归档失败"
	}
	if entry == nil {
		return "归档不存在或不属于你"
	}
	deleted, err := w.Store.DeleteArchiveEntry(ctx, userID, id)
	if errors.Is(err, store.ErrArchiveDeliverySending) {
		return "该归档仍有消息正在投递，请稍后重试删除。"
	}
	if err != nil {
		return "删除归档失败"
	}
	return fmt.Sprintf("归档 #%d 已删除，同时移除 %d 条投递记录。", id, deleted["deliveries"])
}

func archiveID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0
}

func archiveTitle(entry domain.ArchiveEntry) string {
	title := entry.Submission.Text
	for _, source := range entry.Result.Sources {
		if strings.TrimSpace(source.Title) != "" {
			title = source.Title
			break
		}
	}
	if strings.TrimSpace(title) == "" {
		title = entry.Submission.ForwardedText
	}
	if strings.TrimSpace(title) == "" && len(entry.Submission.URLs) > 0 {
		title = entry.Submission.URLs[0]
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return "（无标题）"
	}
	if runes := []rune(title); len(runes) > 80 {
		return string(runes[:79]) + "…"
	}
	return title
}

func archiveLocation(timezone string) *time.Location {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.UTC
	}
	return location
}
