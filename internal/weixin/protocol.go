package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"kagari/internal/channel"
)

const (
	apiVersion            = "2.4.8"
	apiClientVersion      = "132104" // 0x00020408
	maxAPIResponseBytes   = 8 << 20
	apiTimeout            = 15 * time.Second
	longPollTimeout       = 40 * time.Second
	qrPollTimeout         = 35 * time.Second
	defaultCDNMaxWireSize = channel.MaxFileBytes + 16
)

type httpFailure struct {
	status int
	cause  error
}

func (e *httpFailure) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("Weixin HTTP request failed with status %d", e.status)
	}
	if errors.Is(e.cause, context.Canceled) {
		return "Weixin request canceled"
	}
	if errors.Is(e.cause, context.DeadlineExceeded) {
		return "Weixin request timed out"
	}
	return "Weixin network request failed"
}

func (e *httpFailure) Unwrap() error { return e.cause }

type apiError struct {
	Ret     *int   `json:"ret"`
	ErrCode *int   `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func (e apiError) code() int {
	if e.Ret != nil && *e.Ret != 0 {
		return *e.Ret
	}
	if e.ErrCode != nil && *e.ErrCode != 0 {
		return *e.ErrCode
	}
	return 0
}

func (e apiError) failed() bool { return e.code() != 0 }

func (e apiError) success() bool {
	return e.Ret != nil && *e.Ret == 0 && (e.ErrCode == nil || *e.ErrCode == 0)
}

type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
	BotAgent       string `json:"bot_agent"`
}

func makeBaseInfo() baseInfo {
	return baseInfo{ChannelVersion: apiVersion, BotAgent: "Kagari/1.0"}
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: longPollTimeout + 5*time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			prior := via[len(via)-1]
			if prior.Header.Get("Authorization") != "" && req.URL.Host != prior.URL.Host {
				return http.ErrUseLastResponse
			}
			if req.URL.Scheme != "https" || (!isOfficialAPIHost(req.URL.Hostname()) && !isOfficialCDNHost(req.URL.Hostname())) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

func isOfficialCDNHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return strings.HasSuffix(host, ".cdn.weixin.qq.com")
}

func checkServiceURL(raw string, allowTestURL bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid service URL")
	}
	if allowTestURL && u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		return nil
	}
	if u.Scheme != "https" || (!isOfficialAPIHost(u.Hostname()) && !isOfficialCDNHost(u.Hostname())) {
		return errors.New("service URL must use an official Tencent HTTPS host")
	}
	return nil
}

func apiURL(baseURL, endpoint string) (string, error) {
	return apiURLAt(baseURL, endpoint, false)
}

func apiURLAt(baseURL, endpoint string, allowTestURL bool) (string, error) {
	if err := validateAPIBaseURL(baseURL, allowTestURL); err != nil {
		return "", err
	}
	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(endpoint, "/"), nil
}

func makeUIN() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate Weixin request ID: %w", err)
	}
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(raw[:])), 10))), nil
}

func apiHeaders(token string, withAuth bool) (http.Header, error) {
	uin, err := makeUIN()
	if err != nil {
		return nil, err
	}
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("AuthorizationType", "ilink_bot_token")
	h.Set("X-WECHAT-UIN", uin)
	h.Set("iLink-App-Id", "bot")
	h.Set("iLink-App-ClientVersion", apiClientVersion)
	if withAuth && token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h, nil
}

func qrStatusHeaders() http.Header {
	h := make(http.Header)
	h.Set("iLink-App-Id", "bot")
	h.Set("iLink-App-ClientVersion", apiClientVersion)
	return h
}

func request(ctx context.Context, client *http.Client, method, rawURL string, headers http.Header, body []byte, timeout time.Duration) ([]byte, http.Header, error) {
	return requestLimit(ctx, client, method, rawURL, headers, body, timeout, maxAPIResponseBytes)
}

func requestLimit(ctx context.Context, client *http.Client, method, rawURL string, headers http.Header, body []byte, timeout time.Duration, maxBytes int64) ([]byte, http.Header, error) {
	data, responseHeaders, _, err := requestWithStatus(ctx, client, method, rawURL, headers, body, timeout, maxBytes)
	return data, responseHeaders, err
}

func requestWithStatus(ctx context.Context, client *http.Client, method, rawURL string, headers http.Header, body []byte, timeout time.Duration, maxBytes int64) ([]byte, http.Header, int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("build Weixin request: %w", err)
	}
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, &httpFailure{cause: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, nil, resp.StatusCode, &httpFailure{status: resp.StatusCode, cause: err}
	}
	if int64(len(data)) > maxBytes {
		return nil, nil, resp.StatusCode, errors.New("Weixin response exceeds the size limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header.Clone(), resp.StatusCode, &httpFailure{status: resp.StatusCode}
	}
	return data, resp.Header.Clone(), resp.StatusCode, nil
}

func postJSON(ctx context.Context, client *http.Client, baseURL, endpoint, token string, body any, timeout time.Duration) ([]byte, error) {
	return postJSONAt(ctx, client, baseURL, endpoint, token, body, timeout, false)
}

func postJSONAt(ctx context.Context, client *http.Client, baseURL, endpoint, token string, body any, timeout time.Duration, allowTestURL bool) ([]byte, error) {
	url, err := apiURLAt(baseURL, endpoint, allowTestURL)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	headers, err := apiHeaders(token, true)
	if err != nil {
		return nil, err
	}
	result, _, err := request(ctx, client, http.MethodPost, url, headers, data, timeout)
	return result, err
}

func parseJSON[T any](raw []byte, target *T) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("decode Weixin response: %w", err)
	}
	return nil
}

func parseWireID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func validWireID(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value) != ""
	}
	value = strings.TrimSpace(string(raw))
	if value == "" || strings.Trim(value, "0") == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type wireMessage struct {
	MessageID   json.RawMessage `json:"message_id"`
	FromUserID  string          `json:"from_user_id"`
	ToUserID    string          `json:"to_user_id"`
	GroupID     string          `json:"group_id"`
	MessageType int             `json:"message_type"`
	ItemList    []wireItem      `json:"item_list"`
	Context     string          `json:"context_token"`
}

type wireRefMessage struct {
	SvrID json.RawMessage `json:"svr_id"`
}

type wireItem struct {
	Type      int             `json:"type"`
	TextItem  *textItem       `json:"text_item"`
	FileItem  *fileItem       `json:"file_item"`
	ImageItem *mediaItem      `json:"image_item"`
	VoiceItem *mediaItem      `json:"voice_item"`
	VideoItem *mediaItem      `json:"video_item"`
	RefMsg    *wireRefMessage `json:"ref_msg"`
}

type textItem struct {
	Text string `json:"text"`
}

type fileItem struct {
	FileName string    `json:"file_name"`
	MD5      string    `json:"md5"`
	Length   string    `json:"len"`
	Media    *cdnMedia `json:"media"`
}

type mediaItem struct {
	Media *cdnMedia `json:"media"`
}

type cdnMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`
	FullURL           string `json:"full_url"`
}

type getUpdatesResponse struct {
	apiError
	Messages  []wireMessage `json:"msgs"`
	Cursor    string        `json:"get_updates_buf"`
	TimeoutMS int           `json:"longpolling_timeout_ms"`
}

type sendMessageResponse struct {
	apiError
	MessageID json.RawMessage `json:"message_id"`
}

type uploadURLResponse struct {
	apiError
	UploadParam   string `json:"upload_param"`
	UploadFullURL string `json:"upload_full_url"`
}

func makeQRURL(baseURL, qrcode, verifyCode string, allowTestURL bool) (string, error) {
	if err := validateAPIBaseURL(baseURL, allowTestURL); err != nil {
		return "", err
	}
	u, _ := url.Parse(strings.TrimRight(baseURL, "/") + "/ilink/bot/get_qrcode_status")
	query := u.Query()
	query.Set("qrcode", qrcode)
	if verifyCode != "" {
		query.Set("verify_code", verifyCode)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func buildCDNURL(baseURL, direction, encryptedParam, fileKey string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	query := url.Values{}
	query.Set("encrypted_query_param", encryptedParam)
	if direction == "upload" {
		query.Set("filekey", fileKey)
	}
	return baseURL + "/" + direction + "?" + query.Encode()
}
