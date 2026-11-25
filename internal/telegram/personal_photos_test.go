package telegram

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
	"kagari/internal/channel"
)

func TestPersonalAcceptsNativePhoto(t *testing.T) {
	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, image.NewGray(image.Rect(0, 0, 32, 24)), nil); err != nil {
		t.Fatal(err)
	}
	for _, caption := range []string{"", "What is in this image?"} {
		name := "photo only"
		if caption != "" {
			name = "with caption"
		}
		t.Run(name, func(t *testing.T) {
			files := newChannelFilesFixture()
			inbox := &channelInboxFixture{}
			client := personalClient(t, inbox, files, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/getFile"):
					if got := req.FormValue("file_id"); got != "full" {
						t.Fatalf("requested photo = %q, want full", got)
					}
					return apiResponse(req, http.StatusOK, fmt.Sprintf(`{"ok":true,"result":{"file_id":"full","file_path":"photos/image.jpg","file_size":%d}}`, photo.Len())), nil
				case strings.HasSuffix(req.URL.Path, "/photos/image.jpg"):
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(photo.Bytes()))}, nil
				default:
					t.Fatalf("unexpected Telegram request: %s", req.URL.Path)
					return nil, errors.New("unexpected request")
				}
			}))
			message := &models.Message{
				ID: 22, From: &models.User{ID: 7}, Chat: models.Chat{ID: 7, Type: models.ChatTypePrivate},
				Caption: caption, ReplyToMessage: &models.Message{ID: 21},
				Photo: []models.PhotoSize{
					{FileID: "full", Width: 32, Height: 24, FileSize: photo.Len()},
					{FileID: "preview", Width: 16, Height: 12},
				},
			}
			if err := client.acceptUpdate(t.Context(), &models.Update{ID: 1, Message: message}); err != nil {
				t.Fatal(err)
			}
			if len(inbox.inputs) != 1 {
				t.Fatalf("inputs = %#v, want one", inbox.inputs)
			}
			input := inbox.inputs[0]
			if input.Text != caption || input.ReplyToID != "21" || len(input.Attachments) != 1 {
				t.Fatalf("photo input = %#v", input)
			}
			attachment := input.Attachments[0]
			if attachment.ID == "" || attachment.Error != "" || attachment.Name != "image.jpg" || attachment.MIME != "image/jpeg" || attachment.Size != int64(photo.Len()) {
				t.Fatalf("photo attachment = %#v", attachment)
			}
			if !bytes.Equal(files.content[attachment.ID], photo.Bytes()) {
				t.Fatal("stored photo differs from the download")
			}
		})
	}
}

func TestPersonalSendsPhotosUsingTheirContentAndDimensions(t *testing.T) {
	var jpegPhoto, pngPhoto, widePhoto, tallPhoto bytes.Buffer
	if err := jpeg.Encode(&jpegPhoto, image.NewGray(image.Rect(0, 0, 32, 24)), nil); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		output *bytes.Buffer
		width  int
		height int
	}{
		{&pngPhoto, 32, 24},
		{&widePhoto, 21, 1},
		{&tallPhoto, 500, 9501},
	} {
		if err := png.Encode(sample.output, image.NewGray(image.Rect(0, 0, sample.width, sample.height))); err != nil {
			t.Fatal(err)
		}
	}
	largePhoto := make([]byte, maxPhotoBytes+1)
	copy(largePhoto, jpegPhoto.Bytes())
	for _, tc := range []struct {
		name      string
		data      []byte
		wantMIME  string
		method    string
		field     string
		uncertain bool
	}{
		{"JPEG", jpegPhoto.Bytes(), "image/jpeg", "sendPhoto", "photo", false},
		{"PNG", pngPhoto.Bytes(), "image/png", "sendPhoto", "photo", false},
		{"ratio over limit", widePhoto.Bytes(), "application/octet-stream", "sendDocument", "document", false},
		{"sum of sides over limit", tallPhoto.Bytes(), "application/octet-stream", "sendDocument", "document", false},
		{"bytes over limit", largePhoto, "application/octet-stream", "sendDocument", "document", false},
		{"non-image with image filename", []byte("not an image"), "application/octet-stream", "sendDocument", "document", false},
		{"photo missing message ID", jpegPhoto.Bytes(), "image/jpeg", "sendPhoto", "photo", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const filename = "file.jpg"
			files := newChannelFilesFixture()
			id := files.put(filename, "application/octet-stream", tc.data)
			var sends int
			client := personalClient(t, &channelInboxFixture{}, files, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				sends++
				if req.URL.Path != "/bot123:fixture_secret/"+tc.method {
					t.Fatalf("send path = %q, want %s", req.URL.Path, tc.method)
				}
				reader, err := req.MultipartReader()
				if err != nil {
					t.Fatal(err)
				}
				chat, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				chatID, err := io.ReadAll(chat)
				if err != nil || chat.FormName() != "chat_id" || string(chatID) != "7" {
					t.Fatalf("chat ID = %q, %v", chatID, err)
				}
				part, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				if part.FormName() != tc.field || part.FileName() != filename || part.Header.Get("Content-Type") != tc.wantMIME {
					t.Fatalf("multipart file field = %q, name = %q, MIME = %q", part.FormName(), part.FileName(), part.Header.Get("Content-Type"))
				}
				got, err := io.ReadAll(part)
				if err != nil || !bytes.Equal(got, tc.data) {
					t.Fatalf("uploaded bytes changed: read %d of %d bytes, %v", len(got), len(tc.data), err)
				}
				if tc.uncertain {
					return apiResponse(req, http.StatusOK, `{"ok":true,"result":{}}`), nil
				}
				return apiResponse(req, http.StatusOK, `{"ok":true,"result":{"message_id":88}}`), nil
			}))
			receipt, err := client.Send(t.Context(), channel.Content{AttachmentID: id})
			if sends != 1 {
				t.Fatalf("send calls = %d, want one", sends)
			}
			want := channel.Receipt{Channel: Channel, Account: "123", Conversation: "7"}
			if tc.uncertain {
				var sendErr *channel.SendError
				if !errors.As(err, &sendErr) || !sendErr.Uncertain || receipt != want {
					t.Fatalf("send = (%+v, %v), want uncertain outcome", receipt, err)
				}
				return
			}
			want.MessageID = "88"
			if err != nil || receipt != want {
				t.Fatalf("send = (%+v, %v)", receipt, err)
			}
		})
	}
}
