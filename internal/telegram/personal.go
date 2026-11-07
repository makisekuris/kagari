package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"
	"kagari/internal/channel"
	"kagari/internal/config"
	"kagari/internal/logging"
)

const Channel = "telegram"

type PersonalClient struct {
	token   string
	account string
	ownerID int64
	inbox   channel.Inbox
	files   channel.Files
	log     *zap.Logger
	http    *http.Client
	bot     *telegrambot.Bot
	baseURL string

	mu          sync.Mutex
	authInvalid bool
	pollCancel  context.CancelFunc
}

func NewPersonal(cfg config.Telegram, inbox channel.Inbox, files channel.Files, log *zap.Logger) (*PersonalClient, error) {
	return newPersonalClient(cfg, inbox, files, log, apiURL, &http.Transport{Proxy: nil})
}

// newPersonalClient is the package-private API fixture seam; NewPersonal uses api.telegram.org.
func newPersonalClient(cfg config.Telegram, inbox channel.Inbox, files channel.Files, log *zap.Logger, baseURL string, transport http.RoundTripper) (*PersonalClient, error) {
	account, err := botAccount(cfg.Token)
	if !validToken(cfg.Token) || err != nil || len(cfg.AllowedUserIDs) != 1 || cfg.AllowedUserIDs[0] <= 0 || inbox == nil || files == nil {
		return nil, errors.New("telegram personal mode requires a token, one positive owner ID, an inbox, and file storage")
	}
	if log == nil {
		log = zap.NewNop()
	}
	httpClient, bot, baseURL, err := newAPI(cfg.Token, baseURL, transport)
	if err != nil {
		return nil, err
	}
	return &PersonalClient{
		token: cfg.Token, account: account, ownerID: cfg.AllowedUserIDs[0], inbox: inbox, files: files,
		log: log, http: httpClient, bot: bot, baseURL: baseURL,
	}, nil
}

func (c *PersonalClient) AccountID() string { return c.account }

func botAccount(token string) (string, error) {
	parts := strings.SplitN(token, ":", 2)
	if len(parts) != 2 {
		return "", errors.New("Telegram token has no bot ID")
	}
	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || id == 0 {
		return "", errors.New("Telegram token has an invalid bot ID")
	}
	return strconv.FormatUint(id, 10), nil
}

func (c *PersonalClient) Poll(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	pollCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.pollCancel = cancel
	if c.authInvalid {
		cancel()
	}
	c.mu.Unlock()
	err := pollUpdates(pollCtx, c.readOffset, c.getUpdates, c.acceptUpdate, c.acceptMalformedUpdate, c.log)
	c.mu.Lock()
	c.pollCancel = nil
	invalid := c.authInvalid
	c.mu.Unlock()
	cancel()
	if ctx.Err() != nil {
		return nil
	}
	if invalid {
		// A Telegram bot token cannot be refreshed in-process; leave workers running until shutdown.
		<-ctx.Done()
		return nil
	}
	return err
}

func (c *PersonalClient) Ready(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authInvalid {
		return &channel.SendError{Reason: "Telegram authorization is invalid", Permanent: true, Waiting: true}
	}
	return nil
}

func (c *PersonalClient) readOffset(ctx context.Context) (int64, error) {
	cursor, err := c.inbox.Cursor(ctx, Channel, c.account)
	if err != nil {
		return 0, err
	}
	if cursor == "" {
		return 0, nil
	}
	offset, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || offset < 0 || strconv.FormatInt(offset, 10) != cursor {
		return 0, errors.New("Telegram update cursor is invalid")
	}
	return offset, nil
}

func (c *PersonalClient) getUpdates(ctx context.Context, offset int64) ([]json.RawMessage, error) {
	updates, err := getUpdates(ctx, c.http, c.baseURL, c.token, offset)
	var apiErr *pollFailure
	if errors.As(err, &apiErr) && (apiErr.code == http.StatusUnauthorized || apiErr.code == http.StatusForbidden) {
		c.markAuthInvalid()
	}
	return updates, err
}

func (c *PersonalClient) acceptMalformedUpdate(ctx context.Context, updateID int64) error {
	if updateID <= 0 || updateID == math.MaxInt64 {
		return errors.New("Telegram returned an unreadable update")
	}
	_, err := c.inbox.AcceptBatch(logging.WithLogger(ctx, c.log), Channel, c.account, strconv.FormatInt(updateID+1, 10), nil)
	if err != nil {
		return errors.New("Telegram update could not be saved")
	}
	c.log.Warn("discarded unreadable Telegram update", zap.Int64("update_id", updateID))
	return nil
}

func (c *PersonalClient) acceptUpdate(ctx context.Context, update *models.Update) error {
	if update == nil || update.ID <= 0 || update.ID == math.MaxInt64 {
		return errors.New("Telegram returned an unreadable update")
	}
	ctx = logging.WithLogger(ctx, c.log)
	var inputs []channel.Input
	input, err := c.inputForUpdate(ctx, update)
	if err != nil {
		return err
	}
	if input != nil {
		inputs = append(inputs, *input)
	}
	eventIDs, err := c.inbox.AcceptBatch(ctx, Channel, c.account, strconv.FormatInt(update.ID+1, 10), inputs)
	if err != nil {
		c.log.Error("telegram.input.failed", zap.Int64("update_id", update.ID))
		return errors.New("Telegram update could not be saved")
	}
	outcome := "filtered"
	if input != nil {
		outcome = "accepted"
		if len(eventIDs) == 0 || eventIDs[0] == 0 {
			outcome = "duplicate"
		}
	}
	fields := []zap.Field{zap.Int64("update_id", update.ID), zap.String("outcome", outcome), zap.String("account", c.account)}
	if input != nil {
		fields = append(fields, zap.String("message_id", input.MessageID))
	}
	if len(eventIDs) > 0 && eventIDs[0] != 0 {
		fields = append(fields, zap.Int64("event_id", eventIDs[0]))
	}
	c.log.Info("telegram.input", fields...)
	return nil
}

