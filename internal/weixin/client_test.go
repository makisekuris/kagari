package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"kagari/internal/channel"
)

type acceptedBatch struct {
	account string
	cursor  string
	inputs  []channel.Input
}

type fakeInbox struct {
	mu       sync.Mutex
	cursors  map[string]string
	batches  []acceptedBatch
	onAccept func(int)
}

func newFakeInbox() *fakeInbox { return &fakeInbox{cursors: make(map[string]string)} }

func (f *fakeInbox) Cursor(_ context.Context, channelName, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursors[channelName+"/"+account], nil
}

func (f *fakeInbox) AcceptBatch(_ context.Context, _ string, account, cursor string, inputs []channel.Input) ([]int64, error) {
	f.mu.Lock()
	f.cursors[Channel+"/"+account] = cursor
	batch := acceptedBatch{account: account, cursor: cursor, inputs: append([]channel.Input(nil), inputs...)}
	f.batches = append(f.batches, batch)
	n := len(f.batches)
	callback := f.onAccept
	f.mu.Unlock()
	if callback != nil {
		callback(n)
	}
	return nil, nil
}

func (f *fakeInbox) snapshot() []acceptedBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]acceptedBatch(nil), f.batches...)
}

type fakeFiles struct {
	mu          sync.Mutex
	attachments map[string]channel.Attachment
	data        map[string][]byte
	saved       []channel.Attachment
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{attachments: make(map[string]channel.Attachment), data: make(map[string][]byte)}
}

