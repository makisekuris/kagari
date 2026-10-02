package digest

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"kagari/internal/domain"
)

const (
	maxReviewTitleRunes = 160
	maxSectionNameRunes = 80
)

// ValidateReview ensures the model review partitions the input and cites only available evidence.
func ValidateReview(input domain.DigestInput, review domain.DigestReview) error {
	entries := make(map[int64]domain.DigestEntry, len(input.Entries))
	for _, entry := range input.Entries {
		if _, exists := entries[entry.JobID]; exists {
			return fmt.Errorf("duplicate input job id %d", entry.JobID)
		}
		entries[entry.JobID] = entry
	}
	if len(entries) == 0 {
		if len(review.Sections) != 0 {
			return fmt.Errorf("empty input must have no review sections")
		}
		return nil
	}
	if len(review.Sections) == 0 {
		return fmt.Errorf("review must include sections")
	}

	seenEntries := make(map[int64]bool, len(entries))
	seenRefs := make(map[domain.DigestRef]bool)
	for _, section := range review.Sections {
		if !boundedLine(section.Name, maxSectionNameRunes) || len(section.Items) == 0 {
			return fmt.Errorf("section name must be a nonempty single line up to %d characters and section must contain items", maxSectionNameRunes)
		}
		for _, item := range section.Items {
			if len(item.EntryIDs) == 0 || !boundedLine(item.Title, maxReviewTitleRunes) || strings.TrimSpace(item.Review) == "" {
				return fmt.Errorf("review item must have entries, a valid title, and review text")
			}
			itemEntries := make(map[int64]bool, len(item.EntryIDs))
			for _, id := range item.EntryIDs {
				if _, ok := entries[id]; !ok {
					return fmt.Errorf("review references unknown job id %d", id)
				}
				if itemEntries[id] || seenEntries[id] {
					return fmt.Errorf("duplicate review job id %d", id)
				}
				itemEntries[id], seenEntries[id] = true, true
			}
			referencedJobs := make(map[int64]bool)
			for _, ref := range item.Refs {
				if !itemEntries[ref.JobID] || strings.TrimSpace(ref.SourceID) == "" {
					return fmt.Errorf("citation job id %d is not owned by its review item", ref.JobID)
				}
				entry := entries[ref.JobID]
				matches := 0
				usable := true
				for _, source := range entry.Sources {
					if source.ID == ref.SourceID {
						matches++
						usable = source.Usable
					}
				}
				if matches != 1 || !usable {
					return fmt.Errorf("citation (%d, %q) is not a usable input source", ref.JobID, ref.SourceID)
				}
				if seenRefs[ref] {
					return fmt.Errorf("duplicate citation (%d, %q)", ref.JobID, ref.SourceID)
				}
				seenRefs[ref] = true
				referencedJobs[ref.JobID] = true
			}
			for _, id := range item.EntryIDs {
				for _, source := range entries[id].Sources {
					if source.Usable && !referencedJobs[id] {
						return fmt.Errorf("review item does not cite usable evidence for job id %d", id)
					}
				}
			}
		}
	}
	if len(seenEntries) != len(entries) {
		return fmt.Errorf("review is missing %d input entries", len(entries)-len(seenEntries))
	}
	return nil
}

func boundedLine(value string, limit int) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed != "" && !strings.ContainsAny(trimmed, "\r\n\u0085\u2028\u2029") && utf8.RuneCountInString(trimmed) <= limit
}