func (c *PersonalClient) inputForUpdate(ctx context.Context, update *models.Update) (*channel.Input, error) {
	message := update.Message
	if message == nil || message.Chat.Type != models.ChatTypePrivate || message.Chat.ID != c.ownerID || message.From == nil || message.From.ID != c.ownerID {
		return nil, nil
	}
	if message.ID <= 0 {
		return nil, errors.New("Telegram update could not be saved")
	}
	text, entities := message.Text, message.Entities
	if message.Document != nil || text == "" {
		text, entities = message.Caption, message.CaptionEntities
	}
	text = appendEntityURLs(text, entities)
	text = preserveCodeEntities(text, entities)
	attachments := make([]channel.Attachment, 0, 1)
	if message.Document != nil {
		attachment := c.receiveDocument(ctx, message.Document)
		attachments = append(attachments, attachment)
		if attachment.Error != "" {
			note := fmt.Sprintf("[附件 %q 接收失败：%s]", attachment.Name, attachment.Error)
			if text != "" {
				text += "\n"
			}
			text += note
		}
	}
	if media := unsupportedMedia(message); media != "" {
		note := fmt.Sprintf("[收到暂不支持的 Telegram 原生%s；请以普通文件方式重新发送。]", media)
		if text != "" {
			text += "\n"
		}
		text += note
	}
	if text == "" && len(attachments) == 0 {
		return nil, nil
	}
	var replyToID string
	if message.ReplyToMessage != nil && message.ReplyToMessage.ID > 0 {
		replyToID = strconv.Itoa(message.ReplyToMessage.ID)
	}
	return &channel.Input{
		Channel: Channel, Account: c.account, Sender: strconv.FormatInt(c.ownerID, 10),
		MessageID: strconv.Itoa(message.ID), ReplyToID: replyToID, Text: text,
		Attachments: attachments, Material: isThirdPartyForward(message.ForwardOrigin, c.ownerID),
	}, nil
}

func unsupportedMedia(message *models.Message) string {
	switch {
	case len(message.Photo) > 0:
		return "图片"
	case message.Animation != nil:
		return "动画"
	case message.Audio != nil:
		return "音频"
	case message.PaidMedia != nil:
		return "付费媒体"
	case message.Sticker != nil:
		return "贴纸"
	case message.Story != nil:
		return "故事"
	case message.Video != nil:
		return "视频"
	case message.VideoNote != nil:
		return "视频消息"
	case message.Voice != nil:
		return "语音"
	default:
		return ""
	}
}

func (c *PersonalClient) Send(ctx context.Context, content channel.Content) (channel.Receipt, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	receipt := channel.Receipt{Channel: Channel, Account: c.account, Conversation: strconv.FormatInt(c.ownerID, 10)}
	if err := c.Ready(ctx); err != nil {
		return receipt, err
	}
	if content.AttachmentID != "" && content.Text != "" {
		return receipt, &channel.SendError{Reason: "Telegram delivery must contain either text or one attachment", Permanent: true}
	}
	if content.AttachmentID != "" {
		messageID, err := c.sendDocument(ctx, content.AttachmentID)
		if err != nil {
			return receipt, err
		}
		receipt.MessageID = messageID
		return receipt, nil
	}
	if content.Text == "" {
		return receipt, &channel.SendError{Reason: "Telegram delivery is empty", Permanent: true}
	}
	message, err := c.bot.SendMessage(ctx, &telegrambot.SendMessageParams{
		ChatID: c.ownerID, Text: content.Text,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: telegrambot.True()},
	})
	if err != nil {
		return receipt, c.sendError(err)
	}
	if message == nil || message.ID <= 0 {
		return receipt, &channel.SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
	}
	receipt.MessageID = strconv.Itoa(message.ID)
	return receipt, nil
}

func (c *PersonalClient) sendError(err error) *channel.SendError {
	var limited *telegrambot.TooManyRequestsError
	if errors.As(err, &limited) {
		return &channel.SendError{Reason: "Telegram rate limit", RetryAfter: retryAfter(limited.RetryAfter)}
	}
	switch {
	case errors.Is(err, telegrambot.ErrorUnauthorized):
		c.markAuthInvalid()
		return &channel.SendError{Reason: "Telegram authorization is invalid", Permanent: true, Waiting: true}
	case errors.Is(err, telegrambot.ErrorBadRequest):
		return &channel.SendError{Reason: "Telegram rejected the message", Permanent: true}
	case errors.Is(err, telegrambot.ErrorForbidden):
		return &channel.SendError{Reason: "Telegram does not allow this message", Permanent: true}
	default:
		return &channel.SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
	}
}

func (c *PersonalClient) markAuthInvalid() {
	c.mu.Lock()
	c.authInvalid = true
	cancel := c.pollCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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

var _ channel.Client = (*PersonalClient)(nil)
