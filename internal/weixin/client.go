package weixin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"kagari/internal/channel"
)

const Channel = "weixin"

type Client struct {
	credentialPath string
	inbox          channel.Inbox
	files          channel.Files
	log            *zap.Logger
	httpClient     *http.Client

	// Only package tests set these overrides; New always validates Tencent production URLs.
	apiBaseURLOverride string
	cdnBaseURLOverride string
	allowTestURL       bool

	mu            sync.Mutex
	invalidToken  string
	waitingReason string
}

type privateState struct {
	AccountID    string `json:"account_id"`
	UserID       string `json:"user_id"`
	TokenDigest  string `json:"token_digest"`
	ContextToken string `json:"context_token"`
}

func New(credentialPath string, inbox channel.Inbox, files channel.Files, log *zap.Logger) (*Client, error) {
	if strings.TrimSpace(credentialPath) == "" {
		return nil, errors.New("Weixin credential path is empty")
	}
	if inbox == nil || files == nil {
		return nil, errors.New("Weixin client requires inbox and file storage")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{credentialPath: credentialPath, inbox: inbox, files: files, log: log, httpClient: newHTTPClient()}, nil
}

func newClientForTest(credentialPath string, inbox channel.Inbox, files channel.Files, log *zap.Logger, httpClient *http.Client, apiBaseURL, cdnBaseURL string) (*Client, error) {
	if log == nil {
		log = zap.NewNop()
	}
	if httpClient == nil {
		httpClient = newHTTPClient()
	}
	if apiBaseURL != "" {
		if err := validateAPIBaseURL(apiBaseURL, true); err != nil {
			return nil, err
		}
	}
	if cdnBaseURL != "" {
		if err := checkServiceURL(cdnBaseURL, true); err != nil {
			return nil, err
		}
	}
	c, err := New(credentialPath, inbox, files, log)
	if err != nil {
		return nil, err
	}
	c.httpClient = httpClient
	c.apiBaseURLOverride = apiBaseURL
	c.cdnBaseURLOverride = cdnBaseURL
	c.allowTestURL = true
	return c, nil
}

func (c *Client) apiBaseURL(credentials Credentials) string {
	if c.apiBaseURLOverride != "" {
		return c.apiBaseURLOverride
	}
	return credentials.BaseURL
}

func (c *Client) cdnBaseURL() string {
	if c.cdnBaseURLOverride != "" {
		return strings.TrimRight(c.cdnBaseURLOverride, "/")
	}
	return defaultCDNBaseURL
}

func (c *Client) loadCredentials() (Credentials, error) {
	credentials, err := LoadCredentials(c.credentialPath)
	if err != nil {
		return Credentials{}, err
	}
	return credentials, nil
}

func (c *Client) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	credentials, err := c.loadCredentials()
	if errors.Is(err, os.ErrNotExist) {
		return c.waiting("请运行 kagari weixin login 完成微信登录")
	}
	if err != nil {
		return c.waiting("微信凭证无法读取，请检查凭证文件后重新运行服务")
	}
	if c.tokenInvalid(credentials.Token) {
		return c.waiting("微信登录已失效，请运行 kagari weixin login 更新凭证")
	}
	state, err := c.loadState()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !stateMatches(state, credentials) {
		return c.waiting("微信凭证已更新，请向 Bot 发送一条消息以建立新会话")
	}
	c.clearWaiting()
	return nil
}

