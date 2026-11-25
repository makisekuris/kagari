package sqlite

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/documentarchive"
	"kagari/internal/personal"
)

func archiveUserEvent(t *testing.T, s *Store, updateID, messageID int64, fileID string) personal.Event {
	t.Helper()
	input := sourceInput(strconv.FormatInt(messageID, 10), "归档附件")
	input.Attachments = []channel.Attachment{{ID: fileID}}
	id := acceptInput(t, s, strconv.FormatInt(updateID, 10), input)
	event, err := s.ClaimEvent(context.Background())
	if err != nil || event == nil || event.ID != id {
		t.Fatalf("ClaimEvent() = (%+v, %v), want event %d", event, err, id)
	}
	return *event
}

func archiveTestFile(t *testing.T, s *Store, name, body string) channel.Attachment {
	t.Helper()
	file, err := s.SaveAttachment(context.Background(), name, "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestArchiveHistoricalInsertionPreservesLatestAndOlderSearchHit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)

	latestFile := archiveTestFile(t, s, "报价-v2.txt", "第二版报价")
	latestEvent := archiveUserEvent(t, s, 12001, 12001, latestFile.ID)
	latest, err := s.EnqueueArchive(ctx, documentarchive.Request{
		EventID: latestEvent.ID, RequestKey: "latest", AttachmentID: latestFile.ID, Title: "厨房报价",
		Number: 2, Note: "最新报价", Tags: []string{"设备"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := s.EnqueueArchive(ctx, documentarchive.Request{
		EventID: latestEvent.ID, RequestKey: "latest", AttachmentID: latestFile.ID, Title: "厨房报价",
		Number: 2, Note: "最新报价", Tags: []string{"设备"},
	}); err != nil || replay.ID != latest.ID {
		t.Fatalf("archive retry=(%+v,%v), want version %d", replay, err, latest.ID)
	}
	if _, err := s.EnqueueArchive(ctx, documentarchive.Request{
		EventID: latestEvent.ID, RequestKey: "latest", AttachmentID: latestFile.ID, Title: "厨房报价",
		Number: 2, Note: "different payload", Tags: []string{"设备"},
	}); !errors.Is(err, documentarchive.ErrConflict) {
		t.Fatalf("changed request replay error = %v, want conflict", err)
	}
	finishEvent(t, s, latestEvent, personal.TurnOutput{}, nil)

	historicalFile := archiveTestFile(t, s, "报价-v1.txt", "第一版报价")
	historicalEvent := archiveUserEvent(t, s, 12002, 12002, historicalFile.ID)
	historical, err := s.EnqueueArchive(ctx, documentarchive.Request{
		EventID: historicalEvent.ID, RequestKey: "historical", AttachmentID: historicalFile.ID,
		SourceEventID: historicalEvent.ID, DocumentID: latest.DocumentID, Historical: true, Number: 1,
		Note: "防水质保", Tags: []string{"设备"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !historical.Historical || historical.Number != 1 || historical.SourceEventID != historicalEvent.ID {
		t.Fatalf("historical version provenance = %+v", historical)
	}
	finishEvent(t, s, historicalEvent, personal.TurnOutput{}, nil)

	detail, err := s.ArchiveDocument(ctx, latest.DocumentID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Latest.ID != latest.ID || len(detail.Versions) != 2 || detail.Versions[0].ID != latest.ID || detail.Versions[1].ID != historical.ID {
		t.Fatalf("history ordering/latest = latest %d versions %+v", detail.Latest.ID, detail.Versions)
	}
	hits, err := s.SearchArchives(ctx, documentarchive.Query{Text: "防水质保"})
	if err != nil || len(hits) != 1 || hits[0].Match.ID != historical.ID || hits[0].Latest.ID != latest.ID {
		t.Fatalf("old-version search hit = (%+v,%v)", hits, err)
	}
	if !strings.Contains(hits[0].MatchedField, "note") {
		t.Fatalf("matched field = %q, want note", hits[0].MatchedField)
	}

	newestFile := archiveTestFile(t, s, "报价-v3.txt", "第三版报价")
	newestEvent := archiveUserEvent(t, s, 12003, 12003, newestFile.ID)
	newest, err := s.EnqueueArchive(ctx, documentarchive.Request{
		EventID: newestEvent.ID, RequestKey: "newest", AttachmentID: newestFile.ID,
		DocumentID: latest.DocumentID, Note: "第三版",
	})
	if err != nil {
		t.Fatal(err)
	}
	if newest.Number != 3 {
		t.Fatalf("next automatic number = %d, want 3", newest.Number)
	}
	finishEvent(t, s, newestEvent, personal.TurnOutput{}, nil)
	hits, err = s.SearchArchives(ctx, documentarchive.Query{Text: "防水质保"})
	if err != nil || len(hits) != 1 || hits[0].Match.ID != historical.ID || hits[0].Latest.ID != newest.ID {
		t.Fatalf("old-version hit after newer submission = (%+v,%v)", hits, err)
	}

	tooNewHistoryFile := archiveTestFile(t, s, "incorrect-history.txt", "invalid historical position")
	tooNewHistoryEvent := archiveUserEvent(t, s, 12004, 12004, tooNewHistoryFile.ID)
	if _, err := s.EnqueueArchive(ctx, documentarchive.Request{
		EventID: tooNewHistoryEvent.ID, RequestKey: "invalid-historical", AttachmentID: tooNewHistoryFile.ID,
		DocumentID: latest.DocumentID, Historical: true, Number: 9,
	}); !errors.Is(err, documentarchive.ErrConflict) {
		t.Fatalf("historical number after latest error = %v, want conflict", err)
	}
}

func TestArchiveWorkRetryRecoveryCASAndAttachmentGuard(t *testing.T) {
	ctx := context.Background()
	s := testStore(t, ":memory:", time.Minute)
	file := archiveTestFile(t, s, "queued.txt", "queued archive")
	event := archiveUserEvent(t, s, 12101, 12101, file.ID)
	version, err := s.EnqueueArchive(ctx, documentarchive.Request{EventID: event.ID, RequestKey: "queued", AttachmentID: file.ID, Title: "Queued"})
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, personal.TurnOutput{}, nil)
	now := time.Now().UTC()
	first, err := s.ClaimArchiveWork(ctx, now)
	if err != nil || first == nil || first.Kind != documentarchive.WorkUpload || first.Version.ID != version.ID || first.Attempt != 1 {
		t.Fatalf("first upload claim = (%+v,%v)", first, err)
	}
	if err := s.DeleteAttachment(ctx, file.ID); err == nil {
		t.Fatal("attachment deletion succeeded while upload was processing")
	}
	if err := s.RecoverArchiveWork(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimArchiveWork(ctx, now.Add(time.Second))
	if err != nil || second == nil || second.Kind != documentarchive.WorkUpload || second.Attempt != 2 {
		t.Fatalf("recovered upload claim = (%+v,%v)", second, err)
	}
	if err := s.FinishArchiveWork(ctx, *first, documentarchive.WorkResult{Status: documentarchive.StatusReady}); !errors.Is(err, documentarchive.ErrConflict) {
		t.Fatalf("stale finish = %v, want conflict", err)
	}
	retryAt := now.Add(3 * time.Second)
	if err := s.FinishArchiveWork(ctx, *second, documentarchive.WorkResult{Status: documentarchive.StatusPending, RetryAt: retryAt, Error: "temporary"}); err != nil {
		t.Fatal(err)
	}
	if work, err := s.ClaimArchiveWork(ctx, now.Add(2*time.Second)); err != nil || work != nil {
		t.Fatalf("claimed before retry time = (%+v,%v)", work, err)
	}
	third, err := s.ClaimArchiveWork(ctx, retryAt)
	if err != nil || third == nil || third.Kind != documentarchive.WorkUpload || third.Attempt != 3 {
		t.Fatalf("retry claim = (%+v,%v)", third, err)
	}
	if err := s.FinishArchiveWork(ctx, *third, documentarchive.WorkResult{Status: documentarchive.StatusFailed, Error: "terminal"}); err != nil {
		t.Fatal(err)
	}
	if work, err := s.ClaimArchiveWork(ctx, retryAt.Add(time.Second)); err != nil || work != nil {
		t.Fatalf("terminal upload left work queued = (%+v,%v)", work, err)
	}
	got, err := s.ArchiveVersion(ctx, version.ID)
	if err != nil || got.UploadStatus != documentarchive.StatusFailed || got.SummaryStatus != documentarchive.StatusFailed {
		t.Fatalf("terminal upload state = (%+v,%v)", got, err)
	}
	if err := s.DeleteAttachment(ctx, file.ID); err != nil {
		t.Fatalf("delete after terminal upload failure: %v", err)
	}
}
