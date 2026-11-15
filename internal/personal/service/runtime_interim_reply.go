package service

import (
	"context"
	"strings"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

// The model already produced this text alongside a tool call. Deliver it without
// another model call, leaving the source event open for its tools and final reply.
func (r *Runtime) queueInterimReply(ctx context.Context, event personal.Event, text string) error {
	text = channel.PlainTextMarkdown(text, channel.MarkdownCodeLiterals(event.Text)...)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	chunks, err := r.Prepare(text)
	if err != nil {
		return err
	}
	return r.Store.QueueInterimReply(ctx, event.ID, text, chunks)
}
