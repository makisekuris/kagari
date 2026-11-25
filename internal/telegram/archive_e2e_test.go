package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/documentarchive"
	"kagari/internal/inputbudget"
	"kagari/internal/personal"
	"kagari/internal/personal/agentctx"
	"kagari/internal/personal/assistant"
	"kagari/internal/personal/service"
	"kagari/internal/personal/sqlite"
)

const (
	archiveE2EOriginal = "合同条款：租期五年，押金为两个月租金，日常维修责任由出租方承担。\n"
	archiveE2ESummary  = "租期五年，押金为两个月租金，日常维修责任由出租方承担。"
)

func TestTelegramDocumentArchiveEndToEnd(t *testing.T) {
	testTelegramDocumentArchiveEndToEnd(t, false)
}

func TestTelegramDocumentArchiveEndToEndRustFS(t *testing.T) {
	if os.Getenv("KAGARI_ARCHIVE_LIVE_TEST") != "1" {
		t.Skip("set KAGARI_ARCHIVE_LIVE_TEST=1 and configure a RustFS test bucket")
	}
	testTelegramDocumentArchiveEndToEnd(t, true)
}

func testTelegramDocumentArchiveEndToEnd(t *testing.T, live bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	store, err := sqlite.NewStore(filepath.Join(t.TempDir(), "archive-e2e.db"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BindChannel(ctx, Channel, "123", "7", false); err != nil {
		t.Fatal(err)
	}

	objects := &archiveE2EMemoryObjects{objects: make(map[string][]byte)}
	api := &archiveE2ETelegramAPI{store: store, objects: objects, original: []byte(archiveE2EOriginal), sent: make(chan string, 8)}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	objects.baseURL = server.URL

	archiveObjects := documentarchive.ObjectStore(objects)
	if live {
		archiveObjects = archiveE2ERustFS(t, store)
	}
	llm := &archiveE2EModel{store: store}
	limits, err := inputbudget.NewLimits(262_144, 5_000)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := inputbudget.NewAgenticModel(llm, inputbudget.ModelConfig{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	archive := documentarchive.NewService(store, store, archiveObjects, agentctx.ArchiveSummarizer(bounded), nil)
	main := &assistant.MainAgent{
		Store: store, Archive: archive, Model: bounded, MaxIterations: 6, Timeout: 10 * time.Second,
	}
	client, err := newPersonalClient(personalConfig(), store, store, zap.NewNop(), server.URL, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &service.Runtime{
		Store: store, Think: main.Think,
		Execute: func(context.Context, personal.Run, func(string) error) (personal.StageResult, error) {
			return personal.StageResult{}, nil
		},
		ReviewMemory: main.ReviewMemory, PresentReply: main.PresentReply,
		TaskConcurrency: 1, Poll: client.Poll, Archive: archive.Run,
		Prepare: channel.PrepareText, Send: client.Send, Ready: client.Ready, Log: zap.NewNop(),
	}
	runtimeDone := make(chan struct{})
	var runtimeErr error
	go func() {
		runtimeErr = runtime.Run(ctx)
		close(runtimeDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runtimeDone:
		case <-time.After(5 * time.Second):
			t.Error("runtime did not stop during cleanup")
		}
	})

	var delivered string
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for delivered == "" {
		select {
		case message := <-api.sent:
			if strings.Contains(message, "押金为两个月租金") && strings.Contains(message, "http") {
				delivered = message
			}
		case <-runtimeDone:
			t.Fatalf("runtime stopped before delivering the archive result: %v", runtimeErr)
		case <-deadline.C:
			t.Fatalf("timed out waiting for the summary and download link; Telegram requests=%v model calls=%v tool calls=%v replies=%d", api.allRequests(), llm.traces(), llm.toolCalls(), len(api.allSent()))
		}
	}

	cancel()
	select {
	case <-runtimeDone:
		if runtimeErr != nil {
			t.Fatalf("runtime shutdown: %v", runtimeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not stop after cancellation")
	}

	hits, err := archive.Search(context.Background(), documentarchive.Query{Text: "押金", Limit: 5})
	if err != nil || len(hits) != 1 {
		t.Fatalf("summary keyword search returned %d hits: %v", len(hits), err)
	}
	version := hits[0].Match
	if !version.Archived() || version.Summary != archiveE2ESummary || version.SummarySource != documentarchive.SummaryFromOriginal || version.SummaryScope == "" {
		t.Fatalf("archived version is missing uploaded original or extracted summary: %+v", version)
	}

	if got := llm.toolCalls(); !equalStrings(got, []string{"archive_attachment", "search_archives", "get_archive_download_link"}) {
		t.Fatalf("archive tool path = %v", got)
	}
	if !strings.Contains(delivered, archiveE2ESummary) {
		t.Fatal("Telegram reply omitted the searched summary")
	}
	link := archiveE2EURL(delivered)
	if link == "" {
		t.Fatal("Telegram reply omitted a download link")
	}
	downloadCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, link, nil)
	if err != nil {
		t.Fatal("invalid archive download URL in Telegram reply")
	}
	if live && request.URL.Query().Get("X-Amz-Signature") == "" {
		t.Fatal("Telegram reply omitted the RustFS download signature")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("fetching the archive link delivered through Telegram failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(len(archiveE2EOriginal))+1))
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, []byte(archiveE2EOriginal)) {
		t.Fatalf("downloaded archive original differs: status=%d body=%q err=%v", response.StatusCode, body, err)
	}
}

func archiveE2ERustFS(t *testing.T, store *sqlite.Store) documentarchive.ObjectStore {
	t.Helper()
	endpoint, bucket := os.Getenv("KAGARI_ARCHIVE_ENDPOINT"), os.Getenv("KAGARI_ARCHIVE_BUCKET")
	access, secret := os.Getenv("KAGARI_ARCHIVE_ACCESS_KEY_ID"), os.Getenv("KAGARI_ARCHIVE_SECRET_ACCESS_KEY")
	region := os.Getenv("KAGARI_ARCHIVE_REGION")
	if region == "" {
		region = "us-east-1"
	}
	objects, err := documentarchive.NewS3(endpoint, bucket, region, access, secret)
	if err != nil {
		t.Fatal(err)
	}
	cleaner := s3.NewFromConfig(aws.Config{
		Region: region, Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hits, err := store.SearchArchives(ctx, documentarchive.Query{})
		if err != nil {
			t.Errorf("find test object in isolated archive database: %v", err)
			return
		}
		for _, hit := range hits {
			key := hit.Match.ObjectKey
			if _, err := cleaner.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key}); err != nil {
				t.Errorf("could not remove RustFS test object %s", key)
			}
		}
	})
	return objects
}

type archiveE2ETelegramAPI struct {
	store    *sqlite.Store
	objects  *archiveE2EMemoryObjects
	original []byte
	mu       sync.Mutex
	sent     chan string
	messages []string
	requests []string
	second   bool
}

func (a *archiveE2ETelegramAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	request := r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
	if !strings.HasSuffix(r.URL.Path, "/getUpdates") || len(a.requests) < 6 {
		a.requests = append(a.requests, request)
	}
	a.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		a.getUpdates(w, r)
	case strings.HasSuffix(r.URL.Path, "/getFile"):
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{
			"file_id": "telegram-file", "file_unique_id": "telegram-file-unique",
			"file_size": len(a.original), "file_path": "documents/lease.txt",
		}})
	case strings.HasPrefix(r.URL.Path, "/file/bot"):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(a.original)
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		var payload struct {
			Text string `json:"text"`
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, "invalid sendMessage form", http.StatusBadRequest)
				return
			}
			payload.Text = r.FormValue("text")
		} else if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid sendMessage payload", http.StatusBadRequest)
			return
		}
		a.recordMessage(payload.Text)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": len(a.allSent())}})
	case r.URL.Path == "/archive-download":
		file, err := a.objects.Open(r.Context(), r.URL.Query().Get("key"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		_, _ = io.Copy(w, file)
	default:
		http.Error(w, "unexpected Bot API request", http.StatusNotFound)
	}
}

