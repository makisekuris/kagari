package telegram

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"
	"kagari/internal/channel"
	"kagari/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func apiResponse(req *http.Request, status int, payload string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
		Request:    req,
	}
}

type channelInboxFixture struct {
	mu        sync.Mutex
	cursors   map[string]string
	inputs    []channel.Input
	accepts   [][]channel.Input
	seen      map[string]bool
	nextID    int64
	acceptErr error
}

func (i *channelInboxFixture) Cursor(_ context.Context, ch, account string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cursors == nil {
		i.cursors = make(map[string]string)
	}
	return i.cursors[ch+"/"+account], nil
}

func (i *channelInboxFixture) AcceptBatch(_ context.Context, ch, account, cursor string, inputs []channel.Input) ([]int64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.acceptErr != nil {
		return nil, i.acceptErr
	}
	if i.cursors == nil {
		i.cursors = make(map[string]string)
	}
	i.cursors[ch+"/"+account] = cursor
	cloned := append([]channel.Input(nil), inputs...)
	i.accepts = append(i.accepts, cloned)
	i.inputs = append(i.inputs, cloned...)
	ids := make([]int64, len(inputs))
	for n := range ids {
		if i.seen == nil {
			i.seen = make(map[string]bool)
		}
		key := inputs[n].Channel + "/" + inputs[n].Account + "/" + inputs[n].Sender + "/" + inputs[n].MessageID
		if i.seen[key] {
			continue
		}
		i.seen[key] = true
		i.nextID++
		ids[n] = i.nextID
	}
	return ids, nil
}

type channelFilesFixture struct {
	mu      sync.Mutex
	items   map[string]channel.Attachment
	content map[string][]byte
	nextID  int
}

func newChannelFilesFixture() *channelFilesFixture {
	return &channelFilesFixture{items: map[string]channel.Attachment{}, content: map[string][]byte{}}
}

