package digest

import (
	"fmt"
	"strings"
)

func Render(report Report) string {
	text := strings.TrimSpace(report.Body)
	if report.FailedAnalyses == 0 {
		return text
	}
	note := fmt.Sprintf("另有 %d 条分析失败，未纳入本周报。", report.FailedAnalyses)
	if text == "" {
		return note
	}
	return text + "\n\n" + note
}
