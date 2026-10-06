package digest

import "strings"

func Render(report Report) string { return strings.TrimSpace(report.Body) }