func (f *channelFilesFixture) SaveAttachment(_ context.Context, name, mimeType string, input io.Reader) (channel.Attachment, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return channel.Attachment{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.Itoa(f.nextID)
	attachment := channel.Attachment{ID: id, Name: name, MIME: mimeType, Size: int64(len(data))}
	f.items[id], f.content[id] = attachment, data
	return attachment, nil
}

func (f *channelFilesFixture) Attachment(_ context.Context, id string) (channel.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attachment, ok := f.items[id]
	if !ok {
		return channel.Attachment{}, errors.New("missing attachment")
	}
	return attachment, nil
}

func (f *channelFilesFixture) OpenAttachment(_ context.Context, id string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.content[id]
	if !ok {
		return nil, errors.New("missing attachment")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *channelFilesFixture) put(name, mimeType string, data []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.Itoa(f.nextID)
	f.items[id] = channel.Attachment{ID: id, Name: name, MIME: mimeType, Size: int64(len(data))}
	f.content[id] = append([]byte(nil), data...)
	return id
}

func personalConfig() config.Telegram {
	return config.Telegram{Token: "123:fixture_secret", AllowedUserIDs: []int64{7}}
}

func personalClient(t *testing.T, inbox channel.Inbox, files channel.Files, transport http.RoundTripper) *PersonalClient {
	t.Helper()
	client, err := newPersonalClient(personalConfig(), inbox, files, zap.NewNop(), "https://telegram.test", transport)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestNewPersonalRequiresSingleOwnerAndValidDependencies(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Telegram
	}{
		{name: "no owner", cfg: config.Telegram{Token: "123:fixture_secret"}},
		{name: "multiple owners", cfg: config.Telegram{Token: "123:fixture_secret", AllowedUserIDs: []int64{7, 8}}},
		{name: "nonpositive owner", cfg: config.Telegram{Token: "123:fixture_secret", AllowedUserIDs: []int64{0}}},
		{name: "invalid token", cfg: config.Telegram{Token: "123:fixture.secret", AllowedUserIDs: []int64{7}}},
		{name: "no bot account", cfg: config.Telegram{Token: "not-a-bot:fixture_secret", AllowedUserIDs: []int64{7}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newPersonalClient(tc.cfg, &channelInboxFixture{}, newChannelFilesFixture(), zap.NewNop(), "https://telegram.test", nil); err == nil {
				t.Fatal("newPersonalClient() succeeded with invalid personal configuration")
			}
		})
	}
	if _, err := newPersonalClient(personalConfig(), nil, newChannelFilesFixture(), zap.NewNop(), "https://telegram.test", nil); err == nil {
		t.Fatal("newPersonalClient() succeeded without an inbox")
	}
	if _, err := newPersonalClient(personalConfig(), &channelInboxFixture{}, nil, zap.NewNop(), "https://telegram.test", nil); err == nil {
		t.Fatal("newPersonalClient() succeeded without file storage")
	}
	client := personalClient(t, &channelInboxFixture{}, newChannelFilesFixture(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("constructor made a network request")
		return nil, errors.New("unexpected request")
	}))
	if client.account != "123" {
		t.Fatalf("account = %q, want bot ID 123", client.account)
	}
}

func TestPersonalPollFiltersOwnerAndAdvancesCursorForEveryUpdate(t *testing.T) {
	updates := `{"ok":true,"result":[` +
		`{"update_id":1,"message":{"message_id":11,"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"owner"}},` +
		`{"update_id":2,"message":{"message_id":12,"from":{"id":7},"chat":{"id":-100,"type":"group"},"text":"group"}},` +
		`{"update_id":3,"message":{"message_id":13,"from":{"id":8},"chat":{"id":8,"type":"private"},"text":"other user"}},` +
		`{"update_id":4,"message":{"message_id":14,"from":{"id":7},"chat":{"id":9,"type":"private"},"text":"wrong private chat"}},` +
		`{"update_id":5,"edited_message":{"message_id":15}}]}`
	var mu sync.Mutex
	calls := 0
	nextPoll := make(chan struct{})
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/getUpdates") {
			t.Errorf("unexpected Telegram request: %s", req.URL.Path)
			return nil, errors.New("unexpected request")
		}
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			if got := req.URL.Query().Get("offset"); got != "0" {
				t.Errorf("initial offset = %q, want 0", got)
			}
			return apiResponse(req, http.StatusOK, updates), nil
		}
		if got := req.URL.Query().Get("offset"); got != "6" {
			t.Errorf("next offset = %q, want 6", got)
		}
		close(nextPoll)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	inbox := &channelInboxFixture{}
	client := personalClient(t, inbox, newChannelFilesFixture(), transport)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Poll(ctx) }()
	select {
	case <-nextPoll:
	case <-time.After(3 * time.Second):
		t.Fatal("personal poller did not advance to the next batch")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	cursor, err := inbox.Cursor(context.Background(), Channel, "123")
	if err != nil || cursor != "6" {
		t.Fatalf("cursor = (%q, %v), want 6", cursor, err)
	}
	if len(inbox.inputs) != 1 {
		t.Fatalf("accepted inputs = %#v, want only owner private message", inbox.inputs)
	}
	input := inbox.inputs[0]
	if input.Channel != Channel || input.Account != "123" || input.Sender != "7" || input.MessageID != "11" || input.Text != "owner" {
		t.Fatalf("owner input = %#v", input)
	}
	if len(inbox.accepts) != 5 || len(inbox.accepts[1]) != 0 || len(inbox.accepts[4]) != 0 {
		t.Fatalf("accepted batches = %#v, want five cursor commits including filtered updates", inbox.accepts)
	}
}

