package domain

import "strings"

// Usable 只允许引用实际取得的正文；截断片段仍须披露限制。
func (s Source) Usable() bool {
	return (s.Status == "ok" || s.Status == "supplied" || s.Status == "incomplete" && s.Truncated) && strings.TrimSpace(s.Content) != ""
}
