package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"kagari/internal/channel"
)

func (c *PersonalClient) receiveDocument(ctx context.Context, document *models.Document) channel.Attachment {
	name := document.FileName
	if name == "" {
		name = "document"
	}
	mimeType := document.MimeType
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(name))
	}
	return c.receiveFile(ctx, document.FileID, document.FileSize, name, mimeType)
}

// A native photo has no supplied name or media type. Fill them from the download.
func (c *PersonalClient) receiveFile(ctx context.Context, fileID string, fileSize int64, name, mimeType string) channel.Attachment {
	if ctx == nil {
		ctx = context.Background()
	}
	attachment := channel.Attachment{Name: name, MIME: mimeType}
	fail := func(reason string) channel.Attachment {
		if attachment.Name == "" {
			attachment.Name = "photo"
		}
		attachment.Error = reason
		return attachment
	}
	if fileSize < 0 || fileSize > channel.MaxFileBytes {
		return fail("文件超过 20,000,000 字节上限")
	}
	if fileID == "" {
		return fail("Telegram 文件标识缺失")
	}
	file, err := c.bot.GetFile(ctx, &telegrambot.GetFileParams{FileID: fileID})
	if err != nil {
		if errors.Is(err, telegrambot.ErrorUnauthorized) {
			c.markAuthInvalid()
			return fail("Telegram 授权失效")
		}
		return fail("无法取得 Telegram 文件信息")
	}
	if file == nil || file.FileSize < 0 || file.FileSize > channel.MaxFileBytes {
		return fail("文件超过 20,000,000 字节上限")
	}
	downloadURL, err := c.fileURL(file.FilePath)
	if err != nil {
		return fail("Telegram 文件路径无效")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fail("无法创建 Telegram 文件下载请求")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fail("Telegram 文件下载失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.markAuthInvalid()
		return fail("Telegram 授权失效")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail("Telegram 文件下载失败")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, channel.MaxFileBytes+1))
	if err != nil {
		return fail("Telegram 文件内容读取失败")
	}
	if int64(len(data)) > channel.MaxFileBytes {
		return fail("文件超过 20,000,000 字节上限")
	}
	if fileSize > 0 && int64(len(data)) != fileSize || file.FileSize > 0 && int64(len(data)) != file.FileSize {
		return fail("下载文件大小与 Telegram 元数据不符")
	}
	if name == "" {
		attachment.Name = filepath.Base(file.FilePath)
		attachment.MIME = http.DetectContentType(data)
	}
	attachment.Size = int64(len(data))
	saved, err := c.files.SaveAttachment(ctx, attachment.Name, attachment.MIME, bytes.NewReader(data))
	if err != nil || saved.ID == "" {
		return fail("附件保存失败")
	}
	attachment.ID = saved.ID
	return attachment
}

func (c *PersonalClient) fileURL(filePath string) (string, error) {
	if filePath == "" || strings.Contains(filePath, "\\") {
		return "", errors.New("invalid Telegram file path")
	}
	parts := strings.Split(filePath, "/")
	var escaped []string
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("invalid Telegram file path")
		}
		escaped = append(escaped, url.PathEscape(part))
	}
	return c.baseURL + "/file/bot" + c.token + "/" + strings.Join(escaped, "/"), nil
}

