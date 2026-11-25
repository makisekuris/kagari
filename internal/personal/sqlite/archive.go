package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"kagari/internal/documentarchive"
)

const (
	maxArchiveTitleChars    = 200
	maxArchiveCategoryChars = 100
	maxArchiveTagCount      = 50
	maxArchiveTagChars      = 100
	maxArchiveNoteChars     = 4000
	maxArchivePageSize      = 100
	defaultArchivePageSize  = 20
)

const archiveVersionColumns = `v.id,v.document_id,v.number,v.historical,v.attachment_id,v.name,v.mime,v.size,v.object_key,
	v.event_id,v.source_event_id,v.submitted_at,v.note,v.upload_status,v.summary_status,v.summary,v.summary_source,v.summary_scope,
	v.extracted_text,v.extracted_partial,v.extracted_scope`

const archiveVersionCatalogColumns = `v.id,v.document_id,v.number,v.historical,v.attachment_id,v.name,v.mime,v.size,v.object_key,
	v.event_id,v.source_event_id,v.submitted_at,v.note,v.upload_status,v.summary_status,v.summary,v.summary_source,v.summary_scope,
	'' AS extracted_text,0 AS extracted_partial,'' AS extracted_scope`

var _ documentarchive.Store = (*Store)(nil)

func scanArchiveVersion(row scanner) (documentarchive.Version, error) {
	var version documentarchive.Version
	var historical, partial int
	var submitted string
	var uploadStatus, summaryStatus, summarySource string
	err := row.Scan(&version.ID, &version.DocumentID, &version.Number, &historical, &version.AttachmentID, &version.Name,
		&version.MIME, &version.Size, &version.ObjectKey, &version.EventID, &version.SourceEventID, &submitted, &version.Note,
		&uploadStatus, &summaryStatus, &version.Summary, &summarySource, &version.SummaryScope, &version.ExtractedText, &partial, &version.ExtractedScope)
	if err != nil {
		return version, err
	}
	version.Historical = historical != 0
	version.ExtractedPartial = partial != 0
	version.UploadStatus = documentarchive.Status(uploadStatus)
	version.SummaryStatus = documentarchive.Status(summaryStatus)
	version.SummarySource = documentarchive.SummarySource(summarySource)
	version.SubmittedAt, err = parseTimestamp(submitted)
	return version, err
}

func loadArchiveVersionTx(ctx context.Context, tx *sql.Tx, versionID int64) (documentarchive.Version, error) {
	version, err := scanArchiveVersion(tx.QueryRowContext(ctx, `SELECT `+archiveVersionColumns+`
		FROM personal_archive_versions v WHERE v.id=?`, versionID))
	if errors.Is(err, sql.ErrNoRows) {
		return documentarchive.Version{}, documentarchive.ErrNotFound
	}
	return version, err
}

func (s *Store) ArchiveVersion(ctx context.Context, versionID int64) (documentarchive.Version, error) {
	if versionID <= 0 {
		return documentarchive.Version{}, documentarchive.ErrNotFound
	}
	version, err := scanArchiveVersion(s.db.QueryRowContext(ctx, `SELECT `+archiveVersionColumns+`
		FROM personal_archive_versions v WHERE v.id=?`, versionID))
	if errors.Is(err, sql.ErrNoRows) {
		return documentarchive.Version{}, documentarchive.ErrNotFound
	}
	return version, err
}

func canonicalArchiveTags(input []string) ([]string, error) {
	if len(input) > maxArchiveTagCount {
		return nil, fmt.Errorf("personal: archive tags exceed %d entries", maxArchiveTagCount)
	}
	tags := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, tag := range input {
		if !utf8.ValidString(tag) {
			return nil, errors.New("personal: archive tag is not valid UTF-8")
		}
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if utf8.RuneCountInString(tag) > maxArchiveTagChars {
			return nil, fmt.Errorf("personal: archive tag exceeds %d characters", maxArchiveTagChars)
		}
		key := strings.ToLower(tag)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		tags = append(tags, tag)
	}
	return tags, nil
}

