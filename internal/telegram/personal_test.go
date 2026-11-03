package telegram

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/personal"
)

type memoryPersonalInbox struct {
	mu          sync.Mutex
	offset      int64
	inputs      []*personal.Input
	updateIDs   []int64
	acceptError error
}

func (i *memoryPersonalInbox) Offset(context.Context) (int64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.offset, nil
}

func (i *memoryPersonalInbox) AcceptInput(_ context.Context, updateID int64, input *personal.Input) (int64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.acceptError != nil {
		return 0, i.acceptError
	}
	i.updateIDs = append(i.updateIDs, updateID)
	if input == nil {
		i.inputs = append(i.inputs, nil)
	} else {
		copy := *input
		i.inputs = append(i.inputs, &copy)
	}
	if updateID >= i.offset {
		i.offset = updateID + 1
	}
	return 0, nil
}

func (i *memoryPersonalInbox) snapshot() (int64, []int64, []*personal.Input) {
	i.mu.Lock()
	defer i.mu.Unlock()
	inputs := append([]*personal.Input(nil), i.inputs...)
	return i.offset, append([]int64(nil), i.updateIDs...), inputs
}

func personalConfig() config.Telegram {
	return config.Telegram{Token: "123:fixture_secret", AllowedUserIDs: []int64{7}}
}

func personalClient(t *testing.T, inbox PersonalInbox, transport http.RoundTripper) *PersonalClient {
	t.Helper()
	client, err := newPersonalClient(personalConfig(), inbox, zap.NewNop(), "https://telegram.test", transport)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestNewPersonalRequiresExactlyOneOwnerAndNoTargets(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Telegram
	}{
		{name: "no owner", cfg: config.Telegram{Token: "123:fixture_secret"}},
		{name: "multiple owners", cfg: config.Telegram{Token: "123:fixture_secret", AllowedUserIDs: []int64{7, 8}}},
		{name: "nonpositive owner", cfg: config.Telegram{Token: "123:fixture_secret", AllowedUserIDs: []int64{0}}},
		{name: "target chats", cfg: config.Telegram{Token: "123:fixture_secret", AllowedUserIDs: []int64{7}, TargetChatIDs: []int64{7}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newPersonalClient(tc.cfg, &memoryPersonalInbox{}, zap.NewNop(), "https://telegram.test", nil); err == nil {
				t.Fatal("newPersonalClient() succeeded with invalid personal configuration")
			}
		})
	}
	if _, err := newPersonalClient(personalConfig(), nil, zap.NewNop(), "https://telegram.test", nil); err == nil {
		t.Fatal("newPersonalClient() succeeded without an inbox")
	}
}

func TestPersonalPollAcceptsOnlyOwnersPrivateChatAndAdvancesOffset(t *testing.T) {
	updates := `{"ok":true,"result":[` +
		`{"update_id":1,"message":{"message_id":11,"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"owner"}},` +
		`{"update_id":2,"message":{"message_id":12,"from":{"id":7},"chat":{"id":-100,"type":"group"},"text":"group"}},` +
		`{"update_id":3,"message":{"message_id":13,"from":{"id":8},"chat":{"id":8,"type":"private"},"text":"other user"}},` +
		`{"update_id":4,"message":{"message_id":14,"from":{"id":7},"chat":{"id":9,"type":"private"},"text":"wrong private chat"}}]}`
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
		if got := req.URL.Query().Get("offset"); got != "5" {
			t.Errorf("next offset = %q, want 5", got)
		}
		close(nextPoll)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	inbox := &memoryPersonalInbox{}
	client := personalClient(t, inbox, transport)
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
	offset, ids, inputs := inbox.snapshot()
	if offset != 5 || len(ids) != 4 || len(inputs) != 4 {
		t.Fatalf("inbox = offset %d, IDs %v, inputs %#v; want offset 5 and four acknowledged updates", offset, ids, inputs)
	}
	if inputs[0] == nil || inputs[0].Text != "owner" {
		t.Fatalf("owner private message = %#v", inputs[0])
	}
	for i := 1; i < len(inputs); i++ {
		if inputs[i] != nil {
			t.Fatalf("ignored update %d created input %#v", ids[i], inputs[i])
		}
	}
}

func TestPersonalInputKeepsReplyForwardAndEntityURL(t *testing.T) {
	inbox := &memoryPersonalInbox{}
	client := personalClient(t, inbox, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected Telegram request: %s", req.URL)
		return nil, errors.New("unexpected request")
	}))
	updates := []*models.Update{
		{ID: 1, Message: &models.Message{
			ID: 41, From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate},
			Caption: "请看这篇文章", CaptionEntities: []models.MessageEntity{{Type: models.MessageEntityTypeTextLink, Offset: 3, Length: 4, URL: "https://Example.com/post#section"}},
			ReplyToMessage: &models.Message{ID: 40},
			ForwardOrigin:  &models.MessageOrigin{Type: models.MessageOriginTypeHiddenUser},
		}},
		{ID: 2, Message: &models.Message{
			ID: 42, From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate}, Text: "我转发的内容",
			ReplyToMessage: &models.Message{ID: 41},
			ForwardOrigin: &models.MessageOrigin{Type: models.MessageOriginTypeUser, MessageOriginUser: &models.MessageOriginUser{
				SenderUser: models.User{ID: 7},
			}},
		}},
	}
	for _, update := range updates {
		if err := client.acceptUpdate(context.Background(), update); err != nil {
			t.Fatal(err)
		}
	}
	_, _, inputs := inbox.snapshot()
	if len(inputs) != 2 || inputs[0] == nil || inputs[1] == nil {
		t.Fatalf("accepted inputs = %#v", inputs)
	}
	if inputs[0].MessageID != 41 || inputs[0].ReplyToID != 40 || inputs[0].Text != "请看这篇文章\nhttps://example.com/post" || !inputs[0].Material {
		t.Fatalf("third-party forwarded caption input = %#v", inputs[0])
	}
	if inputs[1].MessageID != 42 || inputs[1].ReplyToID != 41 || inputs[1].Text != "我转发的内容" || inputs[1].Material {
		t.Fatalf("owner-forwarded text input = %#v", inputs[1])
	}
}

