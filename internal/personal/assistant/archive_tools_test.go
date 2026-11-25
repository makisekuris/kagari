package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"kagari/internal/channel"
	"kagari/internal/documentarchive"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
)

type archiveToolObjects struct {
	objects   map[string][]byte
	signedURL string
}

func newArchiveToolObjects() *archiveToolObjects {
	return &archiveToolObjects{objects: make(map[string][]byte), signedURL: "https://archive.example/download?signature=private-signature"}
}

func (s *archiveToolObjects) Put(_ context.Context, key string, body io.ReadSeeker, size int64, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return io.ErrUnexpectedEOF
	}
	s.objects[key] = data
	return nil
}

func (s *archiveToolObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, io.EOF
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *archiveToolObjects) DownloadURL(context.Context, string, string, time.Duration) (string, error) {
	return s.signedURL, nil
}

func openArchiveUserEvent(t *testing.T, store *sqlite.Store, message string, files ...channel.Attachment) personal.Event {
	t.Helper()
	input := testkit.SourceInput("101", message)
	input.Attachments = append([]channel.Attachment(nil), files...)
	if _, err := store.AcceptBatch(context.Background(), input.Channel, input.Account, "100", []personal.Input{input}); err != nil {
		t.Fatal(err)
	}
	event, err := store.ClaimEvent(context.Background())
	if err != nil || event == nil {
		t.Fatalf("claim archive event: event=%+v err=%v", event, err)
	}
	return *event
}

func archiveToolsForEvent(t *testing.T, store *sqlite.Store, event personal.Event, objects documentarchive.ObjectStore) (*archiveTurnTools, []tool.BaseTool) {
	t.Helper()
	input := personal.TurnInput{Event: event}
	actions := &personalTools{store: store, event: event, input: input, taskIDs: make(map[int64]struct{})}
	service := documentarchive.NewService(store, store, objects, nil, nil)
	archive := newArchiveTurnTools(service, store, input, actions, nil)
	registered, err := archive.build()
	if err != nil {
		t.Fatal(err)
	}
	return archive, registered
}

func TestArchiveToolsKeepReceiptAndPendingOriginalDeliveryHonest(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Hour)
	file, err := store.SaveAttachment(ctx, "warranty.txt", "text/plain", strings.NewReader("Warranty file contents"))
	if err != nil {
		t.Fatal(err)
	}
	event := openArchiveUserEvent(t, store, "Archive this warranty and send me the file.", file)
	objects := newArchiveToolObjects()
	archive, registered := archiveToolsForEvent(t, store, event, objects)
	args := map[string]any{"attachment_id": file.ID}
	firstRaw := invokeNamedTool(t, registered, "archive_attachment", args)
	var first archiveToolReply
	if err := json.Unmarshal([]byte(firstRaw), &first); err != nil {
		t.Fatalf("decode archive result %q: %v", firstRaw, err)
	}
	if !first.OK || first.Version == nil || first.Version.Archived || first.Version.UploadStatus != documentarchive.StatusPending || first.Version.SummaryStatus != documentarchive.StatusPending {
		t.Fatalf("archive result did not report a pending version honestly: %+v", first)
	}
	secondRaw := invokeNamedTool(t, registered, "archive_attachment", args)
	var second archiveToolReply
	if err := json.Unmarshal([]byte(secondRaw), &second); err != nil {
		t.Fatalf("decode retried archive result %q: %v", secondRaw, err)
	}
	if second.Version == nil || second.Version.ID != first.Version.ID {
		t.Fatalf("same-event retry created another version: first=%+v second=%+v", first.Version, second.Version)
	}
	var listed archiveToolReply
	if err := json.Unmarshal([]byte(invokeNamedTool(t, registered, "search_archives", map[string]any{})), &listed); err != nil {
		t.Fatalf("decode unfiltered archive listing: %v", err)
	}
	if !listed.OK || len(listed.Hits) != 1 || listed.Hits[0].Latest.ID != first.Version.ID || listed.Hits[0].Latest.Archived {
		t.Fatalf("unfiltered listing lost the independent latest status: %+v", listed)
	}
	stored, err := archive.service.Version(ctx, first.Version.ID)
	if err != nil || stored.SourceEventID != event.ID {
		t.Fatalf("archive source event = %d, err=%v; want %d", stored.SourceEventID, err, event.ID)
	}

	preparedRaw := invokeNamedTool(t, registered, "prepare_archive_attachment", map[string]any{"version_id": first.Version.ID})
	var prepared archiveToolReply
	if err := json.Unmarshal([]byte(preparedRaw), &prepared); err != nil {
		t.Fatalf("decode prepared original %q: %v", preparedRaw, err)
	}
	if !prepared.OK || prepared.Original == nil || prepared.Original.AttachmentID != file.ID || prepared.Original.UploadStatus != documentarchive.StatusPending {
		t.Fatalf("chat original should be deliverable while upload is pending: %+v", prepared)
	}
	reply := archive.finalizeReply(personalTurnResponse{ReplyKind: personal.ReplySilent, Attachments: []string{}})
	if reply.ReplyKind != personal.ReplyContent || !strings.Contains(reply.Text, "已登记") || strings.Contains(reply.Text, "已归档") {
		t.Fatalf("archive receipt hid or overstated the pending state: %+v", reply)
	}
	if len(reply.Attachments) != 1 || reply.Attachments[0] != file.ID {
		t.Fatalf("explicit send request did not select the pending original: %v", reply.Attachments)
	}
}

