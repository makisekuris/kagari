package telegram

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/store"
)

const (
	apiURL      = "https://api.telegram.org"
	httpTimeout = 30 * time.Second
)

type Client struct {
	config  config.Telegram
	allowed map[int64]struct{}
	store   *store.Store
	prepare func(domain.Submission) (domain.Submission, error)
	replies ReplyTemplate
	log     *zap.Logger
	http    *http.Client
	bot     *telegrambot.Bot
	baseURL string
}

// ReplyTemplate 提供带任务编号的收件回执，具体角色由入口注入。
type ReplyTemplate interface {
	AskChatID(jobID int64) string
}

type SendError struct {
	Reason     string
	RetryAfter time.Duration
	Uncertain  bool
	Permanent  bool
}

func (e *SendError) Error() string {
	if e == nil {
		return ""
	}
	return e.Reason
}

func New(cfg config.Telegram, st *store.Store, prepare func(domain.Submission) (domain.Submission, error), log *zap.Logger, replies ReplyTemplate) (*Client, error) {
	transport := &http.Transport{Proxy: nil}
	return newClient(cfg, st, prepare, log, replies, apiURL, transport)
}

// newClient is the package-private API fixture seam; New always uses api.telegram.org.
func newClient(cfg config.Telegram, st *store.Store, prepare func(domain.Submission) (domain.Submission, error), log *zap.Logger, replies ReplyTemplate, baseURL string, transport http.RoundTripper) (*Client, error) {
	if replies == nil {
		return nil, errors.New("telegram reply template is required")
	}
	if !validToken(cfg.Token) || len(cfg.AllowedUserIDs) == 0 || st == nil || prepare == nil {
		return nil, errors.New("telegram token, allowlist, store, and prepare function are required")
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("telegram API URL is required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	allowed := make(map[int64]struct{}, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		if id <= 0 {
			return nil, errors.New("telegram allowlist IDs must be positive")
		}
		allowed[id] = struct{}{}
	}
	if transport == nil {
		transport = &http.Transport{Proxy: nil}
	}
	httpClient := &http.Client{
		Timeout:   httpTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	baseURL = strings.TrimRight(baseURL, "/")
	sdk, err := telegrambot.New(cfg.Token,
		telegrambot.WithSkipGetMe(),
		telegrambot.WithServerURL(baseURL),
		telegrambot.WithHTTPClient(httpTimeout, httpClient),
	)
	if err != nil {
		return nil, errors.New("could not initialize Telegram client")
	}
	return &Client{config: cfg, allowed: allowed, store: st, prepare: prepare, replies: replies, log: log, http: httpClient, bot: sdk, baseURL: baseURL}, nil
}

func validToken(token string) bool {
	if token == "" || strings.TrimSpace(token) != token {
		return false
	}
	for _, r := range token {
		if !('a' <= r && r <= 'z') && !('A' <= r && r <= 'Z') && !('0' <= r && r <= '9') && r != ':' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func (c *Client) Send(ctx context.Context, chatID int64, text string) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	message, err := c.bot.SendMessage(ctx, &telegrambot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
		LinkPreviewOptions: &models.LinkPreviewOptions{
			IsDisabled: telegrambot.True(),
		},
	})
	if err != nil {
		return 0, sendError(err)
	}
	if message == nil || message.ID == 0 {
		return 0, &SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
	}
	return int64(message.ID), nil
}

func sendError(err error) *SendError {
	var limited *telegrambot.TooManyRequestsError
	if errors.As(err, &limited) {
		return &SendError{Reason: "Telegram rate limit", RetryAfter: retryAfter(limited.RetryAfter)}
	}
	switch {
	case errors.Is(err, telegrambot.ErrorBadRequest):
		return &SendError{Reason: "Telegram rejected the message", Permanent: true}
	case errors.Is(err, telegrambot.ErrorUnauthorized):
		return &SendError{Reason: "Telegram authorization failed", Permanent: true}
	case errors.Is(err, telegrambot.ErrorForbidden):
		return &SendError{Reason: "Telegram does not allow this message", Permanent: true}
	default:
		return &SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
	}
}
