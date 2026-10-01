package render

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestChunksPreserveEmojiAndText(t *testing.T) {
	text := strings.Repeat("中文😀\n", 2000)
	chunks := Chunks(text)
	if strings.Join(chunks, "") != text {
		t.Fatal("text changed")
	}
	for _, chunk := range chunks {
		if len(utf16.Encode([]rune(chunk))) > 3500 {
			t.Fatal("message exceeds limit")
		}
	}
}
