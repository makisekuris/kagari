package documentarchive_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"kagari/internal/channel"
	"kagari/internal/documentarchive"
	"kagari/internal/personal/sqlite"
	"kagari/internal/textpage"
)

func TestArchiveService(t *testing.T) {
	for _, channelName := range []string{"telegram", "weixin"} {
		t.Run(channelName, func(t *testing.T) {
			objects := memoryObjects{}
			exerciseArchive(t, channelName, objects, func(key string) {
				t.Cleanup(func() { delete(objects, key) })
			}, nil)
		})
	}
}

func TestArchiveWithRustFS(t *testing.T) {
	if os.Getenv("KAGARI_ARCHIVE_LIVE_TEST") != "1" {
		t.Skip("set KAGARI_ARCHIVE_LIVE_TEST=1 and configure a RustFS test bucket")
	}
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
	cleanupClient := s3.NewFromConfig(aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	exerciseArchive(t, "telegram", objects, func(key string) {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := cleanupClient.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key}); err != nil {
				t.Errorf("could not remove the RustFS test object %s", key)
			}
		})
	}, func(ctx context.Context, link, expected string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if err != nil {
			t.Fatal("invalid signed download URL")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("signed download HTTP request failed")
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(len(expected))+1))
		if err != nil || resp.StatusCode != http.StatusOK || string(body) != expected {
			t.Fatalf("signed download could not retrieve the original bytes, status %d", resp.StatusCode)
		}
	})
}

func exerciseArchive(t *testing.T, channelName string, objects documentarchive.ObjectStore, cleanup func(string), download func(context.Context, string, string)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sqlite.NewStore(filepath.Join(t.TempDir(), "archive.db"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.BindChannel(ctx, channelName, "bot", "owner", false); err != nil {
		t.Fatal(err)
	}
	const body = "厨房报价含防水质保，合同期五年。"
	file, err := db.SaveAttachment(ctx, "厨房报价.txt", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := db.AcceptBatch(ctx, channelName, "bot", "2", []channel.Input{{
		Channel: channelName, Account: "bot", Sender: "owner", MessageID: "1", Text: "归档这份报价", Attachments: []channel.Attachment{file},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if event, err := db.ClaimEvent(ctx); err != nil || event == nil || event.ID != ids[0] {
		t.Fatalf("claim archive request event: %v", err)
	}
	archive := documentarchive.NewService(db, db, objects, func(_ context.Context, sample documentarchive.Sample) (string, error) {
		if sample.Text != body {
			return "", errors.New("summarizer did not receive the archived text")
		}
		return "防水质保五年。", nil
	}, nil)
	request := documentarchive.Request{EventID: ids[0], RequestKey: "archive-integration", AttachmentID: file.ID, Title: "厨房报价"}
	version, err := archive.Enqueue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(version.ObjectKey)
	if repeat, err := archive.Enqueue(ctx, request); err != nil || version.ID != repeat.ID {
		t.Fatalf("archive retry did not preserve the version: %v", err)
	}
	if original, err := archive.PrepareAttachment(ctx, version.ID); err != nil || original.AttachmentID != file.ID || original.UploadStatus != documentarchive.StatusPending {
		t.Fatalf("original should be available from the channel before upload: %#v %v", original, err)
	}
	if pending, err := archive.DownloadLink(ctx, version.ID); err != nil || pending.URL != "" || pending.UploadStatus != documentarchive.StatusPending {
		t.Fatal("archive exposed a link before the original was uploaded")
	}
	if worked, err := archive.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("upload work: %v", err)
	}
	link, err := archive.DownloadLink(ctx, version.ID)
	if err != nil || link.URL == "" || link.UploadStatus != documentarchive.StatusReady {
		status, _ := archive.Version(ctx, version.ID)
		t.Fatalf("expected a signed link before summary completion, upload status %s: %v", status.UploadStatus, err)
	}
	if download != nil {
		download(ctx, link.URL, body)
	}
	if worked, err := archive.ProcessNext(ctx); err != nil || !worked {
		t.Fatalf("summary work: %v", err)
	}
	if current, err := archive.Version(ctx, version.ID); err != nil || !current.Archived() || current.SummarySource != documentarchive.SummaryFromOriginal || current.SummaryScope == "" {
		t.Fatalf("archive did not complete with a document summary: %#v %v", current, err)
	}
	hits, err := archive.Search(ctx, documentarchive.Query{Text: "防水质保"})
	if err != nil || len(hits) != 1 || hits[0].Match.ID != version.ID || hits[0].Latest.ID != version.ID {
		t.Fatalf("archive summary search: %#v %v", hits, err)
	}
	text, err := archive.ReadText(ctx, version.ID, textpage.Request{})
	if err != nil || text.Page == nil || text.Page.Text != body || text.Page.SourceTruncated {
		t.Fatalf("read extracted text: %#v %v", text, err)
	}
	if err := db.DeleteAttachment(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	original, err := archive.PrepareAttachment(ctx, version.ID)
	if err != nil || original.AttachmentID == "" || original.AttachmentID == file.ID {
		t.Fatalf("failed to prepare the archive after deleting the local attachment: %#v %v", original, err)
	}
	got, err := db.OpenAttachment(ctx, original.AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	gotBody, readErr := io.ReadAll(got)
	if err := errors.Join(readErr, got.Close()); err != nil || string(gotBody) != body {
		t.Fatal("archive did not restore the original bytes")
	}
	newTitle := "厨房施工报价"
	if doc, err := archive.UpdateDocument(ctx, documentarchive.DocumentPatch{EventID: ids[0], DocumentID: version.DocumentID, Title: &newTitle}); err != nil || doc.Title != newTitle {
		t.Fatalf("archive title edit: %#v %v", doc, err)
	}
	if corrected, err := archive.UpdateSummary(ctx, ids[0], version.ID, "使用者核对后的厨房报价说明"); err != nil || corrected.SummarySource != documentarchive.SummaryFromOwner || corrected.ID != version.ID {
		t.Fatalf("archive summary correction: %#v %v", corrected, err)
	}
	text, err = archive.ReadText(ctx, version.ID, textpage.Request{})
	if err != nil || text.Page == nil || text.Page.Text != body || text.Scope != "UTF-8 正文" {
		t.Fatalf("summary correction changed the document evidence: %#v %v", text, err)
	}
	if detail, err := archive.Document(ctx, version.DocumentID, 0, 10); err != nil || len(detail.Versions) != 1 || detail.Latest.ID != version.ID {
		t.Fatalf("metadata edit created a new version: %#v %v", detail, err)
	}
}

type memoryObjects map[string][]byte

func (m memoryObjects) Put(_ context.Context, key string, body io.ReadSeeker, _ int64, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m[key] = data
	return nil
}

func (m memoryObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := m[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m memoryObjects) DownloadURL(context.Context, string, string, time.Duration) (string, error) {
	return "https://archive.example.invalid/download", nil
}
