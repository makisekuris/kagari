package textclip

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncatePreservesUnicodeAndFitsBudget(t *testing.T) {
	for _, text := range []string{"", "plain text", "中文与🙂混排", "e\u0301 emoji 👩‍💻"} {
		for limit := 0; limit <= utf8.RuneCountInString(text)+1; limit++ {
			got := Truncate(text, limit)
			if !utf8.ValidString(got) || utf8.RuneCountInString(got) > limit {
				t.Fatalf("invalid clipped text %q for limit %d", got, limit)
			}
			if utf8.RuneCountInString(text) <= limit {
				if got != text {
					t.Fatalf("text that fits changed: %q -> %q", text, got)
				}
			} else if limit > 0 && (!strings.HasSuffix(got, "…") || !strings.HasPrefix(text, strings.TrimSuffix(got, "…"))) {
				t.Fatalf("truncation lost its prefix or omission marker: %q -> %q", text, got)
			}
		}
	}
}
