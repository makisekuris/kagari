package documentarchive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"kagari/internal/channel"
	"kagari/internal/textpage"
)

const downloadTTL = 15 * time.Minute

type Service struct {
	store          Store
	files          channel.Files
	objects        ObjectStore
	summarizeText  Summarize
	summarizeImage Summarize
}

func NewService(store Store, files channel.Files, objects ObjectStore, summarizeText, summarizeImage Summarize) *Service {
	return &Service{store: store, files: files, objects: objects, summarizeText: summarizeText, summarizeImage: summarizeImage}
}

func (s *Service) Enqueue(ctx context.Context, request Request) (Version, error) {
	return s.store.EnqueueArchive(ctx, request)
}

func (s *Service) Version(ctx context.Context, id int64) (Version, error) {
	return s.store.ArchiveVersion(ctx, id)
}

func (s *Service) Document(ctx context.Context, id int64, beforeNumber, limit int) (Detail, error) {
	return s.store.ArchiveDocument(ctx, id, beforeNumber, limit)
}

func (s *Service) Search(ctx context.Context, query Query) ([]Hit, error) {
	return s.store.SearchArchives(ctx, query)
}

func (s *Service) UpdateDocument(ctx context.Context, patch DocumentPatch) (Document, error) {
	return s.store.UpdateArchiveDocument(ctx, patch)
}

func (s *Service) UpdateSummary(ctx context.Context, eventID, versionID int64, input string) (Version, error) {
	input = strings.TrimSpace(input)
	if input == "" || !utf8.ValidString(input) || utf8.RuneCountInString(input) > maxSummaryRunes {
		return Version{}, fmt.Errorf("archive summary must contain 1 to %d characters", maxSummaryRunes)
	}
	return s.store.SetArchiveSummary(ctx, eventID, versionID, ownerSummary(input))
}

func (s *Service) DownloadLink(ctx context.Context, versionID int64) (Download, error) {
	v, err := s.Version(ctx, versionID)
	if err != nil {
		return Download{}, err
	}
	result := Download{VersionID: v.ID, UploadStatus: v.UploadStatus}
	if v.UploadStatus != StatusReady {
		return result, nil
	}
	expiresAt := time.Now().UTC().Add(downloadTTL)
	link, err := s.objects.DownloadURL(ctx, v.ObjectKey, v.Name, downloadTTL)
	if err != nil {
		return Download{}, fmt.Errorf("sign archive download: %w", err)
	}
	result.URL, result.ExpiresAt = link, expiresAt
	return result, nil
}

func (s *Service) PrepareAttachment(ctx context.Context, versionID int64) (Original, error) {
	v, err := s.Version(ctx, versionID)
	if err != nil {
		return Original{}, err
	}
	result := Original{VersionID: v.ID, UploadStatus: v.UploadStatus}
	file, err := s.files.OpenAttachment(ctx, v.AttachmentID)
	if err == nil {
		if err := file.Close(); err != nil {
			return Original{}, fmt.Errorf("close archive source: %w", err)
		}
		result.AttachmentID = v.AttachmentID
		return result, nil
	}
	if ctx.Err() != nil {
		return Original{}, ctx.Err()
	}
	if v.UploadStatus != StatusReady {
		return result, nil
	}
	data, err := s.readObject(ctx, v)
	if err != nil {
		return Original{}, fmt.Errorf("retrieve archive original: %w", err)
	}
	attachment, err := s.files.SaveAttachment(ctx, v.Name, v.MIME, bytes.NewReader(data))
	if err != nil {
		return Original{}, fmt.Errorf("prepare archive attachment: %w", err)
	}
	result.AttachmentID = attachment.ID
	return result, nil
}

func (s *Service) ReadText(ctx context.Context, versionID int64, request textpage.Request) (Text, error) {
	if _, err := request.Validate(); err != nil {
		return Text{}, err
	}
	v, err := s.Version(ctx, versionID)
	if err != nil {
		return Text{}, err
	}
	result := Text{VersionID: v.ID, SummaryState: v.SummaryStatus, Scope: v.ExtractedScope}
	if v.ExtractedText == "" {
		return result, nil
	}
	page, err := textpage.Read(v.ExtractedText, request, false)
	if err != nil {
		return Text{}, err
	}
	page.SourceTruncated = v.ExtractedPartial
	result.Page = &page
	return result, nil
}

func (s *Service) readObject(ctx context.Context, v Version) ([]byte, error) {
	file, err := s.objects.Open(ctx, v.ObjectKey)
	if err != nil {
		return nil, err
	}
	return readOriginal(file, v.Size)
}

func readOriginal(file io.ReadCloser, size int64) ([]byte, error) {
	if size < 0 || size > channel.MaxFileBytes {
		return nil, errors.Join(errors.New("invalid archive original size"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, size+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, errors.New("archive original size changed")
	}
	return data, nil
}