func (c *Client) Poll(ctx context.Context) error {
	retryDelay := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		credentials, err := c.loadCredentials()
		if errors.Is(err, os.ErrNotExist) {
			c.setWaitingReason("请运行 kagari weixin login 完成微信登录")
			if err := sleepContext(ctx, time.Second); err != nil {
				return nil
			}
			continue
		}
		if err != nil {
			c.setWaitingReason("微信凭证无法读取，请检查凭证文件后重新运行服务")
			if err := sleepContext(ctx, time.Second); err != nil {
				return nil
			}
			continue
		}
		if c.tokenInvalid(credentials.Token) {
			if err := c.waitForCredentialChange(ctx, credentials); err != nil {
				return nil
			}
			continue
		}
		cursor, err := c.inbox.Cursor(ctx, Channel, credentials.AccountID)
		if err != nil {
			return fmt.Errorf("read Weixin cursor: %w", err)
		}
		response, err := c.getUpdates(ctx, credentials, cursor)
		if err != nil {
			var failure *httpFailure
			if errors.As(err, &failure) && (failure.status == http.StatusUnauthorized || failure.status == http.StatusForbidden) {
				c.markTokenInvalid(credentials.Token)
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			if errors.As(err, &failure) && failure.status >= 400 && failure.status < 500 && failure.status != http.StatusRequestTimeout && failure.status != http.StatusTooManyRequests {
				c.log.Error("weixin.poll.waiting_for_credentials", zap.Int("http_status", failure.status))
				c.markTokenInvalid(credentials.Token)
				if c.waitForCredentialChange(ctx, credentials) != nil {
					return nil
				}
				continue
			}
			c.log.Warn("weixin.poll.retrying", zap.Error(err))
			if err := sleepContext(ctx, retryDelay); err != nil {
				return nil
			}
			if retryDelay < 15*time.Second {
				retryDelay *= 2
			} else {
				retryDelay = 30 * time.Second
			}
			continue
		}
		if response.apiError.failed() {
			if response.apiError.code() == -14 {
				c.markTokenInvalid(credentials.Token)
				continue
			}
			c.log.Error("weixin.poll.waiting_for_credentials", zap.Int("api_code", response.apiError.code()))
			c.markTokenInvalid(credentials.Token)
			if c.waitForCredentialChange(ctx, credentials) != nil {
				return nil
			}
			continue
		}
		retryDelay = time.Second
		currentCredentials, err := c.loadCredentials()
		if err != nil || currentCredentials != credentials {
			continue
		}
		inputs, contextToken, err := c.decodeBatch(ctx, credentials, response.Messages)
		if err != nil {
			return err
		}
		currentCredentials, err = c.loadCredentials()
		if err != nil || currentCredentials != credentials {
			continue
		}
		nextCursor := response.Cursor
		if nextCursor == "" {
			nextCursor = cursor
		}
		if _, err := c.inbox.AcceptBatch(ctx, Channel, credentials.AccountID, nextCursor, inputs); err != nil {
			return fmt.Errorf("save Weixin message batch: %w", err)
		}
		if contextToken != "" {
			if _, err := c.saveStateFor(credentials, privateState{AccountID: credentials.AccountID, UserID: credentials.UserID, TokenDigest: tokenDigest(credentials.Token), ContextToken: contextToken}); err != nil {
				return fmt.Errorf("save Weixin session state: %w", err)
			}
			if currentCredentials, err := c.loadCredentials(); err == nil && currentCredentials == credentials {
				c.clearWaiting()
			}
		}
	}
}

func (c *Client) getUpdates(ctx context.Context, credentials Credentials, cursor string) (getUpdatesResponse, error) {
	body := struct {
		Cursor   string   `json:"get_updates_buf"`
		BaseInfo baseInfo `json:"base_info"`
	}{Cursor: cursor, BaseInfo: makeBaseInfo()}
	data, err := postJSONAt(ctx, c.httpClient, c.apiBaseURL(credentials), "ilink/bot/getupdates", credentials.Token, body, longPollTimeout, c.allowTestURL)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return getUpdatesResponse{Cursor: cursor}, nil
		}
		return getUpdatesResponse{}, err
	}
	var response getUpdatesResponse
	if err := parseJSON(data, &response); err != nil {
		return getUpdatesResponse{}, err
	}
	return response, nil
}

