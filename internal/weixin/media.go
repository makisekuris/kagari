package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"kagari/internal/channel"
)

type wireItemOutbound struct {
	Type     int               `json:"type"`
	TextItem *textItem         `json:"text_item,omitempty"`
	FileItem *outboundFileItem `json:"file_item,omitempty"`
}

type wireOutgoingMessage struct {
	FromUserID   string             `json:"from_user_id"`
	ToUserID     string             `json:"to_user_id"`
	ClientID     string             `json:"client_id"`
	MessageType  int                `json:"message_type"`
	MessageState int                `json:"message_state"`
	Context      string             `json:"context_token"`
	ItemList     []wireItemOutbound `json:"item_list"`
}

type outboundFileItem struct {
	FileName string         `json:"file_name"`
	MD5      string         `json:"md5"`
	Length   string         `json:"len"`
	Media    outboundCDNRef `json:"media"`
}

type outboundCDNRef struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`
	EncryptType       int    `json:"encrypt_type"`
}

func (c *Client) downloadAttachment(ctx context.Context, credentials Credentials, file *fileItem) (channel.Attachment, error) {
	if file == nil {
		return channel.Attachment{}, errors.New("微信普通文件缺少文件信息")
	}
	name := file.FileName
	if name == "" {
		name = "file.bin"
	}
	var declaredSize int64 = -1
	if file.Length != "" {
		parsed, err := strconv.ParseInt(file.Length, 10, 64)
		if err != nil || parsed < 0 {
			return channel.Attachment{}, errors.New("微信附件声明的大小无效")
		}
		if parsed > channel.MaxFileBytes {
			return channel.Attachment{}, errors.New("微信附件超过 20 MB 限制")
		}
		declaredSize = parsed
	}
	if file.Media == nil || file.Media.AESKey == "" {
		return channel.Attachment{}, errors.New("微信附件缺少加密密钥")
	}
	key, err := decodeAESKey(file.Media.AESKey)
	if err != nil {
		return channel.Attachment{}, errors.New("微信附件加密密钥无效")
	}
	cdnURL := file.Media.FullURL
	if cdnURL == "" {
		if file.Media.EncryptQueryParam == "" {
			return channel.Attachment{}, errors.New("微信附件缺少下载地址")
		}
		cdnURL = buildCDNURL(c.cdnBaseURL(), "download", file.Media.EncryptQueryParam, "")
	}
	if err := checkServiceURL(cdnURL, c.allowTestURL); err != nil {
		return channel.Attachment{}, errors.New("微信附件下载地址不是官方 HTTPS 地址")
	}
	ciphertext, _, err := requestLimit(ctx, c.httpClient, http.MethodGet, cdnURL, nil, nil, longPollTimeout, defaultCDNMaxWireSize)
	if err != nil {
		return channel.Attachment{}, attachmentHTTPError(err)
	}
	plaintext, err := decryptAES128ECB(ciphertext, key)
	if err != nil {
		return channel.Attachment{}, errors.New("微信附件解密失败")
	}
	if int64(len(plaintext)) > channel.MaxFileBytes {
		return channel.Attachment{}, errors.New("微信附件超过 20 MB 限制")
	}
	if declaredSize >= 0 && int64(len(plaintext)) != declaredSize {
		return channel.Attachment{}, errors.New("微信附件实际大小与声明不符")
	}
	if file.MD5 != "" {
		checksum := md5.Sum(plaintext)
		if !strings.EqualFold(strings.TrimSpace(file.MD5), hex.EncodeToString(checksum[:])) {
			return channel.Attachment{}, errors.New("微信附件 MD5 校验失败")
		}
	}
	saved, err := c.files.SaveAttachment(ctx, name, contentMIME(name), bytes.NewReader(plaintext))
	if err != nil {
		return channel.Attachment{}, errors.New("微信附件保存失败")
	}
	if saved.ID == "" {
		return channel.Attachment{}, errors.New("微信附件存储未返回可用附件")
	}
	if saved.Name == "" {
		saved.Name = name
	}
	if saved.MIME == "" {
		saved.MIME = contentMIME(name)
	}
	if saved.Size == 0 && len(plaintext) != 0 {
		saved.Size = int64(len(plaintext))
	}
	return saved, nil
}

func (c *Client) uploadAttachment(ctx context.Context, credentials Credentials, attachmentID string) (wireItemOutbound, error) {
	attachment, err := c.files.Attachment(ctx, attachmentID)
	if err != nil {
		return wireItemOutbound{}, &channel.SendError{Reason: "Weixin attachment is unavailable", Permanent: true}
	}
	if attachment.Deleted || attachment.Error != "" || attachment.ID == "" {
		return wireItemOutbound{}, &channel.SendError{Reason: "Weixin attachment is unavailable", Permanent: true}
	}
	if attachment.Size < 0 || attachment.Size > channel.MaxFileBytes {
		return wireItemOutbound{}, &channel.SendError{Reason: "Weixin attachment exceeds the 20 MB limit", Permanent: true}
	}
	reader, err := c.files.OpenAttachment(ctx, attachmentID)
	if err != nil {
		return wireItemOutbound{}, &channel.SendError{Reason: "Weixin attachment could not be opened", Permanent: true}
	}
	defer reader.Close()
	plaintext, err := io.ReadAll(io.LimitReader(reader, channel.MaxFileBytes+1))
	if err != nil {
		return wireItemOutbound{}, &channel.SendError{Reason: "Weixin attachment could not be read"}
	}
	if int64(len(plaintext)) > channel.MaxFileBytes || (attachment.Size != 0 && int64(len(plaintext)) != attachment.Size) {
		return wireItemOutbound{}, &channel.SendError{Reason: "Weixin attachment size is invalid", Permanent: true}
	}

	fileKeyBytes, err := randomBytes(16)
	if err != nil {
		return wireItemOutbound{}, err
	}
	aesKey, err := randomBytes(16)
	if err != nil {
		return wireItemOutbound{}, err
	}
	fileKey := hex.EncodeToString(fileKeyBytes)
	aesKeyHex := hex.EncodeToString(aesKey)
	checksum := md5.Sum(plaintext)
	ciphertext := encryptAES128ECB(plaintext, aesKey)
	uploadRequest := struct {
		FileKey     string   `json:"filekey"`
		MediaType   int      `json:"media_type"`
		ToUserID    string   `json:"to_user_id"`
		RawSize     int64    `json:"rawsize"`
		RawFileMD5  string   `json:"rawfilemd5"`
		FileSize    int64    `json:"filesize"`
		NoNeedThumb bool     `json:"no_need_thumb"`
		AESKey      string   `json:"aeskey"`
		BaseInfo    baseInfo `json:"base_info"`
	}{fileKey, 3, credentials.UserID, int64(len(plaintext)), hex.EncodeToString(checksum[:]), int64(len(ciphertext)), true, aesKeyHex, makeBaseInfo()}
	data, err := postJSONAt(ctx, c.httpClient, c.apiBaseURL(credentials), "ilink/bot/getuploadurl", credentials.Token, uploadRequest, apiTimeout, c.allowTestURL)
	if err != nil {
		return wireItemOutbound{}, c.sendFailure(err, credentials.Token, false)
	}
	var uploadResponse uploadURLResponse
	if err := parseJSON(data, &uploadResponse); err != nil {
		return wireItemOutbound{}, &channel.SendError{Reason: "Weixin upload URL response was invalid", Permanent: true}
	}
	if uploadResponse.apiError.failed() {
		if uploadResponse.apiError.code() == -14 {
			c.markTokenInvalid(credentials.Token)
			return wireItemOutbound{}, c.waiting("微信登录已失效，请运行 kagari weixin login 更新凭证")
		}
		return wireItemOutbound{}, &channel.SendError{Reason: fmt.Sprintf("Weixin rejected the file upload (code %d)", uploadResponse.apiError.code()), Permanent: true}
	}
	uploadURL := strings.TrimSpace(uploadResponse.UploadFullURL)
	if uploadURL == "" && uploadResponse.UploadParam != "" {
		uploadURL = buildCDNURL(c.cdnBaseURL(), "upload", uploadResponse.UploadParam, fileKey)
	}
	if uploadURL == "" || checkServiceURL(uploadURL, c.allowTestURL) != nil {
		return wireItemOutbound{}, &channel.SendError{Reason: "Weixin returned an invalid file upload URL", Permanent: true}
	}
	_, headers, status, err := requestWithStatus(ctx, c.httpClient, http.MethodPost, uploadURL, http.Header{"Content-Type": []string{"application/octet-stream"}}, ciphertext, longPollTimeout, 1<<20)
	if err != nil {
		return wireItemOutbound{}, c.sendFailure(err, credentials.Token, false)
	}
	if status != http.StatusOK {
		return wireItemOutbound{}, &channel.SendError{Reason: fmt.Sprintf("Weixin CDN upload failed (HTTP %d)", status), Permanent: status >= 400 && status < 500}
	}
	downloadParam := strings.TrimSpace(headers.Get("x-encrypted-param"))
	if downloadParam == "" {
		return wireItemOutbound{}, &channel.SendError{Reason: "Weixin CDN upload returned no encrypted reference"}
	}
	name := attachment.Name
	if name == "" {
		name = "file.bin"
	}
	return wireItemOutbound{Type: 4, FileItem: &outboundFileItem{
		FileName: name,
		MD5:      hex.EncodeToString(checksum[:]),
		Length:   strconv.FormatInt(int64(len(plaintext)), 10),
		Media: outboundCDNRef{
			EncryptQueryParam: downloadParam,
			AESKey:            base64.StdEncoding.EncodeToString([]byte(aesKeyHex)),
			EncryptType:       1,
		},
	}}, nil
}

func randomID() (string, error) {
	b, err := randomBytes(16)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomBytes(size int) ([]byte, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate Weixin media key: %w", err)
	}
	return b, nil
}

func decodeAESKey(encoded string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	}
	if err != nil {
		return nil, err
	}
	if len(decoded) == aes.BlockSize {
		return decoded, nil
	}
	if len(decoded) == 2*aes.BlockSize {
		key := make([]byte, aes.BlockSize)
		if _, err := hex.Decode(key, decoded); err == nil {
			return key, nil
		}
	}
	return nil, errors.New("invalid AES key length")
}

func encryptAES128ECB(plaintext, key []byte) []byte {
	block, _ := aes.NewCipher(key)
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, len(plaintext)+padding)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	for offset := 0; offset < len(padded); offset += aes.BlockSize {
		block.Encrypt(padded[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
	}
	return padded
}

func decryptAES128ECB(ciphertext, key []byte) ([]byte, error) {
	if len(key) != aes.BlockSize || len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("invalid AES ciphertext")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plaintext := append([]byte(nil), ciphertext...)
	for offset := 0; offset < len(plaintext); offset += aes.BlockSize {
		block.Decrypt(plaintext[offset:offset+aes.BlockSize], plaintext[offset:offset+aes.BlockSize])
	}
	padding := int(plaintext[len(plaintext)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(plaintext) {
		return nil, errors.New("invalid PKCS#7 padding")
	}
	for _, b := range plaintext[len(plaintext)-padding:] {
		if int(b) != padding {
			return nil, errors.New("invalid PKCS#7 padding")
		}
	}
	return plaintext[:len(plaintext)-padding], nil
}

func attachmentHTTPError(err error) error {
	var failure *httpFailure
	if errors.As(err, &failure) && failure.status != 0 {
		return fmt.Errorf("微信附件下载失败（HTTP %d）", failure.status)
	}
	return errors.New("微信附件下载失败")
}
