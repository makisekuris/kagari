package render

import (
	"strings"

	"kagari/internal/domain"
)

func Analysis(result domain.Result) string { return strings.TrimSpace(result.Body) }

// Telegram measures message length in UTF-16 units. Plain text avoids markup
// escaping and broken entities when a long report is split.
func Chunks(text string) []string {
	const limit = 3500
	var out []string
	var b strings.Builder
	units := 0
	for _, r := range text {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if units+n > limit {
			out = append(out, b.String())
			b.Reset()
			units = 0
		}
		b.WriteRune(r)
		units += n
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
