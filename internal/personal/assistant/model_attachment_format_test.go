package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

type replyAttachmentRepository struct {
	Repository
	files map[string]channel.Attachment
}

func (r *replyAttachmentRepository) SaveAttachment(_ context.Context, name, mime string, contents io.Reader) (channel.Attachment, error) {
	size, err := io.Copy(io.Discard, contents)
	if err != nil {
		return channel.Attachment{}, err
	}
	id := fmt.Sprintf("created-%d", len(r.files)+1)
	file := channel.Attachment{ID: id, Name: personal.SafeAttachmentName(name), MIME: "text/plain", Size: size}
	r.files[id] = file
	return file, nil
}

func (r *replyAttachmentRepository) Attachment(ctx context.Context, id string) (channel.Attachment, error) {
	if file, ok := r.files[id]; ok {
		return file, nil
	}
	return r.Repository.Attachment(ctx, id)
}
func TestThinkFormatsNewAttachmentDeliveryFromLatestCandidate(t *testing.T) {
	ctx := context.Background()
	base := testkit.Store(t, filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	event := testkit.OpenUserEvent(t, base, 1, 1, "Create e2e-example-domains.txt and send it. PRIVATE_USER_HISTORY.")
	repo := &replyAttachmentRepository{Repository: base, files: make(map[string]channel.Attachment)}
	const firstBody = "first version body"
	const latestBody = "latest version body"
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("write_attachment", map[string]string{"name": "e2e-example-domains.txt", "text": firstBody}, "write-first"),
		testkit.ResponseTool("write_attachment", map[string]string{"name": "e2e-example-domains.txt", "text": latestBody}, "write-latest"),
		testkit.ResponseText("文件 e2e-example-domains.txt 已发给你。"),
		testkit.ResponseText(`{"text":"文件会随这条回复附上。","reply_kind":"content","attachments":["created-2"]}`),
	}}
	agent := newMainAgent(t, fixture, repo)
	agent.Persona, agent.Profile = "PRIVATE_PERSONA", "PRIVATE_PROFILE"
	output, err := agent.Think(ctx, personal.TurnInput{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.Requests) != 4 || len(repo.files) != 2 {
		t.Fatalf("requests=%d created files=%d, want two writes, one draft, and one formatter request", len(fixture.Requests), len(repo.files))
	}
	if len(output.Attachments) != 1 || output.Attachments[0] != "created-2" || output.Text != "文件会随这条回复附上。" {
		t.Fatalf("Think output=%+v, want latest candidate attached without claiming prior delivery", output)
	}
	request := fixture.Requests[3]
	assertStrictSchema(t, request, "personal_turn")
	if tools, _ := request["tools"].([]any); len(tools) != 0 {
		t.Fatalf("formatter received %d tools", len(tools))
	}
	input := testkit.RequestInput(request)
	for _, id := range []string{"created-1", "created-2"} {
		if !strings.Contains(input, id) {
			t.Errorf("formatter did not receive created attachment ID %q: %s", id, input)
		}
	}
	if !strings.Contains(input, "e2e-example-domains.txt") || !strings.Contains(input, "created_attachments") {
		t.Fatalf("formatter did not receive created attachment names: %s", input)
	}
	encodedRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{firstBody, latestBody, "PRIVATE_PERSONA", "PRIVATE_PROFILE", "PRIVATE_USER_HISTORY"} {
		if strings.Contains(string(encodedRequest), private) {
			t.Errorf("formatter received private or full attachment data %q", private)
		}
	}
}

func TestThinkDoesNotDeliverAnAttachmentThatWasOnlySaved(t *testing.T) {
	ctx := context.Background()
	base := testkit.Store(t, filepath.Join(t.TempDir(), "agent.db"), time.Minute)
	event := testkit.OpenUserEvent(t, base, 1, 1, "Save e2e-example-domains.txt, but do not send it yet.")
	repo := &replyAttachmentRepository{Repository: base, files: make(map[string]channel.Attachment)}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("write_attachment", map[string]string{"name": "e2e-example-domains.txt", "text": "saved body"}, "write-file"),
		testkit.ResponseText("已保存 e2e-example-domains.txt。"),
		testkit.ResponseText(`{"text":"文件已保存。","reply_kind":"content","attachments":[]}`),
	}}
	output, err := newMainAgent(t, fixture, repo).Think(ctx, personal.TurnInput{Event: event})
	if err != nil || len(output.Attachments) != 0 {
		t.Fatalf("saved-only output=%+v error=%v", output, err)
	}
	if len(repo.files) != 1 || len(fixture.Requests) != 3 {
		t.Fatalf("created files=%d requests=%d, want one save and one formatter request", len(repo.files), len(fixture.Requests))
	}
	if !strings.Contains(testkit.RequestInput(fixture.Requests[2]), "created-1") {
		t.Fatal("formatter did not receive the saved file as a candidate")
	}
}

func TestSelectReplySkipsFormatterForAlreadyFormattedResponse(t *testing.T) {
	fixture := &testkit.ResponsesFixture{T: t}
	agent := &MainAgent{Model: fixture.Model(t)}
	response, err := agent.selectReply(context.Background(), personal.TurnInput{Event: personal.Event{Kind: personal.EventUser, Text: "Hello"}}, &personalTools{}, `{"text":"Done.","reply_kind":"content","attachments":["existing-id"]}`, nil, nil)
	if err != nil || response.Text != "Done." || len(response.Attachments) != 1 || response.Attachments[0] != "existing-id" {
		t.Fatalf("response=%+v error=%v", response, err)
	}
	if len(fixture.Requests) != 0 {
		t.Fatalf("already formatted response triggered %d formatter requests", len(fixture.Requests))
	}
}

func TestLivePersonalFormatterSelectsOnlyRequestedAttachmentCandidate(t *testing.T) {
	engine := testkit.LiveEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := testkit.Store(t, filepath.Join(t.TempDir(), "formatter.db"), time.Minute)
	first, err := s.SaveAttachment(ctx, "e2e-example-domains.txt", "text/plain; charset=utf-8", strings.NewReader("first version"))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.SaveAttachment(ctx, "e2e-example-domains.txt", "text/plain; charset=utf-8", strings.NewReader("latest version"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == latest.ID {
		t.Fatal("same-name updates must create distinct immutable attachments")
	}
	agent := &MainAgent{Model: engine.Model}
	candidates := []channel.Attachment{first, latest}

	t.Run("explicit delivery by filename selects latest", func(t *testing.T) {
		response, err := agent.formatTurn(ctx, "Please send e2e-example-domains.txt with this reply.", candidates)
		if err != nil || response.ReplyKind != personal.ReplyContent || len(response.Attachments) != 1 || response.Attachments[0] != latest.ID {
			t.Fatalf("formatter response=%+v error=%v, want latest candidate %q", response, err, latest.ID)
		}
	})

	t.Run("saved-only draft selects none", func(t *testing.T) {
		response, err := agent.formatTurn(ctx, "e2e-example-domains.txt was saved for later.", candidates)
		if err != nil || len(response.Attachments) != 0 {
			t.Fatalf("saved-only formatter response=%+v error=%v", response, err)
		}
	})
}