func (a *archiveE2ETelegramAPI) getUpdates(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("offset") {
	case "0":
		writeTelegramJSON(w, map[string]any{"ok": true, "result": []any{map[string]any{
			"update_id": 1,
			"message": map[string]any{
				"message_id": 10, "from": map[string]any{"id": 7, "is_bot": false, "first_name": "Owner"},
				"chat":     map[string]any{"id": 7, "type": "private", "first_name": "Owner"},
				"caption":  "请归档这份合同，标题用房屋租赁合同。",
				"document": map[string]any{"file_id": "telegram-file", "file_unique_id": "telegram-file-unique", "file_name": "lease.txt", "mime_type": "text/plain", "file_size": len(a.original)},
			},
		}}})
	case "2":
		if a.isArchiveReady(r.Context()) {
			a.mu.Lock()
			if !a.second {
				a.second = true
				a.mu.Unlock()
				writeTelegramJSON(w, map[string]any{"ok": true, "result": []any{map[string]any{
					"update_id": 2,
					"message": map[string]any{
						"message_id": 11, "from": map[string]any{"id": 7, "is_bot": false, "first_name": "Owner"},
						"chat": map[string]any{"id": 7, "type": "private", "first_name": "Owner"},
						"text": "请搜索押金对应合同的简述，并给我原件下载链接。",
					},
				}}})
				return
			}
			a.mu.Unlock()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(40 * time.Millisecond):
		}
		writeTelegramJSON(w, map[string]any{"ok": true, "result": []any{}})
	default:
		select {
		case <-r.Context().Done():
		case <-time.After(40 * time.Millisecond):
		}
		writeTelegramJSON(w, map[string]any{"ok": true, "result": []any{}})
	}
}