func normalizeArchiveRequest(request documentarchive.Request) (documentarchive.Request, error) {
	if request.EventID <= 0 || request.DocumentID < 0 || request.SourceEventID < 0 || request.Number < 0 || !requestKeyValid(request.RequestKey) {
		return request, errors.New("personal: archive event, request key, and nonnegative document/version numbers are required")
	}
	if strings.TrimSpace(request.AttachmentID) == "" {
		return request, errors.New("personal: archive attachment ID is required")
	}
	if !utf8.ValidString(request.Title) || !utf8.ValidString(request.Category) || !utf8.ValidString(request.Note) {
		return request, errors.New("personal: archive metadata must be valid UTF-8")
	}
	request.Title = strings.TrimSpace(request.Title)
	request.Category = strings.TrimSpace(request.Category)
	if request.DocumentID == 0 && request.Title == "" {
		return request, errors.New("personal: archive title is required when creating a document")
	}
	if utf8.RuneCountInString(request.Title) > maxArchiveTitleChars {
		return request, fmt.Errorf("personal: archive title exceeds %d characters", maxArchiveTitleChars)
	}
	if utf8.RuneCountInString(request.Category) > maxArchiveCategoryChars {
		return request, fmt.Errorf("personal: archive category exceeds %d characters", maxArchiveCategoryChars)
	}
	if utf8.RuneCountInString(request.Note) > maxArchiveNoteChars {
		return request, fmt.Errorf("personal: archive note exceeds %d characters", maxArchiveNoteChars)
	}
	if request.Historical && (request.DocumentID <= 0 || request.Number <= 0) {
		return request, errors.New("personal: historical archive versions require an existing document and an explicit positive number")
	}
	tags, err := canonicalArchiveTags(request.Tags)
	if err != nil {
		return request, err
	}
	request.Tags = tags
	return request, nil
}

func archiveRequestPayload(request documentarchive.Request) (string, error) {
	data, err := json.Marshal(request)
	return string(data), err
}