func TestPersonalPollRejectsUpdateWithoutPositiveIDBeforeCursorChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{name: "missing id", payload: `{"ok":true,"result":[{"message":{"message_id":1}}]}`},
		{name: "zero id", payload: `{"ok":true,"result":[{"update_id":0,"message":{"message_id":1}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbox := &channelInboxFixture{}
			client := personalClient(t, inbox, newChannelFilesFixture(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return apiResponse(req, http.StatusOK, tc.payload), nil
			}))
			if err := client.Poll(context.Background()); err == nil {
				t.Fatal("Poll() succeeded with an invalid update ID")
			}
			if cursor, err := inbox.Cursor(context.Background(), Channel, "123"); err != nil || cursor != "" {
				t.Fatalf("cursor after invalid update = (%q, %v), want empty", cursor, err)
			}
		})
	}
}

func TestUTF16SubstringUsesTelegramEntityOffsets(t *testing.T) {
	text := "😀 https://example.com/a"
	link := "https://example.com/a"
	got, ok := utf16Substring(text, len(utf16.Encode([]rune("😀 "))), len(utf16.Encode([]rune(link))))
	if !ok || got != link {
		t.Fatalf("UTF-16 entity text = (%q, %v), want %q", got, ok, link)
	}
	if _, ok := utf16Substring("😀", 1, 1); ok {
		t.Fatal("UTF-16 entity offset split a surrogate pair")
	}
}

func TestAppendEntityURLsUsesVisibleTextBoundaries(t *testing.T) {
	prefix := "😀 请阅读 "
	visible := "https://www.rfc-editor.org/rfc/rfc6761，比较示例域名的用途和限制。"
	text := prefix + visible
	entities := []models.MessageEntity{{
		Type: models.MessageEntityTypeURL, Offset: len(utf16.Encode([]rune(prefix))),
		Length: len(utf16.Encode([]rune(visible))),
	}}
	if got := appendEntityURLs(text, entities); got != text {
		t.Fatalf("visible URL entity appended prose as a URL: %q", got)
	}
	hidden := "https://example.com/资料%EF%BC%8C说明"
	entities = append(entities, models.MessageEntity{Type: models.MessageEntityTypeTextLink, Offset: 0, Length: 2, URL: hidden})
	want := text + "\nhttps://example.com/%E8%B5%84%E6%96%99%EF%BC%8C%E8%AF%B4%E6%98%8E"
	if got := appendEntityURLs(text, entities); got != want {
		t.Fatalf("hidden link target changed: got %q, want %q", got, want)
	}
}

func TestPersonalInputPreservesCodeEntitiesAndURLOffsets(t *testing.T) {
	client := personalClient(t, &channelInboxFixture{}, newChannelFilesFixture(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected Telegram request: %s", req.URL)
		return nil, errors.New("unexpected request")
	}))

	prefix := "😀 inline "
	code := "**literal**"
	middle := " before pre "
	pre := "const marker = ```;"
	urlLead := " reference "
	url := "https://example.com/a"
	text := prefix + code + middle + pre + urlLead + url
	utf16Length := func(value string) int { return len(utf16.Encode([]rune(value))) }
	entities := []models.MessageEntity{
		{Type: models.MessageEntityTypeCode, Offset: utf16Length(prefix), Length: utf16Length(code)},
		{Type: models.MessageEntityTypePre, Offset: utf16Length(prefix + code + middle), Length: utf16Length(pre)},
		{Type: models.MessageEntityTypeURL, Offset: utf16Length(prefix + code + middle + pre + urlLead), Length: utf16Length(url)},
	}
	message := &models.Message{
		ID: 55, Text: text, Entities: entities,
		From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate},
	}

	input, err := client.inputForUpdate(context.Background(), &models.Update{Message: message})
	if err != nil {
		t.Fatal(err)
	}
	if input == nil {
		t.Fatal("inputForUpdate() returned nil input")
	}
	want := prefix + "\n\n```\n" + code + "\n```\n\n" + middle + "\n\n````\n" + pre + "\n````\n\n" + urlLead + url
	if input.Text != want {
		t.Fatalf("input text = %q, want %q", input.Text, want)
	}
	if strings.Count(input.Text, url) != 1 {
		t.Fatalf("URL occurrence count = %d, want 1: %q", strings.Count(input.Text, url), input.Text)
	}
}

