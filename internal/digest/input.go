package digest

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"kagari/internal/domain"
)

// Prepare freezes the archive entries and instructions supplied to a weekly review.
func Prepare(request domain.DigestRequest, asOf time.Time, timezone, profile, instruction string, entries []domain.ArchiveEntry) (domain.DigestInput, error) {
	if request.Version != domain.DigestVersion {
		return domain.DigestInput{}, fmt.Errorf("unsupported digest version")
	}
	if !request.Start.Before(request.End) {
		return domain.DigestInput{}, fmt.Errorf("digest interval must have start before end")
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return domain.DigestInput{}, fmt.Errorf("digest timezone: %w", err)
	}
	if strings.TrimSpace(instruction) == "" {
		return domain.DigestInput{}, fmt.Errorf("digest instruction is required")
	}

	selected := make([]domain.ArchiveEntry, 0, len(entries))
	for _, entry := range entries {
		receivedAt := entry.Submission.ReceivedAt
		if entry.Submission.UserID == request.UserID && !receivedAt.Before(request.Start) && receivedAt.Before(request.End) {
			selected = append(selected, entry)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		left, right := selected[i], selected[j]
		if left.Submission.ReceivedAt.Equal(right.Submission.ReceivedAt) {
			return left.JobID < right.JobID
		}
		return left.Submission.ReceivedAt.Before(right.Submission.ReceivedAt)
	})

	input := domain.DigestInput{
		UserID: request.UserID, Start: request.Start, Cutoff: request.End, AsOf: asOf,
		Timezone: timezone, Profile: profile, Instruction: instruction,
		Entries: make([]domain.DigestEntry, 0, len(selected)),
	}
	seenKeys := make(map[string]bool, len(selected))
	for _, entry := range selected {
		key := entry.Submission.CacheKey
		if key != "" && seenKeys[key] {
			continue
		}
		if key != "" {
			seenKeys[key] = true
		}
		item := domain.DigestEntry{
			JobID: entry.JobID, ReceivedAt: entry.Submission.ReceivedAt,
			Analysis: cloneAnalysis(entry.Result.Analysis),
			Sources:  make([]domain.DigestSource, 0, len(entry.Result.Sources)),
		}
		for _, source := range entry.Result.Sources {
			item.Sources = append(item.Sources, domain.DigestSource{
				ID: source.ID, Title: source.Title, RequestedURL: source.RequestedURL, URL: source.URL,
				Status: source.Status, Reason: source.Reason, Truncated: source.Truncated, Usable: source.Usable(),
			})
		}
		input.Entries = append(input.Entries, item)
	}
	return input, nil
}

func cloneAnalysis(a domain.Analysis) domain.Analysis {
	if a.Headings != nil {
		headings := *a.Headings
		a.Headings = &headings
	}
	a.Summary = cloneClaims(a.Summary)
	a.Discussion = cloneClaims(a.Discussion)
	a.Evaluation = cloneClaims(a.Evaluation)
	a.Tags = append([]string(nil), a.Tags...)
	a.Uncertainties = append([]string(nil), a.Uncertainties...)
	return a
}

func cloneClaims(claims []domain.Claim) []domain.Claim {
	if claims == nil {
		return nil
	}
	out := make([]domain.Claim, len(claims))
	for i, claim := range claims {
		out[i] = claim
		out[i].SourceIDs = append([]string(nil), claim.SourceIDs...)
	}
	return out
}
