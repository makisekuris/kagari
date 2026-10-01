package digest

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"kagari/internal/domain"
)

type Report struct {
	UserID      int64                 `json:"user_id"`
	Start       time.Time             `json:"start"`
	End         time.Time             `json:"end"`
	GeneratedAt time.Time             `json:"generated_at"`
	Total       int                   `json:"total"`
	Pending     int                   `json:"pending"`
	Failed      int                   `json:"failed"`
	Entries     []domain.ArchiveEntry `json:"entries"`
}

func Build(userID int64, start, end, now time.Time, entries []domain.ArchiveEntry, total, pending, failed int) Report {
	report := Report{
		UserID: userID, Start: start.UTC(), End: end.UTC(), GeneratedAt: now.UTC(),
		Total: total, Pending: pending, Failed: failed, Entries: make([]domain.ArchiveEntry, 0, len(entries)),
	}
	if start.Before(end) {
		for _, entry := range entries {
			// 周报范围按提交 ReceivedAt 的半开区间裁剪，不按原文发布日期。
			receivedAt := entry.Submission.ReceivedAt
			if entry.Submission.UserID == userID && !receivedAt.Before(start) && receivedAt.Before(end) {
				report.Entries = append(report.Entries, entry)
			}
		}
	}
	sort.SliceStable(report.Entries, func(i, j int) bool {
		left, right := report.Entries[i], report.Entries[j]
		if left.Submission.ReceivedAt.Equal(right.Submission.ReceivedAt) {
			return left.JobID < right.JobID
		}
		return left.Submission.ReceivedAt.Before(right.Submission.ReceivedAt)
	})
	return report
}

func Render(report Report, timezone string) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
		timezone = "UTC"
	}
	start, end := report.Start.In(loc), report.End.In(loc)
	var out strings.Builder
	fmt.Fprintf(&out, "每周阅读汇总（用户 %d）\n", report.UserID)
	fmt.Fprintf(&out, "覆盖区间：[%s, %s)（%s）；按收录时间统计，不按文章发布日期。\n",
		start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"), timezone)
	fmt.Fprintf(&out, "提交 %d 条；已完成分析 %d 条；待处理 %d 条；失败 %d 条。\n",
		report.Total, len(report.Entries), report.Pending, report.Failed)
	failedSources, truncatedSources := sourceCounts(report.Entries)
	fmt.Fprintf(&out, "来源读取失败 %d 个；正文截断 %d 个。\n", failedSources, truncatedSources)
	out.WriteString("原文正文已保存在本地归档；本周报只列摘要与链接，不重复输出全文。\n")
	out.WriteString("摘要与评价由 AI 协助整理；重要信息请以原文为准。\n")
	fmt.Fprintf(&out, "生成时间：%s\n", report.GeneratedAt.In(loc).Format("2006-01-02 15:04"))
	if len(report.Entries) == 0 {
		out.WriteString("本周期没有已完成分析，也没有可汇总的新信息。\n")
		return out.String()
	}

	groups := make(map[string][]domain.ArchiveEntry)
	for _, entry := range report.Entries {
		category := oneLine(entry.Result.Analysis.Category)
		if category == "" {
			category = "未分类"
		}
		groups[category] = append(groups[category], entry)
	}
	categories := make([]string, 0, len(groups))
	for category := range groups {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	for _, category := range categories {
		fmt.Fprintf(&out, "\n【%s】\n", category)
		entries := append([]domain.ArchiveEntry(nil), groups[category]...)
		sort.SliceStable(entries, func(i, j int) bool {
			left, right := entries[i], entries[j]
			if left.Submission.ReceivedAt.Equal(right.Submission.ReceivedAt) {
				return left.JobID < right.JobID
			}
			return left.Submission.ReceivedAt.Before(right.Submission.ReceivedAt)
		})
		for _, entry := range entries {
			renderEntry(&out, entry, loc)
		}
	}
	return out.String()
}

func renderEntry(out *strings.Builder, entry domain.ArchiveEntry, loc *time.Location) {
	analysis := entry.Result.Analysis
	title := oneLine(analysis.Title)
	if title == "" {
		title = "（无标题）"
	}
	fmt.Fprintf(out, "\n任务 #%d｜%s\n", entry.JobID, title)
	if overview := oneLine(analysis.Overview); overview != "" {
		fmt.Fprintf(out, "概述：%s\n", overview)
	}
	if evaluation := shortEvaluation(analysis.Evaluation); evaluation != "" {
		fmt.Fprintf(out, "评价：%s\n", evaluation)
	}
	fmt.Fprintf(out, "收录时间：%s\n", entry.Submission.ReceivedAt.In(loc).Format("2006-01-02 15:04"))
	for _, link := range sourceLinks(entry) {
		fmt.Fprintf(out, "原文：%s\n", link)
	}
}

func sourceCounts(entries []domain.ArchiveEntry) (failed, truncated int) {
	for _, entry := range entries {
		for _, source := range entry.Result.Sources {
			if sourceFailed(source) {
				failed++
			}
			if source.Truncated {
				truncated++
			}
		}
	}
	return failed, truncated
}

func sourceFailed(source domain.Source) bool {
	if source.Status == "supplied" {
		return false
	}
	if source.Status == "ok" && strings.TrimSpace(source.Content) != "" {
		return false
	}
	return source.Status != "incomplete" || !source.Truncated || strings.TrimSpace(source.Content) == ""
}

func sourceLinks(entry domain.ArchiveEntry) []string {
	links := make([]string, 0, len(entry.Submission.URLs)+len(entry.Result.Sources))
	seen := make(map[string]bool)
	add := func(title, url string) {
		url = strings.TrimSpace(url)
		if url == "" || seen[url] {
			return
		}
		seen[url] = true
		title = oneLine(title)
		if title == "" || title == url {
			links = append(links, url)
		} else {
			links = append(links, title+": "+url)
		}
	}
	for _, url := range entry.Submission.URLs {
		add("", url)
	}
	for _, source := range entry.Result.Sources {
		add(source.Title, source.RequestedURL)
		add(source.Title, source.URL)
	}
	return links
}

func shortEvaluation(claims []domain.Claim) string {
	for _, claim := range claims {
		text := oneLine(claim.Text)
		if text != "" {
			return limitRunes(text, 220)
		}
	}
	return ""
}

func oneLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func limitRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}
