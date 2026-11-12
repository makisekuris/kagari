package personal

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"kagari/internal/channel"
)

var ErrTaskInputAccess = errors.New("task input access denied")

const MaxPendingInputs = 10

// IsPureURLMessage accepts one or more whitespace-separated HTTP(S) URLs and nothing else.
func IsPureURLMessage(text string) bool {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		u, err := url.Parse(part)
		if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" ||
			(!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
			return false
		}
	}
	return true
}

// TaskInputReference points to an immutable user event assigned to one run.
type TaskInputReference struct {
	EventID     int64                `json:"event_id"`
	Kind        string               `json:"kind"`
	Order       int                  `json:"order"`
	TextRunes   int                  `json:"text_runes"`
	TextPreview string               `json:"text_preview,omitempty"`
	Attachments []channel.Attachment `json:"attachments,omitempty"`
}

type TaskInputProcessingRepository interface {
	ReadTaskInput(context.Context, int64, int64) (Event, error)
}