func (c *PersonalClient) sendAttachment(ctx context.Context, attachmentID string) (string, error) {
	attachment, err := c.files.Attachment(ctx, attachmentID)
	if err != nil || attachment.ID == "" || attachment.Error != "" || attachment.Deleted {
		return "", &channel.SendError{Reason: "Telegram attachment is unavailable", Permanent: true}
	}
	if attachment.Size < 0 || attachment.Size > channel.MaxFileBytes {
		return "", &channel.SendError{Reason: "Telegram document exceeds 20,000,000 byte limit", Permanent: true}
	}
	file, err := c.files.OpenAttachment(ctx, attachmentID)
	if err != nil {
		return "", &channel.SendError{Reason: "Telegram attachment is unavailable", Permanent: true}
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, channel.MaxFileBytes+1))
	if err != nil {
		return "", &channel.SendError{Reason: "Telegram attachment could not be read", Permanent: true}
	}
	if int64(len(data)) > channel.MaxFileBytes {
		return "", &channel.SendError{Reason: "Telegram document exceeds 20,000,000 byte limit", Permanent: true}
	}
	if int64(len(data)) != attachment.Size {
		return "", &channel.SendError{Reason: "Telegram attachment size does not match stored metadata", Permanent: true}
	}
	method, field := "sendDocument", "document"
	mimeType := attachment.MIME
	if photoMIME := photoUploadMIME(data); photoMIME != "" {
		method, field = "sendPhoto", "photo"
		mimeType = photoMIME
	}
	name := attachment.Name
	if name == "" {
		name = field
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(mimeType); err != nil {
		return "", &channel.SendError{Reason: "Telegram attachment media type is invalid", Permanent: true}
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", strconv.FormatInt(c.ownerID, 10)); err != nil {
		return "", &channel.SendError{Reason: "Telegram attachment could not be prepared", Permanent: true}
	}
	header := make(textproto.MIMEHeader)
	disposition := mime.FormatMediaType("form-data", map[string]string{"name": field, "filename": name})
	if disposition == "" {
		return "", &channel.SendError{Reason: "Telegram attachment name is invalid", Permanent: true}
	}
	header.Set("Content-Disposition", disposition)
	header.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", &channel.SendError{Reason: "Telegram attachment could not be prepared", Permanent: true}
	}
	if _, err := part.Write(data); err != nil {
		return "", &channel.SendError{Reason: "Telegram attachment could not be prepared", Permanent: true}
	}
	if err := writer.Close(); err != nil {
		return "", &channel.SendError{Reason: "Telegram attachment could not be prepared", Permanent: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, &body)
	if err != nil {
		return "", &channel.SendError{Reason: "Telegram attachment could not be prepared", Permanent: true}
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return "", &channel.SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		c.markAuthInvalid()
		return "", &channel.SendError{Reason: "Telegram authorization is invalid", Permanent: true, Waiting: true}
	}
	response, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil || len(response) > maxResponseSize {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return "", c.attachmentSendError(field, resp.StatusCode, 0)
		}
		return "", &channel.SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
	}
	var result struct {
		OK         bool            `json:"ok"`
		Result     json.RawMessage `json:"result"`
		ErrorCode  int             `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return "", c.attachmentSendError(field, resp.StatusCode, 0)
		}
		return "", &channel.SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.OK {
		code := result.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return "", c.attachmentSendError(field, code, result.Parameters.RetryAfter)
	}
	var message struct {
		ID json.RawMessage `json:"message_id"`
	}
	if len(result.Result) > 0 && string(result.Result) != "null" {
		if err := json.Unmarshal(result.Result, &message); err != nil {
			return "", &channel.SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
		}
	}
	messageID := telegramID(message.ID)
	if messageID == "" {
		return "", &channel.SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
	}
	return messageID, nil
}

func (c *PersonalClient) attachmentSendError(field string, code, retryAfterSeconds int) *channel.SendError {
	switch code {
	case http.StatusUnauthorized:
		c.markAuthInvalid()
		return &channel.SendError{Reason: "Telegram authorization is invalid", Permanent: true, Waiting: true}
	case http.StatusTooManyRequests:
		return &channel.SendError{Reason: "Telegram rate limit", RetryAfter: retryAfter(retryAfterSeconds)}
	case http.StatusForbidden:
		return &channel.SendError{Reason: "Telegram does not allow this " + field, Permanent: true}
	default:
		if code >= 400 && code < 500 {
			return &channel.SendError{Reason: "Telegram rejected the " + field, Permanent: true}
		}
		return &channel.SendError{Reason: "Telegram send outcome is unknown", Uncertain: true}
	}
}

func telegramID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}