func (s *Store) EnqueueArchive(ctx context.Context, input documentarchive.Request) (documentarchive.Version, error) {
	request, err := normalizeArchiveRequest(input)
	if err != nil {
		return documentarchive.Version{}, err
	}
	payload, err := archiveRequestPayload(request)
	if err != nil {
		return documentarchive.Version{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return documentarchive.Version{}, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, request.EventID, true); err != nil {
		return documentarchive.Version{}, err
	}
	var oldPayload string
	var oldVersionID int64
	err = tx.QueryRowContext(ctx, `SELECT payload,version_id FROM personal_archive_requests WHERE event_id=? AND request_key=?`, request.EventID, request.RequestKey).
		Scan(&oldPayload, &oldVersionID)
	if err == nil {
		if oldPayload != payload {
			return documentarchive.Version{}, documentarchive.ErrConflict
		}
		version, err := loadArchiveVersionTx(ctx, tx, oldVersionID)
		if err != nil {
			return documentarchive.Version{}, err
		}
		if err := tx.Commit(); err != nil {
			return documentarchive.Version{}, err
		}
		return version, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return documentarchive.Version{}, err
	}
	if err := requireUserEvent(ctx, tx, request.EventID, false); err != nil {
		return documentarchive.Version{}, err
	}
	attachment, err := s.validateStoredAttachment(ctx, tx, request.AttachmentID)
	if err != nil {
		return documentarchive.Version{}, err
	}
	sourceEventID, err := resolveArchiveSourceEvent(ctx, tx, request.EventID, request.SourceEventID, request.AttachmentID)
	if err != nil {
		return documentarchive.Version{}, err
	}
	tagsJSON, err := json.Marshal(request.Tags)
	if err != nil {
		return documentarchive.Version{}, err
	}
	var documentID int64
	if request.DocumentID == 0 {
		if request.Historical {
			return documentarchive.Version{}, errors.New("personal: a new archive document cannot begin with a historical version")
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO personal_archive_documents(title,category,tags,created_at,updated_at)
			VALUES(?,?,?,?,?)`, request.Title, request.Category, string(tagsJSON), timestamp(time.Now()), timestamp(time.Now()))
		if err != nil {
			return documentarchive.Version{}, err
		}
		documentID, err = result.LastInsertId()
		if err != nil {
			return documentarchive.Version{}, err
		}
	} else {
		documentID = request.DocumentID
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM personal_archive_documents WHERE id=?`, documentID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return documentarchive.Version{}, documentarchive.ErrNotFound
		} else if err != nil {
			return documentarchive.Version{}, err
		}
	}

	var maxNumber int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0) FROM personal_archive_versions WHERE document_id=?`, documentID).Scan(&maxNumber); err != nil {
		return documentarchive.Version{}, err
	}
	versionNumber := request.Number
	if versionNumber == 0 {
		if maxNumber >= math.MaxInt64 {
			return documentarchive.Version{}, errors.New("personal: archive version number is exhausted")
		}
		versionNumber = int(maxNumber + 1)
	}
	if versionNumber <= 0 {
		return documentarchive.Version{}, errors.New("personal: archive version number must be positive")
	}
	if request.Historical == false && request.Number > 0 && int64(request.Number) <= maxNumber {
		return documentarchive.Version{}, fmt.Errorf("%w: non-historical version number must follow the highest existing number", documentarchive.ErrConflict)
	}
	if request.Historical {
		var latestNormalNumber int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0) FROM personal_archive_versions WHERE document_id=? AND historical=0`, documentID).Scan(&latestNormalNumber); err != nil {
			return documentarchive.Version{}, err
		}
		if int64(versionNumber) >= latestNormalNumber {
			return documentarchive.Version{}, fmt.Errorf("%w: historical version number must be below the latest non-historical version", documentarchive.ErrConflict)
		}
	}
	var numberExists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM personal_archive_versions WHERE document_id=? AND number=?)`, documentID, versionNumber).Scan(&numberExists); err != nil {
		return documentarchive.Version{}, err
	}
	if numberExists != 0 {
		return documentarchive.Version{}, documentarchive.ErrConflict
	}

	randomID, err := uuid.NewRandom()
	if err != nil {
		return documentarchive.Version{}, fmt.Errorf("personal: create archive object key: %w", err)
	}
	objectKey := "documents/" + randomID.String()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO personal_archive_versions(document_id,number,historical,attachment_id,name,mime,size,object_key,
		event_id,source_event_id,submitted_at,note,upload_status,summary_status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'pending','pending')`, documentID, versionNumber, boolInt(request.Historical), attachment.ID,
		attachment.Name, attachment.MIME, attachment.Size, objectKey, request.EventID, sourceEventID, timestamp(now), request.Note)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return documentarchive.Version{}, documentarchive.ErrConflict
		}
		return documentarchive.Version{}, err
	}
	versionID, err := result.LastInsertId()
	if err != nil {
		return documentarchive.Version{}, err
	}
	if !request.Historical {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_documents SET latest_version_id=?,updated_at=? WHERE id=?`, versionID, timestamp(now), documentID); err != nil {
			return documentarchive.Version{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_documents SET updated_at=? WHERE id=?`, timestamp(now), documentID); err != nil {
			return documentarchive.Version{}, err
		}
	}
	for _, kind := range []documentarchive.WorkKind{documentarchive.WorkUpload, documentarchive.WorkSummary} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO personal_archive_work(version_id,kind,status,attempt,next_attempt_at,updated_at)
			VALUES(?,?,'queued',0,?,?)`, versionID, kind, timestamp(now), timestamp(now)); err != nil {
			return documentarchive.Version{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO personal_archive_requests(event_id,request_key,payload,version_id) VALUES(?,?,?,?)`,
		request.EventID, request.RequestKey, payload, versionID); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return documentarchive.Version{}, documentarchive.ErrConflict
		}
		return documentarchive.Version{}, err
	}
	version, err := loadArchiveVersionTx(ctx, tx, versionID)
	if err != nil {
		return documentarchive.Version{}, err
	}
	if err := tx.Commit(); err != nil {
		return documentarchive.Version{}, err
	}
	return version, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func resolveArchiveSourceEvent(ctx context.Context, tx *sql.Tx, archiveEventID, sourceEventID int64, attachmentID string) (int64, error) {
	if sourceEventID > 0 && sourceEventID <= archiveEventID {
		var kind string
		var attachments string
		err := tx.QueryRowContext(ctx, `SELECT kind,attachments FROM personal_events WHERE id=?`, sourceEventID).Scan(&kind, &attachments)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err == nil && (kind == "user" || kind == "material") && eventHasAttachment(attachments, attachmentID) {
			return sourceEventID, nil
		}
	}
	var foundID int64
	err := tx.QueryRowContext(ctx, `SELECT e.id FROM personal_events e, json_each(e.attachments) AS a
		WHERE e.id<=? AND e.kind IN ('user','material') AND json_extract(a.value,'$.id')=? ORDER BY e.id DESC LIMIT 1`,
		archiveEventID, attachmentID).Scan(&foundID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("personal: archive attachment has no matching source event")
	}
	return foundID, err
}

