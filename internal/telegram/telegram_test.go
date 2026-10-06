package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/persona"
	"kagari/internal/store"

	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"
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

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testClient(t *testing.T, st *store.Store, cfg config.Telegram, prepare func(domain.Submission) (domain.Submission, error), transport http.RoundTripper) *Client {
	t.Helper()
	client, err := newClient(cfg, st, prepare, zap.NewNop(), persona.Default(), "https://telegram.test", transport)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type customReplyTemplate struct{}

func (customReplyTemplate) AskChatID(jobID int64) string {
	return fmt.Sprintf("Custom receipt for task #%d", jobID)
}

func baseConfig() config.Telegram {
	return config.Telegram{Token: "123:fixture_secret", AllowedUserIDs: []int64{7}, TargetChatIDs: []int64{900}}
}

func messageUpdate(id int64, msg string) string {
	urlText := "https://example.com/a"
	urlOffset := len(utf16.Encode([]rune("😀 ")))
	urlLength := len(utf16.Encode([]rune(urlText)))
	return fmt.Sprintf(`{"update_id":%d,"message":{"message_id":%d,"from":{"id":7,"is_bot":false,"first_name":"Reader"},"chat":{"id":7,"type":"private"},"date":1700000000,"text":%q,"entities":[{"type":"url","offset":%d,"length":%d}]}}`, id, id, msg, urlOffset, urlLength)
}

func TestPollPersistsBeforeAckExtractsUTF16AndDeduplicates(t *testing.T) {
	st := testStore(t)
	textUpdate := messageUpdate(10, "😀 https://example.com/a")
	captionUpdate := `{"update_id":11,"message":{"message_id":11,"from":{"id":7,"is_bot":false,"first_name":"Reader"},"chat":{"id":7,"type":"private"},"date":1700000001,"caption":"😀 Read","caption_entities":[{"type":"text_link","offset":3,"length":4,"url":"https://hidden.example/post"}]}}`
	noURLUpdate := `{"update_id":12,"message":{"message_id":12,"from":{"id":7,"is_bot":false,"first_name":"Reader"},"chat":{"id":7,"type":"private"},"date":1700000002,"text":"hello"}}`
	updates := `{"ok":true,"result":[` + textUpdate + `,` + captionUpdate + `,` + captionUpdate + `,` + noURLUpdate + `,{"update_id":13,"message":{"message_id":13,"from":{"id":7,"is_bot":false,"first_name":"Reader"},"chat":{"id":7,"type":"private"},"date":1700000003,"text":"/status 4"}}]}`
	var mu sync.Mutex
	pollCalls, sendCalls := 0, 0
	nextPoll := make(chan struct{})
	ackedOffsets := make(chan int64, 8)
	var nextOnce sync.Once
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/getUpdates"):
			mu.Lock()
			pollCalls++
			call := pollCalls
			mu.Unlock()
			if call == 1 {
				if req.URL.Query().Get("offset") != "0" || req.URL.Query().Get("timeout") != "20" {
					t.Errorf("unexpected poll query: %v", req.URL.Query())
				}
				return apiResponse(req, http.StatusOK, updates), nil
			}
			nextOnce.Do(func() { close(nextPoll) })
			<-req.Context().Done()
			return nil, req.Context().Err()
		case strings.HasSuffix(req.URL.Path, "/sendMessage"):
			mu.Lock()
			sendCalls++
			mu.Unlock()
			offset, err := st.Offset(context.Background())
			if err != nil || offset < 11 {
				t.Errorf("ack was sent before update persistence: offset=%d err=%v", offset, err)
			}
			ackedOffsets <- offset
			return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"message_id":88,"date":1700000004,"chat":{"id":7,"type":"private"}}}`), nil
		default:
			t.Errorf("unexpected API method %q", req.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})
	prepareCalls := 0
	client := testClient(t, st, baseConfig(), func(sub domain.Submission) (domain.Submission, error) {
		prepareCalls++
		sub.CacheKey = fmt.Sprintf("prepared-%d", prepareCalls)
		return sub, nil
	}, transport)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Poll(ctx) }()

	for i := 0; i < 2; i++ {
		select {
		case <-ackedOffsets:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for acknowledgements")
		}
	}
	select {
	case <-nextPoll:
	case <-time.After(3 * time.Second):
		t.Fatal("poller did not advance to the next offset")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if offset, err := st.Offset(context.Background()); err != nil || offset != 14 {
		t.Fatalf("persisted offset = (%d, %v), want 14", offset, err)
	}
	if sendCalls != 2 || prepareCalls != 4 {
		t.Fatalf("sends=%d prepare calls=%d, want 2 and 4", sendCalls, prepareCalls)
	}
	for i, want := range []struct {
		kind   string
		target int64
	}{
		{kind: "analyze", target: 900},
		{kind: "analyze", target: 900},
		{kind: "notice", target: 7},
		{kind: "command", target: 7},
	} {
		job, err := st.ClaimJob(context.Background())
		if err != nil || job == nil || job.Kind != want.kind || len(job.Targets) != 1 || job.Targets[0].Address != fmt.Sprint(want.target) {
			t.Fatalf("job %d = (%+v, %v), want kind=%s target=%d", i, job, err, want.kind, want.target)
		}
		if i < 2 {
			var sub domain.Submission
			if err := json.Unmarshal(job.Payload, &sub); err != nil {
				t.Fatal(err)
			}
			if i == 0 && (sub.Text != "😀 https://example.com/a" || len(sub.URLs) != 1 || sub.URLs[0] != "https://example.com/a" || sub.ReceivedAt.Unix() != 1700000000) {
				t.Fatalf("text entity submission = %#v", sub)
			}
			if i == 1 && (sub.Text != "😀 Read" || len(sub.URLs) != 1 || sub.URLs[0] != "https://hidden.example/post" || sub.ReceivedAt.Unix() != 1700000001) {
				t.Fatalf("caption text_link submission = %#v", sub)
			}
		}
	}
}

func TestAcceptUpdateUsesInjectedReplyAndDeduplicatesAck(t *testing.T) {
	st := testStore(t)
	var sends int
	var ackChatID string
	var ackText string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/sendMessage") {
			t.Fatalf("unexpected Telegram request: %s", req.URL.Path)
		}
		sends++
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		ackChatID, ackText = req.FormValue("chat_id"), req.FormValue("text")
		return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"message_id":88,"date":1700000004,"chat":{"id":7,"type":"private"}}}`), nil
	})
	client, err := newClient(baseConfig(), st, func(sub domain.Submission) (domain.Submission, error) { return sub, nil }, zap.NewNop(), customReplyTemplate{}, "https://telegram.test", transport)
	if err != nil {
		t.Fatal(err)
	}
	var update models.Update
	if err := json.Unmarshal([]byte(messageUpdate(40, "https://example.com/a")), &update); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := client.acceptUpdate(context.Background(), &update); err != nil {
			t.Fatal(err)
		}
	}
	if sends != 1 || ackChatID != "7" || ackText != "Custom receipt for task #1" {
		t.Fatalf("ack sends=%d chat=%q text=%q; want one injected ack to chat 7 for task 1", sends, ackChatID, ackText)
	}
	job, err := st.Job(context.Background(), 1)
	if err != nil || job == nil || job.ID != 1 || len(job.Targets) != 1 || job.Targets[0].Address != "900" {
		t.Fatalf("persisted job = (%+v, %v), want task 1 targeting chat 900", job, err)
	}
	// 同一条提交的文章目标可以包含频道；回执仍只发到原私聊。
	client.config.TargetChatIDs = []int64{7, -100123}
	if err := json.Unmarshal([]byte(messageUpdate(41, "https://example.com/b")), &update); err != nil {
		t.Fatal(err)
	}
	if err := client.acceptUpdate(context.Background(), &update); err != nil {
		t.Fatal(err)
	}
	job, err = st.Job(context.Background(), 2)
	if err != nil || job == nil || len(job.Targets) != 2 || job.Targets[0].Address != "7" || job.Targets[1].Address != "-100123" || sends != 2 || ackChatID != "7" {
		t.Fatalf("multi-target job = %+v, %v; ack sends=%d chat=%s", job, err, sends, ackChatID)
	}
}

