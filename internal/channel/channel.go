// Package channel defines the personal agent's transport-independent messages.
package channel

import (
	"context"
	"errors"
	"io"
	"time"
	"unicode/utf8"
)

const MaxFileBytes int64 = 20_000_000

type Attachment struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	MIME    string `json:"mime,omitempty"`
	Size    int64  `json:"size"`
	Error   string `json:"error,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

type Input struct {
	Channel     string       `json:"channel"`
	Account     string       `json:"account"`
	Sender      string       `json:"sender"`
	MessageID   string       `json:"message_id"`
	ReplyToID   string       `json:"reply_to_id,omitempty"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Material    bool         `json:"material,omitempty"`
}

// Content is one independently tracked delivery: text or one stored file.
type Content struct {
	Text         string `json:"text,omitempty"`
	AttachmentID string `json:"attachment_id,omitempty"`
}

type Receipt struct {
	Channel      string `json:"channel"`
	Account      string `json:"account"`
	Conversation string `json:"conversation"`
	MessageID    string `json:"message_id,omitempty"`
}

type Inbox interface {
	Cursor(context.Context, string, string) (string, error)
	AcceptBatch(context.Context, string, string, string, []Input) ([]int64, error)
}

type Files interface {
	SaveAttachment(ctx context.Context, name, mime string, reader io.Reader) (Attachment, error)
	Attachment(context.Context, string) (Attachment, error)
	OpenAttachment(context.Context, string) (io.ReadCloser, error)
}

type Client interface {
	Poll(context.Context) error
	Ready(context.Context) error
	Send(context.Context, Content) (Receipt, error)
}

// SendError distinguishes a known rejection from an ambiguous remote outcome.
type SendError struct {
	Reason     string
	Permanent  bool
	Uncertain  bool
	Waiting    bool
	RetryAfter time.Duration
}

func (e *SendError) Error() string { return e.Reason }

// PrepareText bounds each delivery in bytes for either transport.
func PrepareText(text string) ([]string, error) {
	if !utf8.ValidString(text) {
		return nil, errors.New("text must be valid UTF-8")
	}
	var parts []string
	for len(text) > 0 {
		end := len(text)
		if end > 2000 {
			end = 2000
			for end > 0 && text[end]&0xc0 == 0x80 {
				end--
			}
		}
		parts = append(parts, text[:end])
		text = text[end:]
	}
	return parts, nil
}
