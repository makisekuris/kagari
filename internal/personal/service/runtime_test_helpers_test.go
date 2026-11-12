package service

import (
	"context"
	"time"

	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/personal"
)

func testRuntime(store Repository) *Runtime {
	return &Runtime{
		Store: store, Log: zap.NewNop(), idleDelay: 5 * time.Millisecond,
		PresentReply: func(_ context.Context, _ personal.Event, draft string) (string, error) { return draft, nil },
		Prepare:      func(text string) ([]string, error) { return []string{text}, nil },
		Send: func(context.Context, channel.Content) (channel.Receipt, error) {
			return channel.Receipt{Channel: "telegram", Account: "test-bot", Conversation: "test-owner"}, nil
		},
		ReviewMemory: func(context.Context, personal.MemoryReviewInput) (personal.MemoryReview, error) {
			return personal.MemoryReview{}, nil
		},
	}
}
