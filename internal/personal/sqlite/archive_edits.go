package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"kagari/internal/documentarchive"
)

func (s *Store) UpdateArchiveDocument(ctx context.Context, patch documentarchive.DocumentPatch) (documentarchive.Document, error) {
	if patch.DocumentID <= 0 || (patch.Title == nil && patch.Category == nil && patch.Tags == nil) {
		return documentarchive.Document{}, errors.New("personal: archive document ID and at least one edited field are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return documentarchive.Document{}, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, patch.EventID, false); err != nil {
		return documentarchive.Document{}, err
	}
	doc, _, err := loadArchiveDocumentTx(ctx, tx, patch.DocumentID)
	if err != nil {
		return documentarchive.Document{}, err
	}
	if patch.Title != nil {
		doc.Title = strings.TrimSpace(*patch.Title)
		if doc.Title == "" || !utf8.ValidString(doc.Title) || utf8.RuneCountInString(doc.Title) > maxArchiveTitleChars {
			return documentarchive.Document{}, fmt.Errorf("personal: archive title must contain 1 to %d characters", maxArchiveTitleChars)
		}
	}
	if patch.Category != nil {
		doc.Category = strings.TrimSpace(*patch.Category)
		if !utf8.ValidString(doc.Category) || utf8.RuneCountInString(doc.Category) > maxArchiveCategoryChars {
			return documentarchive.Document{}, fmt.Errorf("personal: archive category must be valid UTF-8 and at most %d characters", maxArchiveCategoryChars)
		}
	}
	if patch.Tags != nil {
		doc.Tags, err = canonicalArchiveTags(*patch.Tags)
		if err != nil {
			return documentarchive.Document{}, err
		}
	}
	tags, err := json.Marshal(doc.Tags)
	if err != nil {
		return documentarchive.Document{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE personal_archive_documents SET title=?,category=?,tags=?,updated_at=? WHERE id=?`,
		doc.Title, doc.Category, string(tags), timestamp(time.Now()), doc.ID)
	if err != nil {
		return documentarchive.Document{}, err
	}
	return doc, tx.Commit()
}

func (s *Store) SetArchiveSummary(ctx context.Context, eventID, versionID int64, summary documentarchive.WorkResult) (documentarchive.Version, error) {
	if summary.Status != documentarchive.StatusReady || summary.SummarySource != documentarchive.SummaryFromOwner ||
		strings.TrimSpace(summary.Summary) == "" || !utf8.ValidString(summary.Summary) {
		return documentarchive.Version{}, errors.New("personal: an explicit user summary is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return documentarchive.Version{}, err
	}
	defer tx.Rollback()
	if err := requireUserEvent(ctx, tx, eventID, false); err != nil {
		return documentarchive.Version{}, err
	}
	version, err := loadArchiveVersionTx(ctx, tx, versionID)
	if err != nil {
		return documentarchive.Version{}, err
	}
	if version.SummaryStatus == documentarchive.StatusPending || version.SummaryStatus == documentarchive.StatusProcessing {
		return documentarchive.Version{}, documentarchive.ErrSummaryPending
	}
	_, err = tx.ExecContext(ctx, `UPDATE personal_archive_versions
		SET summary_status='ready',summary=?,summary_source=?,summary_scope=?,summary_error='' WHERE id=?`,
		summary.Summary, summary.SummarySource, summary.SummaryScope, version.ID)
	if err != nil {
		return documentarchive.Version{}, err
	}
	version.SummaryStatus, version.SummarySource = summary.Status, summary.SummarySource
	version.Summary, version.SummaryScope = summary.Summary, summary.SummaryScope
	return version, tx.Commit()
}
