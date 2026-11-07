package channel

import (
	"reflect"
	"testing"
)

func TestPlainTextMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "formatting and links",
			in:   "# **已完成内容**\n\n整理到 **保留来源**：[白桦状态](https://status.example.invalid/path?q=tea&lang=zh)",
			want: "已完成内容\n\n整理到 保留来源：白桦状态 (https://status.example.invalid/path?q=tea&lang=zh)",
		},
		{
			name: "autolink and bare URL",
			in:   "<https://example.org/a?x=1&y=2> https://example.org/b?x=3&y=4",
			want: "https://example.org/a?x=1&y=2 https://example.org/b?x=3&y=4",
		},
		{
			name: "URL punctuation and plain identifiers",
			in:   "https://example.org/?q=*tea*&label=_green_ foo_bar(x) 2 * 3 中文**强调**",
			want: "https://example.org/?q=*tea*&label=_green_ foo_bar(x) 2 * 3 中文强调",
		},
		{
			name: "URL followed by same-line text",
			in:   "https://example.org/path 仍是普通正文",
			want: "https://example.org/path 仍是普通正文",
		},
		{
			name: "URL after Chinese punctuation",
			in:   "来源：https://example.org/file.txt?q=*tea*&label=_green_",
			want: "来源：https://example.org/file.txt?q=*tea*&label=_green_",
		},
		{
			name: "emphasis around URL",
			in:   "**https://example.org/path**",
			want: "https://example.org/path",
		},
		{
			name: "CJK next to URL",
			in:   "前文 https://example.org/path后续中文",
			want: "前文 https://example.org/path后续中文",
		},
		{
			name: "code remains literal",
			in:   "行内 `**Key**_x_\\n`\n\n```go\nvalue := \"**保持** _a_ \\\\n\"\n```",
			want: "行内 **Key**_x_\\n\n\nvalue := \"**保持** _a_ \\\\n\"\n",
		},
		{
			name: "escapes and entities",
			in:   `\*字面星号\* &amp; &#x4e2d; \_keep\_`,
			want: `*字面星号* & 中 _keep_`,
		},
		{
			name: "unclosed delimiters and raw HTML survive",
			in:   "未闭合 **内容 _仍保留\n\n<div>HTML **原样**</div>",
			want: "未闭合 **内容 _仍保留\n\n<div>HTML **原样**</div>",
		},
		{
			name: "lists and table",
			in:   "- 第一\n- 第二\n\n| 名称 | 数量 |\n| --- | --- |\n| 茶 | 2 |",
			want: "• 第一\n• 第二\n\n名称 | 数量\n茶 | 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlainTextMarkdown(tt.in); got != tt.want {
				t.Errorf("PlainTextMarkdown() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMarkdownCodeLiterals(t *testing.T) {
	markdown := "inline `**literal**` and `**literal**`\n\n```text\nline_1\n**block**\n```\n"
	want := []string{"**literal**", "line_1\n**block**"}
	if got := MarkdownCodeLiterals(markdown); !reflect.DeepEqual(got, want) {
		t.Fatalf("MarkdownCodeLiterals() = %#v, want %#v", got, want)
	}
}

func TestPlainTextMarkdownPreservesExplicitCodeLiterals(t *testing.T) {
	markdown := "**literal** and `**literal**`\n\n```text\n**literal**\n```"
	want := "**literal** and **literal**\n\n**literal**\n"
	if got := PlainTextMarkdown(markdown, MarkdownCodeLiterals(markdown)...); got != want {
		t.Fatalf("PlainTextMarkdown() = %q, want %q", got, want)
	}

	markdown = "**literal** and **普通粗体**; foo_bar(x); https://example.org/?q=*tea*&label=_green_"
	want = "**literal** and 普通粗体; foo_bar(x); https://example.org/?q=*tea*&label=_green_"
	if got := PlainTextMarkdown(markdown, "**literal**"); got != want {
		t.Fatalf("PlainTextMarkdown() = %q, want %q", got, want)
	}
}

func TestPlainTextMarkdownProtectsOverlappingAndRepeatedLiterals(t *testing.T) {
	markdown := "**literal** and *literal* and **literal**"
	want := markdown
	got := PlainTextMarkdown(markdown, "literal", "**literal**", "*literal*", "**literal**")
	if got != want {
		t.Fatalf("PlainTextMarkdown() = %q, want %q", got, want)
	}
}

func TestPlainTextMarkdownPlaceholderCollisions(t *testing.T) {
	markdown := "\ue0000\ue001 &#xE000;1&#xE001; \ue002**2**\ue003 **literal** **literal**"
	want := "\ue0000\ue001 \ue0001\ue001 \ue0022\ue003 **literal** **literal**"
	if got := PlainTextMarkdown(markdown, "**literal**", "**literal**"); got != want {
		t.Fatalf("PlainTextMarkdown() = %q, want %q", got, want)
	}
}
