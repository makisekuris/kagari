package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/logging"
	"kagari/internal/personal"
)

type PersonalInbox interface {
	Offset(context.Context) (int64, error)
	AcceptInput(context.Context, int64, *personal.Input) (int64, error)
}

type PersonalClient struct {
	token   string
	ownerID int64
	inbox   PersonalInbox
	log     *zap.Logger
	http    *http.Client
	bot     *telegrambot.Bot
	baseURL string
}

func NewPersonal(cfg config.Telegram, inbox PersonalInbox, log *zap.Logger) (*PersonalClient, error) {
	return newPersonalClient(cfg, inbox, log, apiURL, &http.Transport{Proxy: nil})
}

// newPersonalClient is the package-private API fixture seam; NewPersonal uses api.telegram.org.
func newPersonalClient(cfg config.Telegram, inbox PersonalInbox, log *zap.Logger, baseURL string, transport http.RoundTripper) (*PersonalClient, error) {
	if !validToken(cfg.Token) || len(cfg.AllowedUserIDs) != 1 || cfg.AllowedUserIDs[0] <= 0 || len(cfg.TargetChatIDs) != 0 || inbox == nil {
		return nil, errors.New("telegram personal mode requires a token, one positive owner ID, no target chats, and an inbox")
	}
	if log == nil {
		log = zap.NewNop()
	}
	httpClient, bot, baseURL, err := newAPI(cfg.Token, baseURL, transport)
	if err != nil {
		return nil, err
	}
	return &PersonalClient{
		token:   cfg.Token,
		ownerID: cfg.AllowedUserIDs[0],
		inbox:   inbox,
		log:     log,
		http:    httpClient,
		bot:     bot,
		baseURL: baseURL,
	}, nil
}

func (c *PersonalClient) Poll(ctx context.Context) error {
	return pollUpdates(ctx, c.inbox.Offset, c.getUpdates, c.acceptUpdate, func(ctx context.Context, updateID int64) error {
		_, err := c.inbox.AcceptInput(ctx, updateID, nil)
		return err
	}, c.log)
}

func (c *PersonalClient) getUpdates(ctx context.Context, offset int64) ([]json.RawMessage, error) {
	return getUpdates(ctx, c.http, c.baseURL, c.token, offset)
}

func (c *PersonalClient) acceptUpdate(ctx context.Context, update *models.Update) error {
	if update == nil || update.ID <= 0 {
		return errors.New("Telegram returned an unreadable update")
	}
	var input *personal.Input
	if message := update.Message; message != nil && message.Chat.Type == models.ChatTypePrivate && message.Chat.ID == c.ownerID && message.From != nil && message.From.ID == c.ownerID {
		text, entities := message.Text, message.Entities
		if text == "" {
			text, entities = message.Caption, message.CaptionEntities
		}
		text = appendEntityURLs(text, entities)
		if strings.TrimSpace(text) != "" {
			if message.ID <= 0 {
				return errors.New("Telegram update could not be saved")
			}
			var replyToID int64
			if message.ReplyToMessage != nil && message.ReplyToMessage.ID > 0 {
				replyToID = int64(message.ReplyToMessage.ID)
			}
			input = &personal.Input{
				MessageID: int64(message.ID),
				ReplyToID: replyToID,
				Text:      text,
				Material:  isThirdPartyForward(message.ForwardOrigin, c.ownerID),
			}
		}
	}
	eventID, err := c.inbox.AcceptInput(logging.WithLogger(ctx, c.log), update.ID, input)
	if err != nil {
		c.log.Error("telegram.input.failed", zap.Int64("update_id", update.ID))
		return errors.New("Telegram update could not be saved")
	}
	outcome := "accepted"
	if input == nil {
		outcome = "filtered"
	} else if eventID == 0 {
		outcome = "duplicate"
	}
	fields := []zap.Field{zap.Int64("update_id", update.ID), zap.String("outcome", outcome)}
	if input != nil {
		fields = append(fields, zap.Int64("message_id", input.MessageID))
	}
	if eventID != 0 {
		fields = append(fields, zap.Int64("event_id", eventID), zap.Any("payload", input))
	}
	// AcceptInput commits the event and offset together; only now is it accepted.
	c.log.Info("telegram.input", fields...)
	return nil
}

func (c *PersonalClient) Send(ctx context.Context, chatID int64, content domain.Content) (int64, error) {
	if chatID != c.ownerID {
		return 0, &SendError{Reason: "Telegram personal recipient is not the owner", Permanent: true}
	}
	return sendTelegram(c.bot, ctx, chatID, content)
}

func appendEntityURLs(text string, entities []models.MessageEntity) string {
	visible := make(map[string]struct{})
	for _, target := range messageURLs(text, nil) {
		visible[target] = struct{}{}
	}
	var extra []string
	for _, target := range messageURLs(text, entities) {
		if _, ok := visible[target]; ok {
			continue
		}
		visible[target] = struct{}{}
		extra = append(extra, target)
	}
	if len(extra) == 0 {
		return text
	}
	if text != "" {
		text += "\n"
	}
	return text + strings.Join(extra, "\n")
}

func isThirdPartyForward(origin *models.MessageOrigin, ownerID int64) bool {
	if origin == nil {
		return false
	}
	return origin.Type != models.MessageOriginTypeUser || origin.MessageOriginUser == nil || int64(origin.MessageOriginUser.SenderUser.ID) != ownerID
}
