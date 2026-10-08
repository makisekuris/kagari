package telegram

import (
	"context"
	"strconv"
	"strings"

	"kagari/internal/distribution"
	"kagari/internal/domain"
)

const Channel = "telegram"
const messageLimit = 3500

func Targets(chatIDs []int64) []domain.DeliveryTarget {
	targets := make([]domain.DeliveryTarget, 0, len(chatIDs))
	for _, chatID := range chatIDs {
		targets = append(targets, domain.DeliveryTarget{Channel: Channel, Address: strconv.FormatInt(chatID, 10)})
	}
	return targets
}

func Adapter(send func(context.Context, int64, domain.Content) (int64, error)) distribution.Adapter {
	return distribution.Adapter{
		Prepare: prepare,
		Send: func(ctx context.Context, target domain.DeliveryTarget, content domain.Content) (string, error) {
			chatID, err := strconv.ParseInt(target.Address, 10, 64)
			if err != nil || chatID == 0 || strconv.FormatInt(chatID, 10) != target.Address {
				return "", &distribution.SendError{Reason: "Telegram target address is invalid", Permanent: true}
			}
			if send == nil {
				return "", &distribution.SendError{Reason: "Telegram sender is unavailable", Permanent: true}
			}
			messageID, err := send(ctx, chatID, content)
			if err != nil {
				return "", err
			}
			if messageID <= 0 {
				return "", &distribution.SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
			}
			return strconv.FormatInt(messageID, 10), nil
		},
	}
}

func prepare(content domain.Content) ([]string, error) {
	switch content.Format {
	case domain.ContentMarkdown:
		if content.Text == "" {
			return nil, nil
		}
		return []string{content.Text}, nil
	case domain.ContentPlainText:
		return splitUTF16(content.Text), nil
	default:
		return nil, &distribution.SendError{Reason: "Telegram content format is unsupported", Permanent: true}
	}
}

func splitUTF16(text string) []string {
	var chunks []string
	var b strings.Builder
	units := 0
	for _, r := range text {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if units+n > messageLimit {
			chunks = append(chunks, b.String())
			b.Reset()
			units = 0
		}
		b.WriteRune(r)
		units += n
	}
	if b.Len() > 0 {
		chunks = append(chunks, b.String())
	}
	return chunks
}
