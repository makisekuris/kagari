package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"kagari/internal/domain"
	"kagari/internal/reader"

	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"
)

func (c *Client) acceptUpdate(ctx context.Context, update *models.Update) error {
	if update == nil || update.ID <= 0 {
		return errors.New("Telegram returned an unreadable update")
	}
	kind, key, payload, targetChatID, ackChatID, err := c.jobForUpdate(update)
	if err != nil {
		return errors.New("Telegram update could not be prepared")
	}
	var targets []domain.DeliveryTarget
	if kind == "analyze" {
		targets = Targets(c.config.ChatIDs(ackChatID))
	}
	jobID, created, err := c.store.AcceptUpdate(ctx, update.ID, kind, key, payload, targetChatID, targets...)
	if err != nil {
		return errors.New("Telegram update could not be saved")
	}
	if created && kind == "analyze" && ackChatID != 0 {
		if _, err := c.Send(ctx, ackChatID, c.replies.AskChatID(jobID)); err != nil {
			c.log.Warn("Telegram acknowledgement failed", zap.String("reason", err.Error()))
		}
	}
	return nil
}

func (c *Client) jobForUpdate(update *models.Update) (kind, key string, payload []byte, targetChatID, ackChatID int64, err error) {
	if update == nil || update.Message == nil {
		return "", "", nil, 0, 0, nil
	}
	message := update.Message
	if message.Chat.Type != models.ChatTypePrivate || message.From == nil {
		return "", "", nil, 0, 0, nil
	}
	if _, allowed := c.allowed[message.From.ID]; !allowed {
		return "", "", nil, 0, 0, nil
	}
	text, entities := message.Text, message.Entities
	if text == "" {
		text, entities = message.Caption, message.CaptionEntities
	}
	key = fmt.Sprintf("tg:%d", update.ID)
	if isCommand(text) {
		payload, err = json.Marshal(domain.Command{UserID: message.From.ID, ChatID: message.Chat.ID, Text: text})
		return "command", key, payload, message.Chat.ID, message.Chat.ID, err
	}
	submission := domain.Submission{
		UserID:     message.From.ID,
		ChatID:     message.Chat.ID,
		Text:       text,
		URLs:       messageURLs(text, entities),
		ReceivedAt: time.Unix(int64(message.Date), 0).UTC(),
	}
	forwarded := message.ForwardOrigin != nil
	if origin := message.ForwardOrigin; origin != nil && origin.Type == models.MessageOriginTypeUser && origin.MessageOriginUser != nil && origin.MessageOriginUser.SenderUser.ID == message.From.ID {
		forwarded = false
	}
	if forwarded {
		submission.ForwardedText = text
		submission.Text = ""
	}
	prepared, prepareErr := c.prepare(submission)
	if prepareErr != nil || len(prepared.URLs) == 0 {
		payload, err = json.Marshal(domain.Command{UserID: message.From.ID, ChatID: message.Chat.ID, Text: "请发送包含链接的消息，可附上分析问题或备注。"})
		return "notice", key, payload, message.Chat.ID, message.Chat.ID, err
	}
	// 身份和权限属于提交者；公共目标群只决定结果发到哪里，不能替代 UserID。
	targetChatID = c.config.ChatIDs(message.Chat.ID)[0]
	payload, err = json.Marshal(prepared)
	return "analyze", key, payload, targetChatID, message.Chat.ID, err
}

func isCommand(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false
	}
	command := strings.SplitN(fields[0], "@", 2)[0]
	switch command {
	case "/start", "/help", "/status", "/retry", "/retry_delivery", "/weekly", "/archive", "/archive_show", "/archive_delete":
		return true
	default:
		return false
	}
}

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
