package digest

import (
	"strings"
	"testing"
	"time"

	"kagari/internal/config"
	"kagari/internal/domain"
)

func TestWeeklyDigestCheck(t *testing.T) {
	utc := time.UTC
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start, end := PreviousWeek(time.Date(2025, 1, 1, 12, 0, 0, 0, utc), utc)
	if !start.Equal(time.Date(2024, 12, 23, 0, 0, 0, 0, utc)) || !end.Equal(time.Date(2024, 12, 30, 0, 0, 0, 0, utc)) {
		t.Fatalf("year boundary PreviousWeek() = [%s,%s)", start, end)
	}
	start, end = PreviousWeek(time.Date(2024, 3, 11, 12, 0, 0, 0, newYork), newYork)
	if !start.Equal(time.Date(2024, 3, 4, 0, 0, 0, 0, newYork)) || !end.Equal(time.Date(2024, 3, 11, 0, 0, 0, 0, newYork)) || end.Sub(start) != 167*time.Hour {
		t.Fatalf("DST PreviousWeek() = [%s,%s), duration %s", start, end, end.Sub(start))
	}
	sundayStart, sundayEnd := PreviousWeek(time.Date(2024, 3, 10, 9, 0, 0, 0, newYork), newYork)
	if !sundayStart.Equal(time.Date(2024, 2, 26, 0, 0, 0, 0, newYork)) || !sundayEnd.Equal(time.Date(2024, 3, 4, 0, 0, 0, 0, newYork)) {
		t.Fatalf("Sunday PreviousWeek() = [%s,%s)", sundayStart, sundayEnd)
	}

	weekly := config.Weekly{Enabled: true, Timezone: "America/New_York", Weekday: 1, Time: "09:00"}
	enabledAt := time.Date(2024, 3, 4, 8, 0, 0, 0, newYork)
	now := time.Date(2024, 3, 18, 9, 0, 0, 0, newYork)
	due, err := Due(now, enabledAt, weekly)
	if err != nil || len(due) != 3 {
		t.Fatalf("Due() = (%+v, %v), want three missed weekly periods", due, err)
	}
	seen := map[string]bool{}
	for _, request := range due {
		key := request.Start.String() + "/" + request.End.String()
		if seen[key] {
			t.Fatalf("Due() repeated period %s", key)
		}
		seen[key] = true
	}
	if !due[1].Start.Equal(time.Date(2024, 3, 4, 9, 0, 0, 0, newYork)) || !due[1].End.Equal(time.Date(2024, 3, 11, 9, 0, 0, 0, newYork)) {
		t.Fatalf("DST due period = [%s,%s)", due[1].Start, due[1].End)
	}
	if !due[0].End.Equal(time.Date(2024, 3, 4, 9, 0, 0, 0, newYork)) || !due[2].End.Equal(time.Date(2024, 3, 18, 9, 0, 0, 0, newYork)) {
		t.Fatalf("catch-up windows should use each trigger as cutoff: %+v", due)
	}
	weekly.Weekday = 1
	exactEnabledAt := time.Date(2024, 3, 11, 9, 0, 0, 0, newYork)
	due, err = Due(time.Date(2024, 3, 18, 9, 0, 0, 0, newYork), exactEnabledAt, weekly)
	if err != nil || len(due) != 1 || !due[0].Start.Equal(time.Date(2024, 3, 11, 9, 0, 0, 0, newYork)) {
		t.Fatalf("Due() at exact enabled boundary = (%+v, %v)", due, err)
	}
	sundayGap := config.Weekly{Enabled: true, Timezone: "America/New_York", Weekday: 0, Time: "02:30"}
	gapEnabledAt := time.Date(2024, 3, 3, 3, 0, 0, 0, newYork)
	due, err = Due(time.Date(2024, 3, 10, 1, 59, 0, 0, newYork), gapEnabledAt, sundayGap)
	if err != nil || len(due) != 0 {
		t.Fatalf("Due() ran before nonexistent local time became valid: (%+v, %v)", due, err)
	}
	due, err = Due(time.Date(2024, 3, 10, 3, 0, 0, 0, newYork), gapEnabledAt, sundayGap)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due() missed first valid time after DST gap: (%+v, %v)", due, err)
	}

	periodStart := time.Date(2024, 3, 4, 0, 0, 0, 0, newYork)
	periodEnd := time.Date(2024, 3, 11, 0, 0, 0, 0, newYork)
	entries := []domain.ArchiveEntry{
		{JobID: 20, Submission: domain.Submission{UserID: 7, ChatID: 9, ReceivedAt: time.Date(2024, 3, 8, 12, 0, 0, 0, newYork), URLs: []string{"https://submitted.example/"}}, Result: domain.Result{Analysis: domain.Analysis{Category: "Z", Title: "Second", Overview: "overview Z", Evaluation: []domain.Claim{{Text: "evaluation Z"}}}, Sources: []domain.Source{{RequestedURL: "https://source.example/requested", URL: "https://source.example/final", Status: "error", Truncated: true, Content: "secret full source text"}, {Status: "incomplete", Truncated: true, Content: "partial source"}, {Status: "supplied", Content: "forwarded discussion"}}}},
		{JobID: 10, Submission: domain.Submission{UserID: 7, ReceivedAt: time.Date(2024, 3, 5, 12, 0, 0, 0, newYork)}, Result: domain.Result{Analysis: domain.Analysis{Category: "AI", Title: "First", Overview: "overview AI", Evaluation: []domain.Claim{{Text: "evaluation AI"}}}, Sources: []domain.Source{{Title: "Source", URL: "https://source.example/ai", Status: "ok", Content: "readable article"}}}},
		{JobID: 30, Submission: domain.Submission{UserID: 7, ReceivedAt: periodEnd}},
		{JobID: 31, Submission: domain.Submission{UserID: 8, ReceivedAt: periodStart}},
	}
	report := Build(7, periodStart, periodEnd, now, entries, 4, 1, 1)
	if len(report.Entries) != 2 || report.Entries[0].JobID != 10 || report.Entries[1].JobID != 20 {
		t.Fatalf("Build() kept wrong entries/order: %+v", report.Entries)
	}
	text := Render(report, "America/New_York")
	aiIndex, zIndex := strings.Index(text, "【AI】"), strings.Index(text, "【Z】")
	if aiIndex < 0 || zIndex < 0 || aiIndex > zIndex || !strings.HasPrefix(text, "阅读汇总（用户 7）\n") || !strings.Contains(text, "覆盖区间：从 2024-03-04 00:00（含）至 2024-03-11 00:00（不含）（America/New_York）；按收录时间统计，不按文章发布日期。") || strings.Contains(text, "每周阅读汇总") || strings.Contains(text, "本周") || !strings.Contains(text, "任务 #10") || !strings.Contains(text, "overview AI") || !strings.Contains(text, "evaluation Z") || !strings.Contains(text, "https://submitted.example/") || !strings.Contains(text, "https://source.example/ai") || !strings.Contains(text, "https://source.example/requested") || !strings.Contains(text, "https://source.example/final") || !strings.Contains(text, "来源读取失败 1 个；正文截断 2 个") || !strings.Contains(text, "原文正文已保存在本地归档") || !strings.Contains(text, "本次汇总只列摘要与链接") || !strings.Contains(text, "摘要与评价由 AI 协助整理") {
		t.Fatalf("Render() omitted or misordered report content:\n%s", text)
	}
	if strings.Contains(text, "secret full source text") || !strings.Contains(text, "提交 4 条；已完成分析 2 条；待处理 1 条；失败 1 条") {
		t.Fatalf("Render() exposed body or miscounted articles:\n%s", text)
	}
	empty := Render(Build(7, periodStart, periodEnd, now, nil, 0, 0, 0), "UTC")
	if !strings.Contains(empty, "本周期没有已完成分析") || !strings.Contains(empty, "没有可汇总的新信息") {
		t.Fatalf("empty report not explicit: %s", empty)
	}
}
