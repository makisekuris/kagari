package channel

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrepareTextPreservesUnicode(t *testing.T) {
	text := strings.Repeat("中文🙂a", 900)
	parts, err := PrepareText(text)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(parts, "") != text {
		t.Fatal("text changed")
	}
	for _, part := range parts {
		if len(part) > 2000 || !utf8.ValidString(part) {
			t.Fatalf("invalid part with %d bytes", len(part))
		}
	}
	if _, err := PrepareText(string([]byte{0xff})); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
}