func TestPersonalPollRetriesAPIAndAdvancesMalformedUpdate(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	nextPoll := make(chan struct{})
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if !strings.HasSuffix(req.URL.Path, "/getUpdates") {
			t.Errorf("unexpected Telegram request: %s", req.URL.Path)
			return nil, errors.New("unexpected request")
		}
		if call == 1 {
			return apiResponse(req, http.StatusInternalServerError, `{"ok":false,"error_code":500}`), nil
		}
		if call == 2 {
			if got := req.URL.Query().Get("offset"); got != "0" {
				t.Errorf("retry offset = %q, want 0", got)
			}
			return apiResponse(req, http.StatusOK, `{"ok":true,"result":[{"update_id":5,"message":{"forward_origin":{"type":"unknown"}}}]}`), nil
		}
		if got := req.URL.Query().Get("offset"); got != "6" {
			t.Errorf("offset after malformed update = %q, want 6", got)
		}
		close(nextPoll)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	inbox := &memoryPersonalInbox{}
	client := personalClient(t, inbox, transport)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Poll(ctx) }()
	select {
	case <-nextPoll:
	case <-time.After(5 * time.Second):
		t.Fatal("personal poller did not retry and advance malformed update")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Poll() = %v", err)
	}
	offset, ids, inputs := inbox.snapshot()
	if offset != 6 || len(ids) != 1 || ids[0] != 5 || len(inputs) != 1 || inputs[0] != nil {
		t.Fatalf("malformed update state = offset %d IDs %v inputs %#v", offset, ids, inputs)
	}
}

func TestPersonalPollLeavesOffsetOnInboxFailureAndRetriesIt(t *testing.T) {
	inboxFailure := errors.New("write failed")
	inbox := &memoryPersonalInbox{acceptError: inboxFailure}
	var mu sync.Mutex
	calls := 0
	nextPoll := make(chan struct{})
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if !strings.HasSuffix(req.URL.Path, "/getUpdates") {
			t.Errorf("unexpected Telegram request: %s", req.URL.Path)
			return nil, errors.New("unexpected request")
		}
		if call == 1 {
			if got := req.URL.Query().Get("offset"); got != "0" {
				t.Errorf("initial offset = %q, want 0", got)
			}
			return apiResponse(req, http.StatusOK, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":2,"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"owner"}}]}`), nil
		}
		if call == 2 {
			if got := req.URL.Query().Get("offset"); got != "0" {
				t.Errorf("retry offset = %q, want 0", got)
			}
			inbox.mu.Lock()
			inbox.acceptError = nil
			inbox.mu.Unlock()
			return apiResponse(req, http.StatusOK, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":2,"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"owner"}}]}`), nil
		}
		if got := req.URL.Query().Get("offset"); got != "2" {
			t.Errorf("next offset = %q, want 2", got)
		}
		close(nextPoll)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	client := personalClient(t, inbox, transport)
	if err := client.Poll(context.Background()); err == nil || err.Error() != "Telegram update could not be saved" {
		t.Fatalf("Poll() with inbox failure = %v", err)
	}
	if offset, _, _ := inbox.snapshot(); offset != 0 {
		t.Fatalf("offset after failed accept = %d, want 0", offset)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Poll(ctx) }()
	select {
	case <-nextPoll:
	case <-time.After(3 * time.Second):
		t.Fatal("poller did not retry the uncommitted update")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("retry Poll() = %v", err)
	}
	if offset, ids, inputs := inbox.snapshot(); offset != 2 || len(ids) != 1 || ids[0] != 1 || len(inputs) != 1 || inputs[0] == nil {
		t.Fatalf("retried inbox state = offset %d IDs %v inputs %#v", offset, ids, inputs)
	}
}

func TestPersonalSendDelegatesToSharedTelegramSender(t *testing.T) {
	var sends int
	client := personalClient(t, &memoryPersonalInbox{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends++
		if req.URL.Path != "/bot123:fixture_secret/sendMessage" {
			t.Fatalf("send path = %q", req.URL.Path)
		}
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if req.FormValue("chat_id") != "7" || req.FormValue("text") != "one outgoing message" {
			t.Errorf("sent chat/text = %q/%q", req.FormValue("chat_id"), req.FormValue("text"))
		}
		return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"message_id":88,"date":1700000004,"chat":{"id":7,"type":"private"}}}`), nil
	}))
	if _, err := client.Send(context.Background(), 8, domain.Content{Text: "must stay private"}); err == nil {
		t.Fatal("PersonalClient.Send() accepted a non-owner destination")
	}
	id, err := client.Send(context.Background(), 7, domain.Content{Text: "one outgoing message"})
	if err != nil || id != 88 || sends != 1 {
		t.Fatalf("PersonalClient.Send() = (%d, %v), API calls %d", id, err, sends)
	}
}