func TestPreserveCodeEntitiesKeepsTrailingLineEnding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "LF", content: "line 1\n"},
		{name: "CRLF", content: "line 1\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "before " + tc.content + " after"
			prefix := "before "
			entity := models.MessageEntity{
				Type:   models.MessageEntityTypePre,
				Offset: len(utf16.Encode([]rune(prefix))), Length: len(utf16.Encode([]rune(tc.content))),
			}
			normalized := preserveCodeEntities(text, []models.MessageEntity{entity})
			literals := channel.MarkdownCodeLiterals(normalized)
			if len(literals) != 1 || literals[0] != tc.content {
				t.Fatalf("code literals = %#v, want %#v; normalized input: %q", literals, []string{tc.content}, normalized)
			}
		})
	}
}

func TestRetryAfterPreservesLongDelays(t *testing.T) {
	if got := retryAfter(3601); got != 3601*time.Second {
		t.Fatalf("retryAfter(3601) = %s, want %s", got, 3601*time.Second)
	}
	if got := retryAfter(-1); got != 0 {
		t.Fatalf("retryAfter(-1) = %s, want zero", got)
	}
}

func TestPersonalInputPreservesCaptionDocumentReplyAndForward(t *testing.T) {
	inbox := &channelInboxFixture{}
	files := newChannelFilesFixture()
	data := []byte("exact telegram document bytes\x00\xff")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/getFile"):
			return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"file_id":"doc-1","file_size":`+strconv.Itoa(len(data))+`,"file_path":"documents/incoming.bin"}}`), nil
		case req.URL.Path == "/file/bot123:fixture_secret/documents/incoming.bin":
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), Request: req}, nil
		default:
			t.Fatalf("unexpected Telegram request: %s", req.URL)
			return nil, errors.New("unexpected request")
		}
	})
	client := personalClient(t, inbox, files, transport)
	message := &models.Message{
		ID: 41, From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate},
		Caption: "please review", CaptionEntities: []models.MessageEntity{{Type: models.MessageEntityTypeTextLink, Offset: 0, Length: 6, URL: "https://example.com/doc"}},
		ReplyToMessage: &models.Message{ID: 40}, ForwardOrigin: &models.MessageOrigin{Type: models.MessageOriginTypeHiddenUser},
		Document: &models.Document{FileID: "doc-1", FileName: "source.bin", MimeType: "application/x-source", FileSize: int64(len(data))},
	}
	if err := client.acceptUpdate(context.Background(), &models.Update{ID: 1, Message: message}); err != nil {
		t.Fatal(err)
	}
	if len(inbox.inputs) != 1 {
		t.Fatalf("inputs = %#v, want one input", inbox.inputs)
	}
	input := inbox.inputs[0]
	if input.Text != "please review\nhttps://example.com/doc" || input.MessageID != "41" || input.ReplyToID != "40" || !input.Material {
		t.Fatalf("caption/document input = %#v", input)
	}
	if len(input.Attachments) != 1 || input.Attachments[0].ID == "" || input.Attachments[0].Name != "source.bin" || input.Attachments[0].MIME != "application/x-source" || input.Attachments[0].Size != int64(len(data)) || input.Attachments[0].Error != "" {
		t.Fatalf("document metadata = %#v", input.Attachments)
	}
	stored, err := files.OpenAttachment(context.Background(), input.Attachments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	got, err := io.ReadAll(stored)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("stored file = %v, %v; want exact bytes", got, err)
	}
}

func TestPersonalInputAcceptsDocumentWithoutText(t *testing.T) {
	data := []byte("file only")
	files := newChannelFilesFixture()
	client := personalClient(t, &channelInboxFixture{}, files, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/getFile"):
			return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"file_id":"f","file_path":"f.bin","file_size":9}}`), nil
		case strings.HasSuffix(req.URL.Path, "/f.bin"):
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), Request: req}, nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
			return nil, errors.New("unexpected request")
		}
	}))
	input, err := client.inputForUpdate(context.Background(), &models.Update{ID: 2, Message: &models.Message{
		ID: 20, From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate},
		Document: &models.Document{FileID: "f", FileName: "f.bin", FileSize: int64(len(data))},
	}})
	if err != nil || input == nil || input.Text != "" || len(input.Attachments) != 1 || input.Attachments[0].Error != "" {
		t.Fatalf("file-only input = (%#v, %v)", input, err)
	}
}

