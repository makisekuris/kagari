package telegram

import (
	"context"
	"strconv"
	"unicode/utf8"

	"kagari/internal/distribution"
	"kagari/internal/domain"
	"kagari/internal/render"
)

const Channel = "telegram"

func Targets(chatIDs []int64) []domain.DeliveryTarget {
	targets := make([]domain.DeliveryTarget, 0, len(chatIDs))
	for _, chatID := range chatIDs {
		targets = append(targets, domain.DeliveryTarget{Channel: Channel, Address: strconv.FormatInt(chatID, 10)})
	}
	return targets
}

func Adapter(send func(context.Context, int64, string) (int64, error)) distribution.Adapter {
	return distribution.Adapter{
		Prepare: func(text string) []string {
			if text == "" {
				return nil
			}
			if utf8.RuneCountInString(text) > 32768 {
				// ponytail: oversized Markdown uses existing text cuts; preserve
				// cross-part formatting if these reports need it.
				return render.Chunks(text)
			}
			return []string{text}
		},
		Send: func(ctx context.Context, target domain.DeliveryTarget, text string) (string, error) {
			chatID, err := strconv.ParseInt(target.Address, 10, 64)
			if err != nil || chatID == 0 || strconv.FormatInt(chatID, 10) != target.Address {
				return "", &distribution.SendError{Reason: "Telegram target address is invalid", Permanent: true}
			}
			if send == nil {
				return "", &distribution.SendError{Reason: "Telegram sender is unavailable", Permanent: true}
			}
			messageID, err := send(ctx, chatID, text)
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
