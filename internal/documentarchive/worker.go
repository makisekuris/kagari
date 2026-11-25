package documentarchive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"kagari/internal/logging"
)

const (
	maxWorkAttempts = 3
	workTimeout     = 2 * time.Minute
	workerPollDelay = 250 * time.Millisecond
	maxSummaryRunes = 240
)

// Run processes persisted work while the caller holds the personal instance lock.
func (s *Service) Run(ctx context.Context) error {
	if err := s.store.RecoverArchiveWork(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(workerPollDelay)
	defer ticker.Stop()
	for ctx.Err() == nil {
		worked, err := s.ProcessNext(ctx)
		if err != nil {
			return err
		}
		if !worked {
			select {
			case <-ctx.Done():
			case <-ticker.C:
			}
		}
	}
	return ctx.Err()
}

func (s *Service) ProcessNext(ctx context.Context) (bool, error) {
	work, err := s.store.ClaimArchiveWork(ctx, time.Now())
	if err != nil || work == nil {
		return false, err
	}
	workCtx, cancel := context.WithTimeout(ctx, workTimeout)
	defer cancel()
	var result WorkResult
	switch work.Kind {
	case WorkUpload:
		err = s.upload(workCtx, work.Version)
		result.Status = StatusReady
	case WorkSummary:
		result, err = s.analyze(workCtx, work.Version)
	default:
		return false, fmt.Errorf("unexpected archive work kind %q", work.Kind)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if err != nil {
		logging.FromContext(ctx).Warn("archive.work.failed", append(logging.ErrorFields(err),
			zap.Int64("version_id", work.Version.ID), zap.String("kind", string(work.Kind)), zap.Int("attempt", work.Attempt))...)
		result.Status = StatusFailed
		result.Error = string(work.Kind) + "_failed"
		if work.Attempt < maxWorkAttempts {
			result.Status = StatusPending
			result.RetryAt = time.Now().Add(time.Second * time.Duration(1<<max(0, work.Attempt-1)))
		} else if work.Kind == WorkSummary && strings.TrimSpace(work.Version.Note) != "" {
			result = ownerSummary(work.Version.Note)
		}
	}
	return true, s.store.FinishArchiveWork(ctx, *work, result)
}

func (s *Service) upload(ctx context.Context, v Version) error {
	file, err := s.files.OpenAttachment(ctx, v.AttachmentID)
	if err != nil {
		return err
	}
	data, err := readOriginal(file, v.Size)
	if err != nil {
		return err
	}
	return s.objects.Put(ctx, v.ObjectKey, bytes.NewReader(data), v.Size, v.MIME)
}

func (s *Service) analyze(ctx context.Context, v Version) (WorkResult, error) {
	data, err := s.readObject(ctx, v)
	if err != nil {
		return WorkResult{}, err
	}
	sample, err := extractSample(ctx, v, data)
	if err != nil {
		if ctx.Err() != nil {
			return WorkResult{}, ctx.Err()
		}
		logging.FromContext(ctx).Warn("archive.extract.unavailable", append(logging.ErrorFields(err), zap.Int64("version_id", v.ID))...)
	}
	model := s.summarizeText
	if len(sample.Image) != 0 {
		model = s.summarizeImage
	}
	if model == nil || (sample.Text == "" && len(sample.Image) == 0) {
		return ownerSummary(v.Note), nil
	}
	summary, err := model(ctx, sample)
	if err != nil {
		return WorkResult{}, err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return WorkResult{}, errors.New("archive summarizer returned no text")
	}
	return WorkResult{
		Status: StatusReady, Summary: clipRunes(summary, maxSummaryRunes), SummarySource: SummaryFromOriginal,
		SummaryScope: sample.Scope, ExtractedText: sample.Text, ExtractedPartial: sample.Partial, ExtractedScope: sample.Scope,
	}, nil
}

func ownerSummary(note string) WorkResult {
	note = strings.TrimSpace(note)
	if note == "" {
		return WorkResult{Status: StatusNeedsNote}
	}
	return WorkResult{Status: StatusReady, Summary: clipRunes(note, maxSummaryRunes), SummarySource: SummaryFromOwner, SummaryScope: "使用者归档说明"}
}

func clipRunes(text string, limit int) string {
	for at := range text {
		if limit == 0 {
			return text[:at]
		}
		limit--
	}
	return text
}