func TestPersonalInputRecordsDownloadFailureAndSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		document  *models.Document
		transport http.RoundTripper
		wantErr   string
	}{
		{name: "failed download", document: &models.Document{FileID: "f", FileName: "f.bin"}, transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/getFile") {
				return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"file_id":"f","file_path":"f.bin"}}`), nil
			}
			return apiResponse(req, http.StatusNotFound, `not found`), nil
		}), wantErr: "Telegram 文件下载失败"},
		{name: "declared too large", document: &models.Document{FileID: "f", FileName: "large.bin", FileSize: channel.MaxFileBytes + 1}, transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("oversized declaration made request: %s", req.URL)
			return nil, errors.New("unexpected request")
		}), wantErr: "文件超过 20,000,000 字节上限"},
		{name: "actual download too large", document: &models.Document{FileID: "f", FileName: "large.bin"}, transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/getFile") {
				return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"file_id":"f","file_path":"large.bin"}}`), nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{'x'}, int(channel.MaxFileBytes+1)))), Request: req}, nil
		}), wantErr: "文件超过 20,000,000 字节上限"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := newChannelFilesFixture()
			client := personalClient(t, &channelInboxFixture{}, files, tc.transport)
			attachment := client.receiveDocument(context.Background(), tc.document)
			if attachment.Error != tc.wantErr || attachment.ID != "" {
				t.Fatalf("failed attachment = %#v, want error %q and no stored ID", attachment, tc.wantErr)
			}
			if tc.name == "failed download" && attachment.Name != "f.bin" {
				t.Fatalf("failure lost attachment name: %#v", attachment)
			}
		})
	}
}

