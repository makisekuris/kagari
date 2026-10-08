package telegram

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"kagari/internal/distribution"
	"kagari/internal/domain"
)

func TestPrepareKeepsMarkdownAndSplitsPlainTextByUTF16(t *testing.T) {
	markdown := strings.Repeat("**😀 原文**\n", 500)
	parts, err := prepare(domain.Content{Text: markdown, Format: domain.ContentMarkdown})
	if err != nil || !reflect.DeepEqual(parts, []string{markdown}) {
		t.Fatalf("markdown prepare = (%d parts, %v)", len(parts), err)
	}

	text := strings.Repeat("🙂", 1800)
	parts, err = prepare(domain.Content{Text: text})
	if err != nil || len(parts) != 2 || len(utf16.Encode([]rune(parts[0]))) != messageLimit || parts[0]+parts[1] != text {
		t.Fatalf("plain prepare = (%d parts, %v)", len(parts), err)
	}
	_, err = prepare(domain.Content{Text: "x", Format: "html"})
	var sendErr *distribution.SendError
	if !errors.As(err, &sendErr) || !sendErr.Permanent {
		t.Fatalf("unknown content format error = %v, want permanent rejection", err)
	}
}

func TestAdapterValidatesAddressAndPreservesReceiptHandling(t *testing.T) {
	content := domain.Content{Text: "**literal**", Format: domain.ContentMarkdown}
	var sent []domain.Content
	adapter := Adapter(func(_ context.Context, chatID int64, got domain.Content) (int64, error) {
		if chatID != -100123 {
			t.Fatalf("chat ID = %d, want -100123", chatID)
		}
		sent = append(sent, got)
		return 71, nil
	})
	target := domain.DeliveryTarget{Channel: Channel, Address: "-100123"}
	for _, address := range []string{"0", "01", "+1", " 1", "9223372036854775808"} {
		if _, err := adapter.Send(context.Background(), domain.DeliveryTarget{Channel: Channel, Address: address}, content); err == nil {
			t.Fatalf("address %q was accepted", address)
		}
	}
	receipt, err := adapter.Send(context.Background(), target, content)
	if err != nil || receipt != "71" || !reflect.DeepEqual(sent, []domain.Content{content}) {
		t.Fatalf("Send() = (%q, %v), sent %#v", receipt, err, sent)
	}
	if got := Targets([]int64{7, -100123}); !reflect.DeepEqual(got, []domain.DeliveryTarget{{Channel: Channel, Address: "7"}, target}) {
		t.Fatalf("Targets() = %#v", got)
	}

	adapter = Adapter(func(context.Context, int64, domain.Content) (int64, error) { return 0, nil })
	_, err = adapter.Send(context.Background(), target, content)
	var sendErr *distribution.SendError
	if !errors.As(err, &sendErr) || !sendErr.Uncertain {
		t.Fatalf("zero Telegram receipt error = %#v", err)
	}
}
