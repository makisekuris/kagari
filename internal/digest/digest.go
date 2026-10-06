package digest

import "strings"

func Render(report Report, timezone string) string { return strings.TrimSpace(report.Body) }