func (f *fakeFiles) SaveAttachment(_ context.Context, name, contentType string, source io.Reader) (channel.Attachment, error) {
	data, err := io.ReadAll(source)
	if err != nil {
		return channel.Attachment{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "saved-" + string(rune('a'+len(f.saved)))
	attachment := channel.Attachment{ID: id, Name: name, MIME: contentType, Size: int64(len(data))}
	f.attachments[id] = attachment
	f.data[id] = data
	f.saved = append(f.saved, attachment)
	return attachment, nil
}

func (f *fakeFiles) Attachment(_ context.Context, id string) (channel.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attachment, ok := f.attachments[id]
	if !ok {
		return channel.Attachment{}, errors.New("missing attachment")
	}
	return attachment, nil
}

func (f *fakeFiles) OpenAttachment(_ context.Context, id string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.data[id]
	if !ok {
		return nil, errors.New("missing attachment")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (f *fakeFiles) put(id string, attachment channel.Attachment, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachments[id] = attachment
	f.data[id] = append([]byte(nil), data...)
}

func (f *fakeFiles) read(id string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data[id]...)
}

func writeTestCredentials(t *testing.T, path, token, account, user string) Credentials {
	t.Helper()
	credentials := Credentials{Token: token, AccountID: account, UserID: user, BaseURL: defaultAPIBaseURL}
	if err := SaveCredentials(path, credentials); err != nil {
		t.Fatal(err)
	}
	return credentials
}

func makeTestClient(t *testing.T, server *httptest.Server, path string, inbox *fakeInbox, files *fakeFiles, httpClient *http.Client) *Client {
	t.Helper()
	client, err := newClientForTest(path, inbox, files, zap.NewNop(), httpClient, server.URL, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func setPrivateContext(t *testing.T, client *Client, credentials Credentials, contextToken string) {
	t.Helper()
	_, err := client.saveStateFor(credentials, privateState{AccountID: credentials.AccountID, UserID: credentials.UserID, TokenDigest: tokenDigest(credentials.Token), ContextToken: contextToken})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPollOwnerCursorFileAndPrivateContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weixin.json")
	credentials := writeTestCredentials(t, path, "secret-token", "bot-account", "owner")
	files, inbox := newFakeFiles(), newFakeInbox()
	fileBytes := []byte("file bytes with null \x00 and utf-8 文本")
	aesKey := bytes.Repeat([]byte{0x3a}, 16)
	ciphertext := encryptAES128ECB(fileBytes, aesKey)
	var cancel context.CancelFunc
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cdn-download":
			if r.Header.Get("Authorization") != "" {
				t.Error("credential token was sent to CDN")
			}
			_, _ = w.Write(ciphertext)
		case "/ilink/bot/getupdates":
			if r.Header.Get("Authorization") != "Bearer secret-token" || r.Header.Get("AuthorizationType") != "ilink_bot_token" {
				t.Errorf("missing API auth headers: %v", r.Header)
			}
			if r.Header.Get("iLink-App-Id") != "bot" || r.Header.Get("iLink-App-ClientVersion") != apiClientVersion {
				t.Errorf("missing API app headers: %v", r.Header)
			}
			var body struct {
				Cursor   string         `json:"get_updates_buf"`
				BaseInfo map[string]any `json:"base_info"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode updates body: %v", err)
			}
			if body.BaseInfo["channel_version"] != apiVersion || body.BaseInfo["bot_agent"] == nil {
				t.Errorf("missing base_info: %#v", body.BaseInfo)
			}
			requestCount++
			if requestCount == 1 {
				if body.Cursor != "" {
					t.Errorf("initial cursor = %q", body.Cursor)
				}
				mediaKey := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(aesKey)))
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ret":             0,
					"get_updates_buf": "opaque cursor/with+symbols",
					"msgs": []any{
						map[string]any{"message_id": json.Number("18446744073709551615"), "from_user_id": "owner", "message_type": 1, "context_token": "private-context-secret", "item_list": []any{
							map[string]any{"type": 1, "text_item": map[string]string{"text": "hello"}, "ref_msg": map[string]string{"svr_id": "quoted-message"}},
							map[string]any{"type": 4, "file_item": map[string]any{"file_name": "memo.txt", "len": strconv.Itoa(len(fileBytes)), "md5": md5Hex(fileBytes), "media": map[string]string{"full_url": serverURLForCDN(r), "aes_key": mediaKey}}},
							map[string]any{"type": 4, "file_item": map[string]any{"file_name": "bad.txt", "len": strconv.Itoa(len(fileBytes)), "md5": strings.Repeat("0", 32), "media": map[string]string{"full_url": serverURLForCDN(r), "aes_key": mediaKey}}},
							map[string]any{"type": 2},
						}},
						map[string]any{"message_id": 2, "from_user_id": "owner", "group_id": "group-1", "message_type": 1, "item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": "group"}}}},
						map[string]any{"message_id": 3, "from_user_id": "other", "message_type": 1, "item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": "foreign"}}}},
						map[string]any{"message_id": 4, "from_user_id": "owner", "message_type": 2, "item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": "bot"}}}},
						map[string]any{"from_user_id": "owner", "message_type": 1, "item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": "missing ID"}}}},
					},
				})
				return
			}
			if body.Cursor != "opaque cursor/with+symbols" {
				t.Errorf("cursor changed between requests: %q", body.Cursor)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "get_updates_buf": "", "msgs": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := makeTestClient(t, server, path, inbox, files, &http.Client{Timeout: 2 * time.Second})
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop
	inbox.onAccept = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	if err := client.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if requestCount != 2 {
		t.Fatalf("getupdates requests = %d, want 2", requestCount)
	}
	batches := inbox.snapshot()
	if len(batches) != 2 || batches[0].cursor != "opaque cursor/with+symbols" || batches[1].cursor != "opaque cursor/with+symbols" {
		t.Fatalf("cursor batches = %#v", batches)
	}
	if len(batches[0].inputs) != 1 {
		t.Fatalf("accepted %d messages, want only one private owner message", len(batches[0].inputs))
	}
	input := batches[0].inputs[0]
	if input.MessageID != "18446744073709551615" || input.ReplyToID != "quoted-message" || input.Text != "hello" || input.Sender != "owner" || input.Account != "bot-account" {
		t.Fatalf("unexpected message: %+v", input)
	}
	if len(input.Attachments) != 3 || input.Attachments[0].ID == "" || input.Attachments[0].Name != "memo.txt" || input.Attachments[0].Size != int64(len(fileBytes)) || string(files.read(input.Attachments[0].ID)) != string(fileBytes) || !strings.Contains(input.Attachments[1].Error, "MD5") || input.Attachments[2].Error == "" {
		t.Fatalf("unexpected file/media attachments: %+v", input.Attachments)
	}
	encodedInput, _ := json.Marshal(input)
	if strings.Contains(string(encodedInput), "private-context-secret") {
		t.Fatal("context token escaped into channel input")
	}
	state, err := client.loadState()
	if err != nil || state.ContextToken != "private-context-secret" || state.TokenDigest != tokenDigest(credentials.Token) {
		t.Fatalf("private state was not saved: %+v, %v", state, err)
	}
}

func TestPollLongPollTimeoutDoesNotStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weixin.json")
	writeTestCredentials(t, path, "secret-token", "bot-account", "owner")
	inbox := newFakeInbox()
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		time.Sleep(30 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "msgs": []any{}})
	}))
	defer server.Close()
	client := makeTestClient(t, server, path, inbox, newFakeFiles(), &http.Client{Timeout: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbox.onAccept = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	if err := client.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if requestCount.Load() < 2 || len(inbox.snapshot()) < 2 {
		t.Fatalf("poll stopped after timeout: requests=%d accepts=%d", requestCount.Load(), len(inbox.snapshot()))
	}
}

func TestPollCredentialReloadDropsStaleContextResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weixin.json")
	oldCredentials := writeTestCredentials(t, path, "old-token", "bot-account", "owner")
	newCredentials := oldCredentials
	newCredentials.Token = "new-token"
	inbox := newFakeInbox()
	entered, release := make(chan struct{}), make(chan struct{})
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("Authorization") == "Bearer old-token" {
			close(entered)
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "get_updates_buf": "old-cursor", "msgs": []any{map[string]any{"message_id": 1, "from_user_id": "owner", "message_type": 1, "context_token": "stale-context", "item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": "stale"}}}}}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-token" {
			t.Errorf("unexpected reloaded token header %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "get_updates_buf": "new-cursor", "msgs": []any{map[string]any{"message_id": 2, "from_user_id": "owner", "message_type": 1, "context_token": "fresh-context", "item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": "fresh"}}}}}})
	}))
	defer server.Close()
	client := makeTestClient(t, server, path, inbox, newFakeFiles(), &http.Client{Timeout: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbox.onAccept = func(int) { cancel() }
	done := make(chan error, 1)
	go func() { done <- client.Poll(ctx) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old long poll did not start")
	}
	if err := SaveCredentials(path, newCredentials); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not switch to refreshed credentials")
	}
	if requestCount != 2 {
		t.Fatalf("getupdates calls = %d, want old response discarded and new credentials polled", requestCount)
	}
	batches := inbox.snapshot()
	if len(batches) != 1 || batches[0].account != "bot-account" || len(batches[0].inputs) != 1 || batches[0].inputs[0].Text != "fresh" {
		t.Fatalf("stale batch was accepted: %#v", batches)
	}
	state, err := client.loadState()
	if err != nil || state.ContextToken != "fresh-context" || state.TokenDigest != tokenDigest(newCredentials.Token) {
		t.Fatalf("stale context overwrote refreshed session: %+v, %v", state, err)
	}
}

func TestPollSessionExpiryWaitsForCredentialReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weixin.json")
	oldCredentials := writeTestCredentials(t, path, "old-token", "bot-account", "owner")
	newCredentials := oldCredentials
	newCredentials.Token = "new-token"
	inbox := newFakeInbox()
	expired := make(chan struct{})
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Header.Get("Authorization") == "Bearer old-token" {
			close(expired)
			_ = json.NewEncoder(w).Encode(map[string]any{"ret": -14, "errmsg": "expired"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-token" {
			t.Errorf("unexpected refreshed auth header %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "msgs": []any{map[string]any{"message_id": 7, "from_user_id": "owner", "message_type": 1, "context_token": "fresh-session", "item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": "resume"}}}}}})
	}))
	defer server.Close()
	client := makeTestClient(t, server, path, inbox, newFakeFiles(), &http.Client{Timeout: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inbox.onAccept = func(int) { cancel() }
	done := make(chan error, 1)
	go func() { done <- client.Poll(ctx) }()
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("expired session response was not received")
	}
	if err := SaveCredentials(path, newCredentials); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("poll did not resume after credentials were replaced")
	}
	if requestCount != 2 || len(inbox.snapshot()) != 1 {
		t.Fatalf("poll did not wait for then use renewed credentials: requests=%d batches=%d", requestCount, len(inbox.snapshot()))
	}
	if inbox.snapshot()[0].inputs[0].Text != "resume" {
		t.Fatalf("unexpected resumed message: %+v", inbox.snapshot()[0].inputs[0])
	}
}

func TestReadyWaitsForRenewedTokenAndFreshSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weixin.json")
	credentials := writeTestCredentials(t, path, "old-token", "bot-account", "owner")
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	client := makeTestClient(t, server, path, newFakeInbox(), newFakeFiles(), nil)
	if err := client.Ready(context.Background()); !isWaiting(err) {
		t.Fatalf("Ready without a session = %v, want waiting", err)
	}
	setPrivateContext(t, client, credentials, "old-private-context")
	if err := client.Ready(context.Background()); err != nil {
		t.Fatalf("Ready with current session = %v", err)
	}
	client.markTokenInvalid(credentials.Token)
	if err := client.Ready(context.Background()); !isWaiting(err) {
		t.Fatalf("Ready after auth expiry = %v, want waiting", err)
	}
	credentials.Token = "new-token"
	if err := SaveCredentials(path, credentials); err != nil {
		t.Fatal(err)
	}
	if err := client.Ready(context.Background()); !isWaiting(err) {
		t.Fatalf("Ready reused old context after token reload = %v, want waiting", err)
	}
	setPrivateContext(t, client, credentials, "new-private-context")
	if err := client.Ready(context.Background()); err != nil {
		t.Fatalf("Ready with refreshed context = %v", err)
	}
}

func isWaiting(err error) bool {
	var sendErr *channel.SendError
	return errors.As(err, &sendErr) && sendErr.Waiting
}

func serverURLForCDN(r *http.Request) string { return "http://" + r.Host + "/cdn-download" }

func serverURLForUpload(r *http.Request) string {
	return "http://" + r.Host + "/cdn-upload?target=upload"
}

func md5Hex(data []byte) string {
	checksum := md5.Sum(data)
	return hex.EncodeToString(checksum[:])
}

func paddedSize(data []byte) int { return (len(data)/aes.BlockSize + 1) * aes.BlockSize }

func TestSendTextAndFileWithKnownAndUncertainOutcomes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weixin.json")
	credentials := writeTestCredentials(t, path, "secret-token", "bot-account", "owner")
	files, inbox := newFakeFiles(), newFakeInbox()
	fileData := []byte("outbound bytes that must roundtrip exactly")
	files.put("out", channel.Attachment{ID: "out", Name: "report.txt", MIME: "text/plain", Size: int64(len(fileData))}, fileData)
	var mu sync.Mutex
	var sendCount int
	var uploaded []byte
	var uploadedAESKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/sendmessage":
			if r.Header.Get("Authorization") != "Bearer secret-token" {
				t.Errorf("send token header = %q", r.Header.Get("Authorization"))
			}
			var body struct {
				Message struct {
					From    string `json:"from_user_id"`
					To      string `json:"to_user_id"`
					Client  string `json:"client_id"`
					Context string `json:"context_token"`
					Items   []struct {
						Type int `json:"type"`
						File *struct {
							Name   string `json:"file_name"`
							MD5    string `json:"md5"`
							Length string `json:"len"`
							Media  struct {
								Param       string `json:"encrypt_query_param"`
								AESKey      string `json:"aes_key"`
								EncryptType int    `json:"encrypt_type"`
							} `json:"media"`
						} `json:"file_item"`
						Text *textItem `json:"text_item"`
					} `json:"item_list"`
				} `json:"msg"`
				BaseInfo map[string]any `json:"base_info"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode send body: %v", err)
			}
			if body.Message.From != "" || body.Message.To != "owner" || body.Message.Client == "" || body.Message.Context != "private-context-secret" || body.BaseInfo["channel_version"] != apiVersion {
				t.Errorf("unexpected outbound message metadata: %+v", body)
			}
			mu.Lock()
			sendCount++
			count := sendCount
			mu.Unlock()
			if len(body.Message.Items) == 1 && body.Message.Items[0].File != nil {
				if body.Message.Items[0].File.Media.Param != "encrypted-ref" || body.Message.Items[0].File.Name != "report.txt" || body.Message.Items[0].File.Media.EncryptType != 1 {
					t.Errorf("unexpected file item: %+v", body.Message.Items[0].File)
				}
				keyText, err := base64.StdEncoding.DecodeString(body.Message.Items[0].File.Media.AESKey)
				if err != nil {
					t.Errorf("decode outbound media key: %v", err)
				}
				key, err := hex.DecodeString(string(keyText))
				if err != nil || len(key) != 16 {
					t.Errorf("invalid outbound media key: %q", keyText)
				}
				var ct []byte
				mu.Lock()
				ct = append([]byte(nil), uploaded...)
				mu.Unlock()
				plain, err := decryptAES128ECB(ct, key)
				if err != nil || !bytes.Equal(plain, fileData) {
					t.Errorf("uploaded file did not roundtrip: %q, %v", plain, err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "message_id": 987654321})
				return
			}
			switch count {
			case 1:
				if body.Message.Items[0].Text == nil || body.Message.Items[0].Text.Text != "hello" {
					t.Errorf("unexpected text item: %+v", body.Message.Items)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0})
			case 2:
				_ = json.NewEncoder(w).Encode(map[string]any{"ret": 9, "errmsg": "known rejection"})
			case 3:
				_, _ = io.WriteString(w, `{}`)
			case 5:
				w.WriteHeader(http.StatusInternalServerError)
			default:
				t.Errorf("unexpected send call %d", count)
			}
		case "/ilink/bot/getuploadurl":
			var body struct {
				FileKey     string         `json:"filekey"`
				MediaType   int            `json:"media_type"`
				To          string         `json:"to_user_id"`
				RawSize     int64          `json:"rawsize"`
				RawMD5      string         `json:"rawfilemd5"`
				FileSize    int64          `json:"filesize"`
				NoNeedThumb bool           `json:"no_need_thumb"`
				AESKey      string         `json:"aeskey"`
				BaseInfo    map[string]any `json:"base_info"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upload URL body: %v", err)
			}
			if r.Header.Get("Authorization") != "Bearer secret-token" || body.MediaType != 3 || body.To != "owner" || body.RawSize != int64(len(fileData)) || body.FileSize != int64(paddedSize(fileData)) || !body.NoNeedThumb {
				t.Errorf("unexpected getuploadurl request: %+v", body)
			}
			if _, err := hex.DecodeString(body.AESKey); err != nil || len(body.AESKey) != 32 {
				t.Errorf("invalid getuploadurl aeskey %q", body.AESKey)
			}
			mu.Lock()
			uploadedAESKey = body.AESKey
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "upload_full_url": serverURLForUpload(r)})
		case "/cdn-upload":
			data, _ := io.ReadAll(r.Body)
			mu.Lock()
			uploaded = data
			mu.Unlock()
			w.Header().Set("x-encrypted-param", "encrypted-ref")
			w.WriteHeader(http.StatusOK)
		case "/cdn-upload/":
			t.Error("unexpected upload URL path")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := makeTestClient(t, server, path, inbox, files, &http.Client{Timeout: 3 * time.Second})
	setPrivateContext(t, client, credentials, "private-context-secret")

	receipt, err := client.Send(context.Background(), channel.Content{Text: "hello"})
	if err != nil || receipt.Channel != Channel || receipt.Account != "bot-account" || receipt.Conversation != "owner" || receipt.MessageID != "" {
		t.Fatalf("text success without numeric/server ID should keep empty receipt ID: %+v, %v", receipt, err)
	}
	receipt, err = client.Send(context.Background(), channel.Content{Text: "known reject"})
	if err == nil || receipt.MessageID != "" || receipt.Channel != Channel || !sendError(err).Permanent {
		t.Fatalf("business rejection classification/receipt = %+v, %v", receipt, err)
	}
	receipt, err = client.Send(context.Background(), channel.Content{Text: "missing ret"})
	if err == nil || receipt.MessageID != "" || receipt.Conversation != "owner" || !sendError(err).Uncertain {
		t.Fatalf("missing ret should be uncertain with target receipt: %+v, %v", receipt, err)
	}
	receipt, err = client.Send(context.Background(), channel.Content{AttachmentID: "out"})
	if err != nil || receipt.MessageID != "987654321" {
		t.Fatalf("file send receipt = %+v, %v", receipt, err)
	}
	mu.Lock()
	if uploadedAESKey == "" || !bytes.Equal(uploaded, encryptAES128ECB(fileData, mustHex(t, uploadedAESKey))) {
		mu.Unlock()
		t.Fatal("CDN upload ciphertext did not match getuploadurl aeskey")
	}
	mu.Unlock()
	receipt, err = client.Send(context.Background(), channel.Content{Text: "uncertain"})
	if err == nil || receipt.MessageID != "" || receipt.Account != "bot-account" || !sendError(err).Uncertain {
		t.Fatalf("HTTP 500 should be uncertain with target receipt: %+v, %v", receipt, err)
	}
}

func sendError(err error) *channel.SendError {
	var value *channel.SendError
	if errors.As(err, &value) {
		return value
	}
	return nil
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	out, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAES128ECBPKCS7Padding(t *testing.T) {
	key := bytes.Repeat([]byte{0x7d}, 16)
	for _, size := range []int{0, 1, 15, 16, 17, 31, 32, 33} {
		plain := bytes.Repeat([]byte("x"), size)
		ciphertext := encryptAES128ECB(plain, key)
		if len(ciphertext) != paddedSize(plain) {
			t.Fatalf("size %d ciphertext = %d, want %d", size, len(ciphertext), paddedSize(plain))
		}
		decoded, err := decryptAES128ECB(ciphertext, key)
		if err != nil || !bytes.Equal(decoded, plain) {
			t.Fatalf("size %d roundtrip = %q, %v", size, decoded, err)
		}
	}
	if _, err := decryptAES128ECB([]byte("short"), key); err == nil {
		t.Fatal("accepted non-block ciphertext")
	}
	if _, err := decryptAES128ECB(make([]byte, 16), key); err == nil {
		t.Fatal("accepted invalid padding")
	}
}

func TestSaveCredentialsRejectsUntrustedHosts(t *testing.T) {
	credentials := Credentials{Token: "token", AccountID: "account", UserID: "user", BaseURL: "https://attacker.example"}
	if err := SaveCredentials(filepath.Join(t.TempDir(), "creds.json"), credentials); err == nil {
		t.Fatal("saved credentials with an untrusted API host")
	}
}