func TestArchiveDownloadURLStaysOutOfToolResultAndPresentation(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Hour)
	file, err := store.SaveAttachment(ctx, "record.txt", "text/plain", strings.NewReader("record body"))
	if err != nil {
		t.Fatal(err)
	}
	event := openArchiveUserEvent(t, store, "Archive this record and provide its download link.", file)
	objects := newArchiveToolObjects()
	archive, registered := archiveToolsForEvent(t, store, event, objects)
	var result archiveToolReply
	if err := json.Unmarshal([]byte(invokeNamedTool(t, registered, "archive_attachment", map[string]any{"attachment_id": file.ID})), &result); err != nil {
		t.Fatal(err)
	}
	if result.Version == nil {
		t.Fatalf("archive response has no version: %+v", result)
	}
	if worked, err := archive.service.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("process upload: worked=%v err=%v", worked, err)
	}
	linkRaw := invokeNamedTool(t, registered, "get_archive_download_link", map[string]any{"version_id": result.Version.ID})
	if strings.Contains(linkRaw, objects.signedURL) || strings.Contains(linkRaw, "private-signature") {
		t.Fatalf("signed URL leaked into model-visible tool output: %s", linkRaw)
	}
	var linkReply archiveToolReply
	if err := json.Unmarshal([]byte(linkRaw), &linkReply); err != nil {
		t.Fatalf("decode download tool result %q: %v", linkRaw, err)
	}
	if linkReply.Download == nil || !linkReply.Download.Ready || linkReply.Download.ExpiresAt.IsZero() {
		t.Fatalf("download readiness/expiry missing from tool result: %+v", linkReply)
	}
	response := archive.finalizeReply(personalTurnResponse{Text: "下载链接已准备好。", ReplyKind: personal.ReplyContent})
	if !strings.Contains(response.Text, objects.signedURL) || !response.SkipReplyPresentation || response.ReplyKind != personal.ReplyContent {
		t.Fatalf("signed URL was not appended safely after generation: %+v", response)
	}
	if strings.Contains(response.Text, "private-signature") == false {
		t.Fatal("final reply omitted the actual signed URL")
	}
}

func TestArchiveAttachmentRejectsIDOutsideKnownUserContext(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Hour)
	known, err := store.SaveAttachment(ctx, "known.txt", "text/plain", strings.NewReader("known"))
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := store.SaveAttachment(ctx, "unrelated.txt", "text/plain", strings.NewReader("unrelated"))
	if err != nil {
		t.Fatal(err)
	}
	event := openArchiveUserEvent(t, store, "Please archive my attachment.", known)
	_, registered := archiveToolsForEvent(t, store, event, newArchiveToolObjects())
	raw := invokeNamedTool(t, registered, "archive_attachment", map[string]any{"attachment_id": unrelated.ID})
	var result archiveToolReply
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode rejection %q: %v", raw, err)
	}
	if result.OK || !strings.Contains(result.Error, "conversation context") {
		t.Fatalf("archive accepted an unrelated attachment ID: %+v", result)
	}
	if strings.Contains(result.Error, unrelated.ID) {
		t.Fatal(fmt.Sprintf("error unexpectedly exposed the unrelated ID: %s", result.Error))
	}
}

func TestArchiveReceiptRequiresBothOriginalAndSummary(t *testing.T) {
	version := documentarchive.Version{DocumentID: 3, ID: 8, Number: 2, UploadStatus: documentarchive.StatusReady, SummaryStatus: documentarchive.StatusPending}
	if version.Archived() || !strings.Contains(archiveReceipt(version), "已登记") || strings.Contains(archiveReceipt(version), "已归档") {
		t.Fatalf("upload-only readiness was reported as archived: %q", archiveReceipt(version))
	}
	version.SummaryStatus = documentarchive.StatusReady
	if !version.Archived() || !strings.Contains(archiveReceipt(version), "已归档") {
		t.Fatalf("fully ready version was not reported as archived: %q", archiveReceipt(version))
	}
}
