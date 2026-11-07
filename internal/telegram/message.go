package telegram

import (
	"sort"
	"strings"
	"unicode/utf16"

	"kagari/internal/reader"

	"github.com/go-telegram/bot/models"
)

func messageURLs(text string, entities []models.MessageEntity) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(raw string) {
		normalized, err := reader.NormalizeURL(raw)
		if err != nil {
			return
		}
		if _, ok := seen[normalized]; ok {
			return
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	for _, found := range reader.ExtractURLs(text) {
		add(found)
	}
	for _, entity := range entities {
		switch entity.Type {
		case models.MessageEntityTypeURL:
			if link, ok := utf16Substring(text, entity.Offset, entity.Length); ok {
				for _, found := range reader.ExtractURLs(link) {
					add(found)
				}
			}
		case models.MessageEntityTypeTextLink:
			add(entity.URL)
		}
	}
	return out
}

func utf16Substring(text string, offset, length int) (string, bool) {
	if offset < 0 || length <= 0 {
		return "", false
	}
	units := utf16.Encode([]rune(text))
	if offset > len(units) || length > len(units)-offset {
		return "", false
	}
	end := offset + length
	if splitSurrogate(units, offset) || splitSurrogate(units, end) {
		return "", false
	}
	return string(utf16.Decode(units[offset:end])), true
}

// preserveCodeEntities wraps Telegram's literal Code and Pre spans so later
// Markdown rendering does not interpret their contents as formatting.
func preserveCodeEntities(text string, entities []models.MessageEntity) string {
	type codeSpan struct {
		start, end int
		text       string
	}
	units := utf16.Encode([]rune(text))
	spans := make([]codeSpan, 0)
	for _, entity := range entities {
		if entity.Type != models.MessageEntityTypeCode && entity.Type != models.MessageEntityTypePre {
			continue
		}
		content, ok := utf16Substring(text, entity.Offset, entity.Length)
		if !ok {
			continue
		}
		spans = append(spans, codeSpan{start: entity.Offset, end: entity.Offset + entity.Length, text: content})
	}
	if len(spans) == 0 {
		return text
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	var out strings.Builder
	cursor := 0
	for _, span := range spans {
		if span.start < cursor {
			continue
		}
		out.WriteString(string(utf16.Decode(units[cursor:span.start])))
		fence := codeFence(span.text)
		out.WriteString("\n\n")
		out.WriteString(fence)
		out.WriteByte('\n')
		out.WriteString(span.text)
		out.WriteByte('\n')
		out.WriteString(fence)
		out.WriteString("\n\n")
		cursor = span.end
	}
	out.WriteString(string(utf16.Decode(units[cursor:])))
	return out.String()
}

func codeFence(content string) string {
	longest, current := 0, 0
	for _, r := range content {
		if r == '`' {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}
	length := longest + 1
	if length < 3 {
		length = 3
	}
	return strings.Repeat("`", length)
}

func splitSurrogate(units []uint16, at int) bool {
	return at > 0 && at < len(units) && 0xD800 <= units[at-1] && units[at-1] <= 0xDBFF && 0xDC00 <= units[at] && units[at] <= 0xDFFF
}