func (c *Client) decodeBatch(ctx context.Context, credentials Credentials, messages []wireMessage) ([]channel.Input, string, error) {
	inputs := make([]channel.Input, 0, len(messages))
	latestContext := ""
	for _, message := range messages {
		if message.GroupID != "" {
			c.log.Debug("weixin.group_message.ignored")
			continue
		}
		if message.MessageType != 1 || message.FromUserID != credentials.UserID {
			continue
		}
		if !validWireID(message.MessageID) {
			c.log.Warn("weixin.message.skipped_invalid_id")
			continue
		}
		input := channel.Input{
			Channel:   Channel,
			Account:   credentials.AccountID,
			Sender:    message.FromUserID,
			MessageID: parseWireID(message.MessageID),
		}
		for _, item := range message.ItemList {
			switch {
			case item.Type == 1 || (item.Type == 0 && item.TextItem != nil):
				if item.TextItem != nil {
					if input.Text != "" {
						input.Text += "\n"
					}
					input.Text += item.TextItem.Text
				}
			case item.Type == 4 || (item.Type == 0 && item.FileItem != nil):
				name := "file.bin"
				if item.FileItem != nil && item.FileItem.FileName != "" {
					name = item.FileItem.FileName
				}
				attachment, err := c.downloadAttachment(ctx, credentials, item.FileItem)
				if err != nil {
					input.Attachments = append(input.Attachments, channel.Attachment{Name: name, Error: err.Error()})
				} else {
					input.Attachments = append(input.Attachments, attachment)
				}
			case item.Type == 2 || item.Type == 3 || item.Type == 5:
				input.Attachments = append(input.Attachments, channel.Attachment{Name: "微信媒体", Error: "暂不支持此微信媒体类型，请以普通文件重新发送"})
			}
			if input.ReplyToID == "" && item.RefMsg != nil {
				input.ReplyToID = parseWireID(item.RefMsg.SvrID)
			}
		}
		if message.Context != "" {
			latestContext = message.Context
		}
		inputs = append(inputs, input)
	}
	return inputs, latestContext, nil
}

func (c *Client) Send(ctx context.Context, content channel.Content) (channel.Receipt, error) {
	credentials, err := c.loadCredentials()
	if errors.Is(err, os.ErrNotExist) {
		return channel.Receipt{}, c.waiting("请运行 kagari weixin login 完成微信登录")
	}
	if err != nil {
		return channel.Receipt{}, c.waiting("微信凭证无法读取，请检查凭证文件后重新运行服务")
	}
	receipt := channel.Receipt{Channel: Channel, Account: credentials.AccountID, Conversation: credentials.UserID}
	if c.tokenInvalid(credentials.Token) {
		return receipt, c.waiting("微信登录已失效，请运行 kagari weixin login 更新凭证")
	}
	state, err := c.loadState()
	if err != nil || !stateMatches(state, credentials) {
		return receipt, c.waiting("微信凭证已更新，请向 Bot 发送一条消息以建立新会话")
	}
	if (content.Text == "") == (content.AttachmentID == "") {
		return receipt, &channel.SendError{Reason: "Weixin delivery must contain exactly one text or file", Permanent: true}
	}
	clientID, err := randomID()
	if err != nil {
		return receipt, err
	}
	var item wireItemOutbound
	if content.AttachmentID != "" {
		item, err = c.uploadAttachment(ctx, credentials, content.AttachmentID)
		if err != nil {
			return receipt, err
		}
	} else {
		item = wireItemOutbound{Type: 1, TextItem: &textItem{Text: content.Text}}
	}
	current, err := c.loadCredentials()
	if err != nil || current != credentials {
		return receipt, c.waiting("微信账号凭证已变化，请使用当前账号重试投递")
	}
	requestBody := struct {
		Message  wireOutgoingMessage `json:"msg"`
		BaseInfo baseInfo            `json:"base_info"`
	}{
		Message: wireOutgoingMessage{
			ToUserID:     credentials.UserID,
			ClientID:     clientID,
			MessageType:  2,
			MessageState: 2,
			Context:      state.ContextToken,
			ItemList:     []wireItemOutbound{item},
		},
		BaseInfo: makeBaseInfo(),
	}
	data, err := postJSONAt(ctx, c.httpClient, c.apiBaseURL(credentials), "ilink/bot/sendmessage", credentials.Token, requestBody, apiTimeout, c.allowTestURL)
	if err != nil {
		return receipt, c.sendFailure(err, credentials.Token, true)
	}
	var response sendMessageResponse
	if err := parseJSON(data, &response); err != nil {
		return receipt, &channel.SendError{Reason: "Weixin send result could not be confirmed", Uncertain: true}
	}
	if response.apiError.failed() {
		if response.apiError.code() == -14 {
			c.markTokenInvalid(credentials.Token)
			return receipt, c.waiting("微信登录已失效，请运行 kagari weixin login 更新凭证")
		}
		return receipt, &channel.SendError{Reason: fmt.Sprintf("Weixin rejected the message (code %d)", response.apiError.code()), Permanent: true}
	}
	if !response.apiError.success() {
		return receipt, &channel.SendError{Reason: "Weixin send result could not be confirmed", Uncertain: true}
	}
	messageID := parseWireID(response.MessageID)
	if messageID == "" {
		messageID = ""
	}
	receipt.MessageID = messageID
	c.clearWaiting()
	return receipt, nil
}

