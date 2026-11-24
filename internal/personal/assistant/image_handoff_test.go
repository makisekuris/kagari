package assistant

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

func TestImageHandoffReturnsObservationToTextOnlyChat(t *testing.T) {
	ctx := t.Context()
	store := testkit.Store(t, ":memory:", time.Minute)
	const png = testkit.OnePixelPNG
	data, err := base64.StdEncoding.DecodeString(png)
	if err != nil {
		t.Fatal(err)
	}
	image, err := store.SaveAttachment(ctx, "screen.png", "image/png", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	input := testkit.SourceInput("10", "Read the error code in this screenshot.")
	input.Attachments = []channel.Attachment{image}
	testkit.AcceptInput(t, store, "10", input)
	event, err := store.ClaimEvent(ctx)
	if err != nil || event == nil {
		t.Fatalf("claim image event: %v", err)
	}
	const observation = "E7053"
	chat := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("analyze_image", map[string]string{"id": image.ID, "question": input.Text}, "inspect"),
		testkit.ResponseText(`{"text":"E7053","reply_kind":"content","attachments":[]}`),
	}}
	vision := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{testkit.ResponseText(observation)}}
	a := newMainAgent(t, chat, store)
	a.MultimodalModel = vision.RawModel(t)
	out, err := a.Think(ctx, personal.TurnInput{Event: *event})
	if err != nil || out.Text != observation || len(chat.Requests) != 2 || len(vision.Requests) != 1 {
		t.Fatalf("answer=%q err=%v chat calls=%d vision calls=%d", out.Text, err, len(chat.Requests), len(vision.Requests))
	}
	var advertised bool
	for _, raw := range chat.Requests[0]["tools"].([]any) {
		if raw.(map[string]any)["name"] == "analyze_image" {
			advertised = true
		}
	}
	if !advertised || !strings.Contains(testkit.RequestInput(chat.Requests[1]), observation) {
		t.Fatal("chat must receive the tool schema, then the delegated observation")
	}
	for _, request := range chat.Requests {
		raw, _ := json.Marshal(request)
		if strings.Contains(string(raw), png) {
			t.Fatal("chat received the original image")
		}
	}
	visualInput := testkit.RequestInput(vision.Requests[0])
	if !strings.Contains(visualInput, png) || !strings.Contains(visualInput, input.Text) || strings.Contains(visualInput, a.Profile) {
		t.Fatal("visual request must carry the image and question in its own context")
	}
}