func (a *archiveE2ETelegramAPI) isArchiveReady(ctx context.Context) bool {
	hits, err := a.store.SearchArchives(ctx, documentarchive.Query{Text: "押金", Limit: 5})
	return err == nil && len(hits) == 1 && hits[0].Match.Archived() && hits[0].Match.Summary == archiveE2ESummary
}

func (a *archiveE2ETelegramAPI) recordMessage(message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, message)
	if a.sent == nil {
		a.sent = make(chan string, 8)
	}
	a.sent <- message
}

func (a *archiveE2ETelegramAPI) allSent() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.messages...)
}

func (a *archiveE2ETelegramAPI) allRequests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func writeTelegramJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

type archiveE2EMemoryObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	baseURL string
}

func (o *archiveE2EMemoryObjects) Put(_ context.Context, key string, body io.ReadSeeker, size int64, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return io.ErrUnexpectedEOF
	}
	o.mu.Lock()
	o.objects[key] = data
	o.mu.Unlock()
	return nil
}

func (o *archiveE2EMemoryObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	data, ok := o.objects[key]
	o.mu.Unlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (o *archiveE2EMemoryObjects) DownloadURL(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return o.baseURL + "/archive-download?key=" + url.QueryEscape(key), nil
}

type archiveE2EModel struct {
	store *sqlite.Store
	mu    sync.Mutex
	calls []string
	trace []string
}

func (m *archiveE2EModel) Generate(ctx context.Context, messages []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	allText, currentText, results := archiveE2EModelInput(messages)
	common := model.GetCommonOptions(nil, opts...)
	m.mu.Lock()
	m.trace = append(m.trace, fmt.Sprintf("tools=%d current=%q hasArchiveInstruction=%t", len(common.Tools), currentText, strings.Contains(allText, "Document archive")))
	m.mu.Unlock()
	if len(common.Tools) > 0 || strings.Contains(allText, "Document archive") {
		if result, ok := results["get_archive_download_link"]; ok {
			_ = result
			_, summary := archiveE2ESearchSummary(results["search_archives"])
			return archiveE2EAssistant(fmt.Sprintf("合同简述：%s", summary)), nil
		}
		if result, ok := results["search_archives"]; ok {
			versionID, summary := archiveE2ESearchSummary(result)
			if versionID <= 0 || summary == "" {
				return nil, errors.New("search result did not include its version summary")
			}
			m.recordTool("get_archive_download_link")
			return archiveE2EToolCall("download", "get_archive_download_link", fmt.Sprintf(`{"version_id":%d}`, versionID)), nil
		}
		if _, ok := results["archive_attachment"]; ok {
			return archiveE2EAssistant("文件已提交归档。"), nil
		}
		if strings.Contains(currentText, "归档这份合同") {
			files, err := m.store.Attachments(ctx)
			if err != nil {
				return nil, err
			}
			for _, file := range files {
				if file.Name == "lease.txt" {
					m.recordTool("archive_attachment")
					args, _ := json.Marshal(map[string]any{"attachment_id": file.ID, "title": "房屋租赁合同"})
					return archiveE2EToolCall("archive", "archive_attachment", string(args)), nil
				}
			}
			return nil, errors.New("Telegram document was not saved to SQLite")
		}
		if strings.Contains(currentText, "押金") {
			m.recordTool("search_archives")
			return archiveE2EToolCall("search", "search_archives", `{"query":"押金","limit":5}`), nil
		}
		return nil, fmt.Errorf("unexpected tool-enabled model input: %q", currentText)
	}

	if strings.Contains(allText, "给归档文件写一两句中文简述") {
		if !strings.Contains(allText, strings.TrimSpace(archiveE2EOriginal)) {
			return nil, errors.New("archive summarizer did not receive extracted document text")
		}
		return archiveE2EAssistant(archiveE2ESummary), nil
	}
	if payload, ok := archiveE2ELastInputJSON(messages); ok {
		var format struct {
			Draft        string `json:"draft"`
			OwnerRequest string `json:"owner_request"`
		}
		if json.Unmarshal([]byte(payload), &format) == nil && format.Draft != "" {
			if strings.Contains(payload, `"owner_request"`) {
				encoded, _ := json.Marshal(map[string]string{"omitted_text": "", "text": format.Draft})
				return archiveE2EAssistant(string(encoded)), nil
			}
			encoded, _ := json.Marshal(map[string]any{"text": format.Draft, "reply_kind": "content", "attachments": []string{}})
			return archiveE2EAssistant(string(encoded)), nil
		}
	}
	// The runtime's real memory-review worker has no durable fact to commit here.
	return archiveE2EAssistant(`{"changes":[],"question":""}`), nil
}

func (m *archiveE2EModel) traces() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.trace...)
}