func (c *Client) sendFailure(err error, token string, messageMayBeSent bool) *channel.SendError {
	var failure *httpFailure
	if errors.As(err, &failure) {
		switch {
		case failure.status == http.StatusUnauthorized || failure.status == http.StatusForbidden:
			c.markTokenInvalid(token)
			return c.waiting("微信登录已失效，请运行 kagari weixin login 更新凭证")
		case failure.status >= 400 && failure.status < 500:
			return &channel.SendError{Reason: fmt.Sprintf("Weixin rejected the request (HTTP %d)", failure.status), Permanent: true}
		case messageMayBeSent:
			return &channel.SendError{Reason: "Weixin send result is uncertain", Uncertain: true}
		default:
			return &channel.SendError{Reason: "Weixin upload request failed"}
		}
	}
	if messageMayBeSent {
		return &channel.SendError{Reason: "Weixin send result is uncertain", Uncertain: true}
	}
	return &channel.SendError{Reason: "Weixin file upload failed"}
}

func (c *Client) waiting(reason string) *channel.SendError {
	c.setWaitingReason(reason)
	return &channel.SendError{Reason: reason, Waiting: true}
}

func (c *Client) setWaitingReason(reason string) {
	c.mu.Lock()
	changed := c.waitingReason != reason
	c.waitingReason = reason
	c.mu.Unlock()
	if changed {
		c.log.Info("weixin.waiting", zap.String("reason", reason))
	}
}

func (c *Client) clearWaiting() {
	c.mu.Lock()
	wasWaiting := c.waitingReason != ""
	c.waitingReason = ""
	c.mu.Unlock()
	if wasWaiting {
		c.log.Info("weixin.waiting.resolved")
	}
}

func (c *Client) tokenInvalid(token string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return token != "" && c.invalidToken == token
}

func (c *Client) markTokenInvalid(token string) {
	c.mu.Lock()
	c.invalidToken = token
	c.mu.Unlock()
	c.setWaitingReason("微信登录已失效，请运行 kagari weixin login 更新凭证")
}

func (c *Client) waitForCredentialChange(ctx context.Context, old Credentials) error {
	for {
		if err := sleepContext(ctx, time.Second); err != nil {
			return err
		}
		current, err := c.loadCredentials()
		if err == nil && current != old {
			c.mu.Lock()
			c.invalidToken = ""
			c.mu.Unlock()
			return nil
		}
	}
}

func (c *Client) loadState() (privateState, error) {
	f, err := os.Open(c.credentialPath + ".state")
	if err != nil {
		return privateState{}, err
	}
	defer f.Close()
	var state privateState
	if err := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&state); err != nil {
		return privateState{}, fmt.Errorf("read private Weixin session state: %w", err)
	}
	return state, nil
}

func (c *Client) saveStateFor(expected Credentials, state privateState) (bool, error) {
	stateWriteMu.Lock()
	defer stateWriteMu.Unlock()
	current, err := LoadCredentials(c.credentialPath)
	if err != nil || current != expected {
		return false, nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	if err := atomicPrivateWrite(c.credentialPath+".state", data); err != nil {
		return false, err
	}
	return true, nil
}

func stateMatches(state privateState, credentials Credentials) bool {
	return state.AccountID == credentials.AccountID && state.UserID == credentials.UserID && state.TokenDigest == tokenDigest(credentials.Token) && state.ContextToken != ""
}

func tokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func contentMIME(name string) string {
	if mimeType := mime.TypeByExtension(filepath.Ext(name)); mimeType != "" {
		return mimeType
	}
	return "application/octet-stream"
}
