package telegram

import (
	"errors"
	"net/http"
	"strings"
	"time"

	telegrambot "github.com/go-telegram/bot"
)

const (
	apiURL      = "https://api.telegram.org"
	httpTimeout = 30 * time.Second
)

func newAPI(token, baseURL string, transport http.RoundTripper) (*http.Client, *telegrambot.Bot, string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, nil, "", errors.New("telegram API URL is required")
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
	sdk, err := telegrambot.New(token,
		telegrambot.WithSkipGetMe(),
		telegrambot.WithServerURL(baseURL),
		telegrambot.WithHTTPClient(httpTimeout, httpClient),
	)
	if err != nil {
		return nil, nil, "", errors.New("could not initialize Telegram client")
	}
	return httpClient, sdk, baseURL, nil
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