func (m *archiveE2EModel) Stream(ctx context.Context, messages []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	response, err := m.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{response}), nil
}

func (m *archiveE2EModel) recordTool(name string) {
	m.mu.Lock()
	m.calls = append(m.calls, name)
	m.mu.Unlock()
}

func (m *archiveE2EModel) toolCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func archiveE2EModelInput(messages []*schema.AgenticMessage) (allText, currentText string, results map[string]string) {
	results = make(map[string]string)
	for _, message := range messages {
		if message == nil {
			continue
		}
		for _, block := range message.ContentBlocks {
			if block == nil {
				continue
			}
			if block.UserInputText != nil {
				allText += block.UserInputText.Text + "\n"
				var view struct {
					Event struct {
						Text string `json:"text"`
					} `json:"event"`
				}
				if json.Unmarshal([]byte(block.UserInputText.Text), &view) == nil && view.Event.Text != "" {
					currentText = view.Event.Text
				}
			}
			if block.AssistantGenText != nil {
				allText += block.AssistantGenText.Text + "\n"
			}
			if call := block.FunctionToolCall; call != nil {
				allText += call.Name + " " + call.Arguments + "\n"
			}
			if result := block.FunctionToolResult; result != nil {
				var content strings.Builder
				for _, part := range result.Content {
					if part != nil && part.Text != nil {
						content.WriteString(part.Text.Text)
					}
				}
				results[result.Name] = content.String()
				allText += result.Name + " " + content.String() + "\n"
			}
		}
	}
	return allText, currentText, results
}

func archiveE2ELastInputJSON(messages []*schema.AgenticMessage) (string, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i] == nil {
			continue
		}
		for j := len(messages[i].ContentBlocks) - 1; j >= 0; j-- {
			block := messages[i].ContentBlocks[j]
			if block == nil || block.UserInputText == nil {
				continue
			}
			text := block.UserInputText.Text
			if json.Valid([]byte(text)) {
				return text, true
			}
		}
	}
	return "", false
}

func archiveE2ESearchSummary(raw string) (int64, string) {
	var reply struct {
		Hits []struct {
			Match struct {
				ID      int64  `json:"version_id"`
				Summary string `json:"summary"`
			} `json:"match"`
		} `json:"hits"`
	}
	if json.Unmarshal([]byte(raw), &reply) != nil || len(reply.Hits) == 0 {
		return 0, ""
	}
	return reply.Hits[0].Match.ID, reply.Hits[0].Match.Summary
}

func archiveE2EAssistant(text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.AssistantGenText{Text: text}),
	}}
}

func archiveE2EToolCall(id, name, args string) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
		schema.NewContentBlock(&schema.FunctionToolCall{CallID: id, Name: name, Arguments: args}),
	}}
}

func archiveE2EURL(text string) string {
	for _, part := range strings.Fields(text) {
		if strings.HasPrefix(part, "http://") || strings.HasPrefix(part, "https://") {
			return strings.TrimRight(part, ".,;)")
		}
	}
	return ""
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