func eventHasAttachment(raw, attachmentID string) bool {
	var attachments []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &attachments); err != nil {
		return false
	}
	for _, attachment := range attachments {
		if attachment.ID == attachmentID {
			return true
		}
	}
	return false
}

func loadArchiveDocumentTx(ctx context.Context, tx *sql.Tx, documentID int64) (documentarchive.Document, int64, error) {
	var doc documentarchive.Document
	var rawTags string
	var latestID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT id,title,category,tags,latest_version_id FROM personal_archive_documents WHERE id=?`, documentID).
		Scan(&doc.ID, &doc.Title, &doc.Category, &rawTags, &latestID)
	if errors.Is(err, sql.ErrNoRows) {
		return documentarchive.Document{}, 0, documentarchive.ErrNotFound
	}
	if err != nil {
		return documentarchive.Document{}, 0, err
	}
	if err := json.Unmarshal([]byte(rawTags), &doc.Tags); err != nil {
		return documentarchive.Document{}, 0, fmt.Errorf("personal: decode archive tags for document %d: %w", documentID, err)
	}
	if doc.Tags == nil {
		doc.Tags = []string{}
	}
	if !latestID.Valid {
		return doc, 0, errors.New("personal: archive document has no non-historical version")
	}
	return doc, latestID.Int64, nil
}

func (s *Store) ArchiveDocument(ctx context.Context, documentID int64, beforeNumber, limit int) (documentarchive.Detail, error) {
	if documentID <= 0 || beforeNumber < 0 || limit < 0 {
		return documentarchive.Detail{}, errors.New("personal: archive document ID and nonnegative page bounds are required")
	}
	if limit == 0 {
		limit = defaultArchivePageSize
	} else if limit > maxArchivePageSize {
		limit = maxArchivePageSize
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return documentarchive.Detail{}, err
	}
	defer tx.Rollback()
	doc, latestID, err := loadArchiveDocumentTx(ctx, tx, documentID)
	if err != nil {
		return documentarchive.Detail{}, err
	}
	latest, err := scanArchiveVersion(tx.QueryRowContext(ctx, `SELECT `+archiveVersionCatalogColumns+`
		FROM personal_archive_versions v WHERE v.id=?`, latestID))
	if errors.Is(err, sql.ErrNoRows) {
		return documentarchive.Detail{}, documentarchive.ErrNotFound
	}
	if err != nil {
		return documentarchive.Detail{}, err
	}
	query := `SELECT ` + archiveVersionCatalogColumns + ` FROM personal_archive_versions v
		WHERE v.document_id=? AND (?=0 OR v.number<?) ORDER BY v.number DESC,v.id DESC LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, documentID, beforeNumber, beforeNumber, limit+1)
	if err != nil {
		return documentarchive.Detail{}, err
	}
	versions := make([]documentarchive.Version, 0, limit+1)
	for rows.Next() {
		version, err := scanArchiveVersion(rows)
		if err != nil {
			_ = rows.Close()
			return documentarchive.Detail{}, err
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return documentarchive.Detail{}, err
	}
	if err := rows.Close(); err != nil {
		return documentarchive.Detail{}, err
	}
	nextBefore := 0
	if len(versions) > limit {
		versions = versions[:limit]
		nextBefore = versions[len(versions)-1].Number
	}
	if err := tx.Commit(); err != nil {
		return documentarchive.Detail{}, err
	}
	return documentarchive.Detail{Document: doc, Latest: latest, Versions: versions, NextBeforeNumber: nextBefore}, nil
}
