// Package textpage provides bounded, rune-based text and list pagination.
package textpage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

const (
	DefaultTextLimit = 4_000
	MaxTextLimit     = 8_000
	DefaultListLimit = 20
	MaxListLimit     = 50
	MaxJSONBytes     = 32 << 10
)

type maxBytesKey struct{}

var (
	ErrInvalidPage          = errors.New("invalid_page")
	ErrVersionRequired      = errors.New("version_required")
	ErrResponseItemTooLarge = errors.New("response_item_too_large")
	ErrInvalidResponseLimit = errors.New("invalid_response_limit")
)

type Request struct {
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit,omitempty"`
	Version string `json:"version,omitempty"`
}

type PageRequest = Request

type Page struct {
	Text            string `json:"text"`
	Offset          int    `json:"offset"`
	End             int    `json:"end"`
	TotalRunes      int    `json:"total_runes"`
	NextOffset      *int   `json:"next_offset,omitempty"`
	HasMore         bool   `json:"has_more"`
	Version         string `json:"version"`
	SourceTruncated bool   `json:"source_truncated"`
}

type VersionChangedError struct {
	Version string
}

func (VersionChangedError) Error() string { return "version_changed" }

func WithMaxBytes(ctx context.Context, max int) context.Context {
	if max < 1 {
		max = 1
	}
	if max > MaxJSONBytes {
		max = MaxJSONBytes
	}
	return context.WithValue(ctx, maxBytesKey{}, max)
}

func MaxBytes(ctx context.Context) int {
	if max, ok := ctx.Value(maxBytesKey{}).(int); ok && max > 0 {
		return min(max, MaxJSONBytes)
	}
	return MaxJSONBytes
}

func (r Request) Validate() (int, error) {
	if r.Offset < 0 || r.Limit < 0 || r.Limit > MaxTextLimit {
		return 0, ErrInvalidPage
	}
	if r.Limit == 0 {
		return DefaultTextLimit, nil
	}
	return r.Limit, nil
}

func ValidateList(afterName string, limit int) (int, error) {
	if limit < 0 || limit > MaxListLimit {
		return 0, ErrInvalidPage
	}
	if limit == 0 {
		return DefaultListLimit, nil
	}
	return limit, nil
}

// Read returns one page. Mutable resources require a version on every later page.
func Read(body string, req Request, mutable bool) (Page, error) {
	limit, err := req.Validate()
	if err != nil || !utf8.ValidString(body) {
		return Page{}, ErrInvalidPage
	}
	version := Version(body)
	if mutable && req.Offset > 0 && req.Version == "" {
		return Page{}, ErrVersionRequired
	}
	if req.Version != "" && req.Version != version {
		return Page{}, &VersionChangedError{Version: version}
	}

	total := utf8.RuneCountInString(body)
	if req.Offset > total {
		return Page{}, ErrInvalidPage
	}
	start := runeByteOffset(body, req.Offset)
	endRune := req.Offset + limit
	if endRune > total {
		endRune = total
	}
	end := runeByteOffset(body, endRune)
	page := Page{
		Text:       body[start:end],
		Offset:     req.Offset,
		End:        endRune,
		TotalRunes: total,
		Version:    version,
	}
	page.updateMore()
	return page, nil
}

func Version(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Shrink removes half of the current page while keeping its rune offsets valid.
func (p *Page) Shrink() bool {
	runes := utf8.RuneCountInString(p.Text)
	if runes <= 1 {
		return false
	}
	keep := runes / 2
	p.Text = p.Text[:runeByteOffset(p.Text, keep)]
	p.End = p.Offset + keep
	p.updateMore()
	return true
}

func (p *Page) updateMore() {
	p.HasMore = p.End < p.TotalRunes
	if p.HasMore {
		next := p.End
		p.NextOffset = &next
	} else {
		p.NextOffset = nil
	}
}

// EncodeBounded marshals the complete value and asks shrink to reduce it until
// the JSON fits. Shrink must preserve complete strings and identifiers.
func EncodeBounded(value any, maxBytes int, shrink func() bool) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, ErrInvalidResponseLimit
	}
	if maxBytes > MaxJSONBytes {
		maxBytes = MaxJSONBytes
	}
	seen := map[string]struct{}{}
	for {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= maxBytes {
			return encoded, nil
		}
		key := string(encoded)
		if _, ok := seen[key]; ok {
			return nil, ErrResponseItemTooLarge
		}
		seen[key] = struct{}{}
		if shrink == nil || !shrink() {
			return nil, ErrResponseItemTooLarge
		}
		next, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		if string(next) == key {
			return nil, ErrResponseItemTooLarge
		}
	}
}

func runeByteOffset(text string, offset int) int {
	i := 0
	for count := 0; count < offset; count++ {
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
	}
	return i
}