func TestNewClientRequiresReplyTemplate(t *testing.T) {
	_, err := newClient(baseConfig(), testStore(t), func(sub domain.Submission) (domain.Submission, error) { return sub, nil }, zap.NewNop(), nil, "https://telegram.test", nil)
	if err == nil || err.Error() != "telegram reply template is required" {
		t.Fatalf("newClient() error = %v, want clear missing reply template error", err)
	}
}

func TestForwardedMessageSeparatesTaskAndThirdPartyText(t *testing.T) {
	var prepared domain.Submission
	client := testClient(t, testStore(t), baseConfig(), func(sub domain.Submission) (domain.Submission, error) {
		prepared = sub
		return sub, nil
	}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected Telegram request: %s", req.URL)
		return nil, errors.New("unexpected request")
	}))
	message := &models.Message{
		ID: 1, From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate}, Date: 1700000000,
		ForwardOrigin: &models.MessageOrigin{Type: models.MessageOriginTypeHiddenUser},
		Text:          "third-party discussion https://example.org/post",
	}
	kind, _, payload, ack, err := client.jobForUpdate(&models.Update{ID: 1, Message: message})
	if err != nil || kind != "analyze" || ack != 7 {
		t.Fatalf("jobForUpdate() = (%q, %d, %v)", kind, ack, err)
	}
	var queued domain.Submission
	if err := json.Unmarshal(payload, &queued); err != nil {
		t.Fatal(err)
	}
	if queued.Text != "" || queued.ForwardedText != message.Text || len(queued.URLs) != 1 || queued.URLs[0] != "https://example.org/post" {
		t.Fatalf("forwarded submission mixed identities: %#v", queued)
	}
	if prepared.Text != "" || prepared.ForwardedText != queued.ForwardedText {
		t.Fatalf("prepare input mixed identities: %#v", prepared)
	}
	message.ForwardOrigin = &models.MessageOrigin{
		Type: models.MessageOriginTypeUser,
		MessageOriginUser: &models.MessageOriginUser{
			SenderUser: models.User{ID: 7},
		},
	}
	kind, _, payload, _, err = client.jobForUpdate(&models.Update{ID: 2, Message: message})
	if err != nil || kind != "analyze" {
		t.Fatalf("own forwarded message job = (%q, %v)", kind, err)
	}
	queued = domain.Submission{}
	if err := json.Unmarshal(payload, &queued); err != nil {
		t.Fatal(err)
	}
	if queued.Text != message.Text || queued.ForwardedText != "" {
		t.Fatalf("own forward became third-party source: %#v", queued)
	}
}

