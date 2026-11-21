package service

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/reader"
)

const maxTaskFallbackSummaryBytes = 1400

func taskBudgetText(task *personal.Task, sources []domain.SourceRef) string {
	text := fmt.Sprintf("「%s」本轮执行时间已用完。已有成果已保存，需要你授权后才能继续。", task.Title)
	if summary := taskReplySummary(task); strings.TrimSpace(summary) != "" {
		text += "\n\n" + summary
	}
	return appendTaskSources(text, sources)
}

func taskClosingText(task *personal.Task, sources []domain.SourceRef) string {
	status := "已经处理完了"
	switch task.Status {
	case personal.TaskFailed:
		status = "这次未能完成"
	case personal.TaskWaitingInput:
		status = "需要你补充信息后才能继续"
	}
	text := fmt.Sprintf("「%s」%s。", task.Title, status)
	if summary := taskReplySummary(task); strings.TrimSpace(summary) != "" {
		text += "\n\n" + summary
	}
	return appendTaskSources(text, sources)
}

func taskReplySummary(task *personal.Task) string {
	summary, legacy := stripLegacySourceExcerpt(task.Summary)
	if task.Status == personal.TaskCompleted && !legacy {
		return task.Summary
	}
	return boundTaskSummary(summary, maxTaskFallbackSummaryBytes)
}

func stripLegacySourceExcerpt(summary string) (string, bool) {
	if !strings.Contains(summary, "Partial source evidence returned:") || !strings.Contains(summary, "; returned text:") {
		return summary, false
	}
	lines := strings.Split(summary, "\n")
	kept := make([]string, 0, len(lines))
	droppingExcerpt := false
	for _, line := range lines {
		if droppingExcerpt {
			if !isLegacySourceSummaryLine(line) {
				continue
			}
			droppingExcerpt = false
		}
		if head, _, found := strings.Cut(line, "; returned text:"); found {
			line = head
			droppingExcerpt = true
		}
		kept = append(kept, legacyFailureLabels.Replace(line))
	}
	return strings.Join(kept, "\n"), true
}

var legacyFailureLabels = strings.NewReplacer(
	"Stage did not complete.", "本阶段未能完成。",
	"Partial source evidence returned:", "来源读取情况：",
	"(status: ok)", "（读取完成）",
	"(status: supplied)", "（用户提供）",
	"(status: incomplete)", "（读取不完整）",
	"(status: restricted)", "（访问受限）",
	"(status: failed)", "（读取失败）",
	"; note: ", "，原因是 ",
)

func isLegacySourceSummaryLine(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "- ") {
		return false
	}
	for _, status := range []string{"ok", "supplied", "incomplete", "restricted", "failed"} {
		if strings.Contains(line, " (status: "+status+")") {
			return true
		}
	}
	return false
}

func boundTaskSummary(summary string, limit int) string {
	if len(summary) <= limit {
		return summary
	}
	const omission = "\n\n（中间内容已省略，完整内容保存在任务记录中。）\n\n"
	budget := limit - len(omission)
	if budget <= 0 {
		return "…"
	}
	headBudget := budget * 2 / 3
	tailBudget := budget - headBudget
	headEnd := headBudget
	for headEnd > 0 && !utf8.RuneStart(summary[headEnd]) {
		headEnd--
	}
	tailStart := len(summary) - tailBudget
	for tailStart < len(summary) && !utf8.RuneStart(summary[tailStart]) {
		tailStart++
	}
	return summary[:headEnd] + omission + summary[tailStart:]
}

func appendTaskSources(text string, sources []domain.SourceRef) string {
	seen := make(map[string]bool)
	for _, url := range reader.ExtractURLs(text) {
		seen[url] = true
	}
	available := make([]string, 0, len(sources))
	unavailable := make([]string, 0, len(sources))
	provided := make([]string, 0, len(sources))
	for _, source := range sources {
		if source.URL == "" || seen[source.URL] {
			continue
		}
		seen[source.URL] = true
		switch source.Status {
		case "ok":
			available = append(available, source.URL)
		case "supplied":
			provided = append(provided, source.URL)
		default:
			unavailable = append(unavailable, source.URL+"（"+taskSourceStatusLabel(source.Status)+"）")
		}
	}
	if len(available) > 0 {
		text += "\n\n本阶段来源：\n" + strings.Join(available, "\n")
	}
	if len(provided) > 0 {
		text += "\n\n用户提供的来源：\n" + strings.Join(provided, "\n")
	}
	if len(unavailable) > 0 {
		text += "\n\n未能完整读取的来源：\n" + strings.Join(unavailable, "\n")
	}
	return text
}

func taskSourceStatusLabel(status string) string {
	switch status {
	case "incomplete":
		return "读取不完整"
	case "restricted":
		return "访问受限"
	case "failed":
		return "读取失败"
	default:
		return status
	}
}