func TestPersonalFailedFileRemainsAnExplicitInput(t *testing.T) {
	client := personalClient(t, &channelInboxFixture{}, newChannelFilesFixture(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/getFile") {
			return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"file_id":"f","file_path":"f.bin"}}`), nil
		}
		return apiResponse(req, http.StatusNotFound, `{"ok":false}`), nil
	}))
	input, err := client.inputForUpdate(context.Background(), &models.Update{ID: 1, Message: &models.Message{
		ID: 2, From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate},
		Document: &models.Document{FileID: "f", FileName: "f.bin"},
	}})
	if err != nil || input == nil || len(input.Attachments) != 1 || input.Attachments[0].Error == "" || input.Attachments[0].ID != "" || !strings.Contains(input.Text, "附件 \"f.bin\" 接收失败") {
		t.Fatalf("failed-file input = (%#v, %v), want explicit error attachment and body", input, err)
	}
}

func TestPersonalCursorOnlyMovesAfterAcceptBatchSucceeds(t *testing.T) {
	inbox := &channelInboxFixture{acceptErr: errors.New("storage unavailable")}
	client := personalClient(t, inbox, newChannelFilesFixture(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("cursor test made a Telegram request")
		return nil, errors.New("unexpected request")
	}))
	update := &models.Update{ID: 1, Message: &models.Message{ID: 2, Text: "hello", From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate}}}
	if err := client.acceptUpdate(context.Background(), update); err == nil {
		t.Fatal("acceptUpdate() succeeded with failed storage")
	}
	if cursor, _ := inbox.Cursor(context.Background(), Channel, "123"); cursor != "" {
		t.Fatalf("cursor after failed accept = %q, want empty", cursor)
	}
	inbox.acceptErr = nil
	if err := client.acceptUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if cursor, _ := inbox.Cursor(context.Background(), Channel, "123"); cursor != "2" {
		t.Fatalf("cursor after successful accept = %q, want 2", cursor)
	}
}

func TestPersonalSendDocumentPreservesBytesNameMIMEAndReceipt(t *testing.T) {
	files := newChannelFilesFixture()
	data := []byte("delivery bytes\x00\xff")
	attachmentID := files.put("result report.bin", "application/x-report", data)
	var sends int
	client := personalClient(t, &channelInboxFixture{}, files, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/bot123:fixture_secret/sendDocument" {
			t.Fatalf("send path = %q", req.URL.Path)
		}
		sends++
		mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("request content type = %q, %v", req.Header.Get("Content-Type"), err)
		}
		reader := multipart.NewReader(req.Body, params["boundary"])
		var part *multipart.Part
		for {
			part, err = reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if part.FormName() == "chat_id" {
				chatID, _ := io.ReadAll(part)
				if string(chatID) != "7" {
					t.Errorf("document chat = %q, want 7", chatID)
				}
			}
			if part.FormName() == "document" {
				if part.FileName() != "result report.bin" || part.Header.Get("Content-Type") != "application/x-report" {
					t.Errorf("document metadata = filename %q MIME %q", part.FileName(), part.Header.Get("Content-Type"))
				}
				got, _ := io.ReadAll(part)
				if !bytes.Equal(got, data) {
					t.Errorf("document bytes = %v, want %v", got, data)
				}
			}
		}
		return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"message_id":88}}`), nil
	}))
	receipt, err := client.Send(context.Background(), channel.Content{AttachmentID: attachmentID})
	if err != nil || sends != 1 || receipt != (channel.Receipt{Channel: "telegram", Account: "123", Conversation: "7", MessageID: "88"}) {
		t.Fatalf("Send() = (%+v, %v), calls %d", receipt, err, sends)
	}
}

func TestPersonalSendTextAndMissingMessageID(t *testing.T) {
	for _, tc := range []struct {
		name      string
		response  string
		wantID    string
		uncertain bool
	}{
		{name: "success", response: `{"ok":true,"result":{"message_id":91}}`, wantID: "91"},
		{name: "missing message ID", response: `{"ok":true,"result":{}}`, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends int
			client := personalClient(t, &channelInboxFixture{}, newChannelFilesFixture(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/bot123:fixture_secret/sendMessage" {
					t.Fatalf("send path = %q", req.URL.Path)
				}
				sends++
				if err := req.ParseMultipartForm(1 << 20); err != nil {
					t.Fatal(err)
				}
				if req.FormValue("chat_id") != "7" || req.FormValue("text") != "one outgoing message" {
					t.Errorf("sent chat/text = %q/%q", req.FormValue("chat_id"), req.FormValue("text"))
				}
				return apiResponse(req, http.StatusOK, tc.response), nil
			}))
			receipt, err := client.Send(context.Background(), channel.Content{Text: "one outgoing message"})
			if sends != 1 {
				t.Fatalf("send calls = %d, want 1", sends)
			}
			if tc.uncertain {
				var sendErr *channel.SendError
				if !errors.As(err, &sendErr) || !sendErr.Uncertain || receipt != (channel.Receipt{Channel: Channel, Account: "123", Conversation: "7"}) {
					t.Fatalf("Send() = (%+v, %#v), want target receipt and uncertain outcome", receipt, err)
				}
				return
			}
			if err != nil || receipt.MessageID != tc.wantID || receipt.Channel != Channel || receipt.Account != "123" || receipt.Conversation != "7" {
				t.Fatalf("Send() = (%+v, %v), want message ID %s", receipt, err, tc.wantID)
			}
		})
	}
}