func TestPollConfirmsGroupAndUnauthorizedUpdatesWithoutJobs(t *testing.T) {
	st := testStore(t)
	updates := `{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":7},"chat":{"id":-100,"type":"group"},"date":1,"text":"https://example.com"}},{"update_id":2,"message":{"message_id":2,"from":{"id":8},"chat":{"id":8,"type":"private"},"date":1,"text":"https://example.com"}}]}`
	var calls int
	nextPoll := make(chan struct{})
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/getUpdates") {
			calls++
			if calls == 1 {
				return apiResponse(req, http.StatusOK, updates), nil
			}
			close(nextPoll)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		t.Errorf("unauthorized/group update sent a message: %s", req.URL.Path)
		return nil, errors.New("unexpected send")
	})
	client := testClient(t, st, baseConfig(), func(sub domain.Submission) (domain.Submission, error) { return sub, nil }, transport)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Poll(ctx) }()
	select {
	case <-nextPoll:
	case <-time.After(3 * time.Second):
		t.Fatal("poller did not request next batch")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	if offset, err := st.Offset(context.Background()); err != nil || offset != 3 {
		t.Fatalf("offset = (%d, %v), want 3", offset, err)
	}
	if job, err := st.ClaimJob(context.Background()); err != nil || job != nil {
		t.Fatalf("unexpected job for ignored updates: (%+v, %v)", job, err)
	}
}

func TestSendClassifiesFailuresWithoutLeakingDetails(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		transport  error
		permanent  bool
		uncertain  bool
		retryAfter time.Duration
	}{
		{name: "rate limit", status: http.StatusOK, body: `{"ok":false,"error_code":429,"description":"slow down","parameters":{"retry_after":7}}`, retryAfter: 7 * time.Second},
		{name: "bad request", status: http.StatusOK, body: `{"ok":false,"error_code":400,"description":"bad message"}`, permanent: true},
		{name: "unauthorized", status: http.StatusOK, body: `{"ok":false,"error_code":401,"description":"bad token"}`, permanent: true},
		{name: "forbidden", status: http.StatusOK, body: `{"ok":false,"error_code":403,"description":"blocked"}`, permanent: true},
		{name: "server failure", status: http.StatusInternalServerError, body: `{"ok":false,"error_code":500}`, uncertain: true},
		{name: "network failure", transport: errors.New("request to bot123:fixture_secret timed out"), uncertain: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			var sends int
			client := testClient(t, st, baseConfig(), func(sub domain.Submission) (domain.Submission, error) { return sub, nil }, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				sends++
				if tc.transport != nil {
					return nil, tc.transport
				}
				return apiResponse(req, tc.status, tc.body), nil
			}))
			_, err := client.Send(context.Background(), 7, "plain text")
			if err == nil {
				t.Fatal("Send() unexpectedly succeeded")
			}
			var sendErr *SendError
			if !errors.As(err, &sendErr) || sendErr.Permanent != tc.permanent || sendErr.Uncertain != tc.uncertain || sendErr.RetryAfter != tc.retryAfter {
				t.Fatalf("Send() error = %#v, want permanent=%v uncertain=%v retry=%v", err, tc.permanent, tc.uncertain, tc.retryAfter)
			}
			if strings.Contains(err.Error(), "fixture_secret") || strings.Contains(err.Error(), "api.telegram.org") || strings.Contains(err.Error(), "bad token") {
				t.Fatalf("unsafe error text leaked: %q", err)
			}
			if sends != 1 {
				t.Fatalf("Send() made %d API attempts; want one", sends)
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

func TestPollRejectsUpdateWithoutPositiveIDBeforeOffsetChange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{name: "missing id", payload: `{"ok":true,"result":[{"message":{"message_id":1}}]}`},
		{name: "zero id", payload: `{"ok":true,"result":[{"update_id":0,"message":{"message_id":1}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			var called bool
			client := testClient(t, st, baseConfig(), func(sub domain.Submission) (domain.Submission, error) { return sub, nil }, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				return apiResponse(req, http.StatusOK, tc.payload), nil
			}))
			err := client.Poll(context.Background())
			if err == nil || !called {
				t.Fatalf("Poll() = %v, called=%v; want malformed update error", err, called)
			}
			if offset, err := st.Offset(context.Background()); err != nil || offset != 0 {
				t.Fatalf("offset changed after invalid update: (%d, %v)", offset, err)
			}
		})
	}
}
