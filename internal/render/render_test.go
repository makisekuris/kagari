package render

import (
	"strings"
	"testing"
	"unicode/utf16"

	"kagari/internal/domain"
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

func TestAnalysisPreservesMarkdown(t *testing.T) {
	body := "## 我的判断\n\n正文 [原文](https://example.org)"
	if Analysis(domain.Result{Body: body}) != body {
		t.Fatal("model prose was altered")
	}
}
