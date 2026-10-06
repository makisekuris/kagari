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

func TestAdapterPreservesUnicodeAndRejectsInvalidAddress(t *testing.T) {
	var sent []string
	adapter := Adapter(func(_ context.Context, chatID int64, text string) (int64, error) {
		sent = append(sent, text)
		if chatID != -100123 {
			t.Fatalf("chat ID = %d, want -100123", chatID)
		}
		return 71, nil
	})
	text := "🙂"
	for i := 0; i < 1800; i++ {
		text += "🙂"
	}
	chunks := adapter.Prepare(text)
	if len(chunks) != 1 || chunks[0] != text {
		t.Fatalf("unicode body was split or changed: %d chunks", len(chunks))
	}
	id, err := adapter.Send(context.Background(), domain.DeliveryTarget{Channel: Channel, Address: "-100123"}, chunks[0])
	if err != nil || id != "71" || !reflect.DeepEqual(sent, []string{chunks[0]}) {
		t.Fatalf("Send() = %q, %v, sent %d chunks", id, err, len(sent))
	}
	for _, address := range []string{"0", "01", "+1", " 1", "9223372036854775808"} {
		if _, err := adapter.Send(context.Background(), domain.DeliveryTarget{Channel: Channel, Address: address}, "text"); err == nil {
			t.Fatalf("address %q was accepted", address)
		}
	}
	if len(sent) != 1 {
		t.Fatalf("invalid addresses reached sender; sends = %#v", sent)
	}
	unknown := Adapter(func(context.Context, int64, string) (int64, error) { return 0, nil })
	_, err = unknown.Send(context.Background(), domain.DeliveryTarget{Channel: Channel, Address: "7"}, "text")
	var sendErr *distribution.SendError
	if !errors.As(err, &sendErr) || !sendErr.Uncertain {
		t.Fatalf("zero Telegram receipt error = %#v", err)
	}
	if got := Targets([]int64{7, -100123}); !reflect.DeepEqual(got, []domain.DeliveryTarget{{Channel: Channel, Address: "7"}, {Channel: Channel, Address: "-100123"}}) {
		t.Fatalf("Targets() = %#v", got)
	}
}

func TestAdapterSplitsOversizedMarkdown(t *testing.T) {
	text := strings.Repeat("甲😀", 17000)
	chunks := Adapter(nil).Prepare(text)
	if len(chunks) < 2 || strings.Join(chunks, "") != text {
		t.Fatal("oversized body was lost")
	}
	for _, chunk := range chunks {
		if len(utf16.Encode([]rune(chunk))) > 3500 {
			t.Fatal("fallback chunk exceeds the existing limit")
		}
	}
}