func TestPersonalFileWithoutMessageIDIsUncertain(t *testing.T) {
	files := newChannelFilesFixture()
	id := files.put("result.txt", "text/plain", []byte("result"))
	client := personalClient(t, &channelInboxFixture{}, files, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return apiResponse(req, http.StatusOK, `{"ok":true,"result":{}}`), nil
	}))
	receipt, err := client.Send(context.Background(), channel.Content{AttachmentID: id})
	var sendErr *channel.SendError
	if !errors.As(err, &sendErr) || !sendErr.Uncertain || receipt != (channel.Receipt{Channel: Channel, Account: "123", Conversation: "7"}) {
		t.Fatalf("file send without message ID = (%+v, %#v), want target receipt and uncertain outcome", receipt, err)
	}
}

func TestPersonalSendKeepsAuthWaitingAndUncertainSeparate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		response  string
		status    int
		transport error
		waiting   bool
		uncertain bool
	}{
		{name: "auth", status: http.StatusUnauthorized, response: `{"ok":false,"error_code":401,"description":"bad token fixture_secret"}`, waiting: true},
		{name: "known rejection", status: http.StatusBadRequest, response: `{"ok":false,"error_code":400}`},
		{name: "uncertain transport", transport: errors.New("connection lost"), uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := newChannelFilesFixture()
			id := files.put("f.txt", "text/plain", []byte("f"))
			client := personalClient(t, &channelInboxFixture{}, files, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if tc.transport != nil {
					return nil, tc.transport
				}
				return apiResponse(req, tc.status, tc.response), nil
			}))
			receipt, err := client.Send(context.Background(), channel.Content{AttachmentID: id})
			var sendErr *channel.SendError
			if !errors.As(err, &sendErr) || sendErr.Waiting != tc.waiting || sendErr.Uncertain != tc.uncertain || receipt != (channel.Receipt{Channel: Channel, Account: "123", Conversation: "7"}) {
				t.Fatalf("Send() = (%+v, %#v), want target receipt, waiting=%v uncertain=%v", receipt, err, tc.waiting, tc.uncertain)
			}
			if strings.Contains(err.Error(), "fixture_secret") || strings.Contains(err.Error(), "bad token") {
				t.Fatalf("unsafe error text leaked: %q", err)
			}
			if tc.waiting {
				if err := client.Ready(context.Background()); err == nil {
					t.Fatal("Ready() allowed a send after known auth failure")
				}
				if _, err := client.Send(context.Background(), channel.Content{Text: "later"}); !errors.As(err, &sendErr) || !sendErr.Waiting {
					t.Fatalf("send after auth failure = %v, want waiting error", err)
				}
			}
		})
	}
}

func TestUnsupportedNativeMediaBecomesExplicitInput(t *testing.T) {
	client := personalClient(t, &channelInboxFixture{}, newChannelFilesFixture(), roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported media made an API request")
		return nil, errors.New("unexpected request")
	}))
	input, err := client.inputForUpdate(context.Background(), &models.Update{ID: 1, Message: &models.Message{
		ID: 9, From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate},
		Photo: []models.PhotoSize{{FileID: "photo"}}, Caption: "caption",
	}})
	if err != nil || input == nil || !strings.Contains(input.Text, "暂不支持的 Telegram 原生图片") || !strings.Contains(input.Text, "caption") {
		t.Fatalf("unsupported-media input = (%#v, %v)", input, err)
	}
}

func TestPollParksAfterAuthenticationFailureWithoutReturning(t *testing.T) {
	started := make(chan struct{})
	client := personalClient(t, &channelInboxFixture{}, newChannelFilesFixture(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		return apiResponse(req, http.StatusUnauthorized, `{"ok":false,"error_code":401}`), nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Poll(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("poll request did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("Poll() returned after auth failure: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := client.Ready(context.Background()); err == nil {
		t.Fatal("Ready() did not report auth failure")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Poll() after cancellation = %v", err)
	}
}

var _ channel.Inbox = (*channelInboxFixture)(nil)
var _ channel.Files = (*channelFilesFixture)(nil)
