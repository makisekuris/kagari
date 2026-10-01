package render

import (
	"fmt"
	"strings"
	"unicode"

	"kagari/internal/domain"
)

func Analysis(result domain.Result) string {
	a := result.Analysis
	var b strings.Builder
	for _, tag := range a.Tags {
		tag = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				return r
			}
			return -1
		}, tag)
		if tag != "" {
			fmt.Fprintf(&b, "#%s ", tag)
		}
	}
	fmt.Fprintf(&b, "\n%s\n%s\n", a.Title, a.Overview)
	refs := map[string]int{}
	for i, s := range result.Sources {
		refs[s.ID] = i + 1
	}
	for _, group := range []struct {
		Title  string
		Claims []domain.Claim
	}{{"AI 摘要", a.Summary}, {"讨论者观点", a.Discussion}, {"Agent 评价", a.Evaluation}} {
		if len(group.Claims) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s：\n", group.Title)
		for _, c := range group.Claims {
			fmt.Fprintf(&b, "• %s", c.Text)
			for _, id := range c.SourceIDs {
				fmt.Fprintf(&b, " [%d]", refs[id])
			}
			b.WriteByte('\n')
		}
	}
	if a.Relevance != "" {
		fmt.Fprintf(&b, "\n与你的关注点：%s\n", a.Relevance)
	}
	if len(a.Uncertainties) > 0 {
		b.WriteString("\n未确认与限制：\n")
		for _, u := range a.Uncertainties {
			fmt.Fprintf(&b, "• %s\n", u)
		}
	}
	b.WriteString("\n原文与来源：\n")
	for i, s := range result.Sources {
		title := s.Title
		if title == "" {
			title = s.Kind
		}
		fmt.Fprintf(&b, "[%d] %s", i+1, title)
		if s.Author != "" {
			fmt.Fprintf(&b, " · %s", s.Author)
		}
		if s.URL != "" {
			fmt.Fprintf(&b, "\n%s", s.URL)
		}
		if !((s.Status == "ok") || (s.Status == "supplied")) {
			fmt.Fprintf(&b, "（%s）", s.Status)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n由 AI 协助摘要与评价，重要判断请核对原文；提取的正文、来源与追读记录已保存到本地档案。")
	return strings.TrimSpace(b.String())
}

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
