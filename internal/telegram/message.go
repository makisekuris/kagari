package telegram

import (
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
				add(link)
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

func splitSurrogate(units []uint16, at int) bool {
	return at > 0 && at < len(units) && 0xD800 <= units[at-1] && units[at-1] <= 0xDBFF && 0xDC00 <= units[at] && units[at] <= 0xDFFF
}
