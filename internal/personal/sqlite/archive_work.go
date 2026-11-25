package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"kagari/internal/documentarchive"
)

const maxArchiveWorkErrorChars = 4000

func (s *Store) ClaimArchiveWork(ctx context.Context, now time.Time) (*documentarchive.Work, error) {
	if now.IsZero() {
		return nil, errors.New("personal: archive work claim requires a timestamp")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var workID, versionID int64
	var kind string
	var attempt int
	err = tx.QueryRowContext(ctx, `SELECT w.id,w.version_id,w.kind,w.attempt FROM personal_archive_work w
		JOIN personal_archive_versions v ON v.id=w.version_id
		WHERE w.status IN ('queued','retry') AND w.next_attempt_at<=?
		AND (w.kind<>'summary' OR v.upload_status='ready') ORDER BY w.next_attempt_at,w.id LIMIT 1`, timestamp(now)).
		Scan(&workID, &versionID, &kind, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE personal_archive_work SET status='processing',attempt=attempt+1,updated_at=?
		WHERE id=? AND status IN ('queued','retry') AND attempt=?`, timestamp(now), workID, attempt)
	if err != nil {
		return nil, err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if changed != 1 {
		return nil, documentarchive.ErrConflict
	}
	statusColumn, errorColumn := "upload_status", "upload_error"
	if documentarchive.WorkKind(kind) == documentarchive.WorkSummary {
		statusColumn, errorColumn = "summary_status", "summary_error"
	} else if documentarchive.WorkKind(kind) != documentarchive.WorkUpload {
		return nil, fmt.Errorf("personal: invalid stored archive work kind %q", kind)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_versions SET `+statusColumn+`='processing',`+errorColumn+`='' WHERE id=?`, versionID); err != nil {
		return nil, err
	}
	version, err := loadArchiveVersionTx(ctx, tx, versionID)
	if err != nil {
		return nil, err
	}
	work := &documentarchive.Work{Kind: documentarchive.WorkKind(kind), Version: version, Attempt: attempt + 1}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return work, nil
}

func validateArchiveWorkResult(kind documentarchive.WorkKind, result documentarchive.WorkResult) (jobStatus string, versionStatus documentarchive.Status, nextAttempt time.Time, err error) {
	if kind != documentarchive.WorkUpload && kind != documentarchive.WorkSummary {
		return "", "", time.Time{}, errors.New("personal: invalid archive work kind")
	}
	if !utf8.ValidString(result.Error) || utf8.RuneCountInString(result.Error) > maxArchiveWorkErrorChars {
		return "", "", time.Time{}, errors.New("personal: archive work error is invalid or too long")
	}
	if result.Status == documentarchive.StatusPending {
		if result.RetryAt.IsZero() {
			return "", "", time.Time{}, errors.New("personal: pending archive work requires a retry time")
		}
		return "retry", documentarchive.StatusPending, result.RetryAt.UTC(), nil
	}
	if !result.RetryAt.IsZero() {
		return "", "", time.Time{}, errors.New("personal: retry time is only valid for pending archive work")
	}
	switch kind {
	case documentarchive.WorkUpload:
		switch result.Status {
		case documentarchive.StatusReady:
			return "done", result.Status, time.Time{}, nil
		case documentarchive.StatusFailed:
			return "failed", result.Status, time.Time{}, nil
		default:
			return "", "", time.Time{}, errors.New("personal: upload work must finish ready, pending, or failed")
		}
	case documentarchive.WorkSummary:
		switch result.Status {
		case documentarchive.StatusReady:
			if strings.TrimSpace(result.Summary) == "" || (result.SummarySource != documentarchive.SummaryFromOriginal && result.SummarySource != documentarchive.SummaryFromOwner) || strings.TrimSpace(result.SummaryScope) == "" {
				return "", "", time.Time{}, errors.New("personal: ready archive summary requires text, a valid source, and scope")
			}
			if !utf8.ValidString(result.Summary) || !utf8.ValidString(result.SummaryScope) || !utf8.ValidString(result.ExtractedText) || !utf8.ValidString(result.ExtractedScope) {
				return "", "", time.Time{}, errors.New("personal: archive summary data must be valid UTF-8")
			}
			return "done", result.Status, time.Time{}, nil
		case documentarchive.StatusNeedsNote:
			return "done", result.Status, time.Time{}, nil
		case documentarchive.StatusFailed:
			return "failed", result.Status, time.Time{}, nil
		default:
			return "", "", time.Time{}, errors.New("personal: summary work must finish ready, needs_note, pending, or failed")
		}
	}
	return "", "", time.Time{}, errors.New("personal: unsupported archive work kind")
}

func (s *Store) FinishArchiveWork(ctx context.Context, work documentarchive.Work, result documentarchive.WorkResult) error {
	if work.Version.ID <= 0 || work.Attempt <= 0 {
		return errors.New("personal: archive work version and attempt must be positive")
	}
	jobStatus, versionStatus, nextAttempt, err := validateArchiveWorkResult(work.Kind, result)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var attempt int
	err = tx.QueryRowContext(ctx, `SELECT status,attempt FROM personal_archive_work WHERE version_id=? AND kind=?`, work.Version.ID, work.Kind).
		Scan(&status, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return documentarchive.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "processing" || attempt != work.Attempt {
		return documentarchive.ErrConflict
	}
	now := time.Now().UTC()
	nextAt := timestamp(now)
	if !nextAttempt.IsZero() {
		nextAt = timestamp(nextAttempt)
	}
	jobUpdate, err := tx.ExecContext(ctx, `UPDATE personal_archive_work SET status=?,next_attempt_at=?,last_error=?,updated_at=?
		WHERE version_id=? AND kind=? AND status='processing' AND attempt=?`, jobStatus, nextAt, result.Error, timestamp(now), work.Version.ID, work.Kind, work.Attempt)
	if err != nil {
		return err
	}
	if changed, err := jobUpdate.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return documentarchive.ErrConflict
	}
	if work.Kind == documentarchive.WorkUpload {
		if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_versions SET upload_status=?,upload_error=? WHERE id=?`, versionStatus, result.Error, work.Version.ID); err != nil {
			return err
		}
		if versionStatus == documentarchive.StatusFailed {
			if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_versions SET summary_status='failed',summary_error=? WHERE id=? AND summary_status='pending'`, result.Error, work.Version.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_work SET status='failed',next_attempt_at=?,last_error=?,updated_at=?
				WHERE version_id=? AND kind='summary' AND status IN ('queued','retry')`, timestamp(now), result.Error, timestamp(now), work.Version.ID); err != nil {
				return err
			}
		}
	} else {
		if len(result.ExtractedText) > 40<<20 || utf8.RuneCountInString(result.Summary) > 4000 || utf8.RuneCountInString(result.SummaryScope) > 2000 || utf8.RuneCountInString(result.ExtractedScope) > 2000 || utf8.RuneCountInString(result.ExtractedText) > 10<<20 {
			return errors.New("personal: archive summary data exceeds its size limit")
		}
		source, summary, scope, extracted, extractedScope := "", "", "", "", ""
		partial := false
		if versionStatus == documentarchive.StatusReady {
			source, summary, scope, extracted, extractedScope = string(result.SummarySource), result.Summary, result.SummaryScope, result.ExtractedText, result.ExtractedScope
			partial = result.ExtractedPartial
		}
		if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_versions SET summary_status=?,summary=?,summary_source=?,summary_scope=?,
			extracted_text=?,extracted_partial=?,extracted_scope=?,summary_error=? WHERE id=?`, versionStatus, summary, source, scope, extracted, boolInt(partial), extractedScope, result.Error, work.Version.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecoverArchiveWork(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := timestamp(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_versions SET upload_status='pending',upload_error=''
		WHERE upload_status='processing' AND EXISTS(SELECT 1 FROM personal_archive_work w WHERE w.version_id=personal_archive_versions.id AND w.kind='upload' AND w.status='processing')`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_versions SET summary_status='pending',summary_error=''
		WHERE summary_status='processing' AND EXISTS(SELECT 1 FROM personal_archive_work w WHERE w.version_id=personal_archive_versions.id AND w.kind='summary' AND w.status='processing')`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE personal_archive_work SET status='queued',next_attempt_at=?,updated_at=?
		WHERE status='processing'`, now, now); err != nil {
		return err
	}
	return tx.Commit()
}
