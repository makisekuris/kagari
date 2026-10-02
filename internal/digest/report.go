package digest

import (
	"time"

	"kagari/internal/domain"
)

type Report struct {
	UserID      int64                 `json:"user_id"`
	Start       time.Time             `json:"start"`
	End         time.Time             `json:"end"`
	GeneratedAt time.Time             `json:"generated_at"`
	Total       int                   `json:"total,omitempty"`
	Pending     int                   `json:"pending,omitempty"`
	Failed      int                   `json:"failed,omitempty"`
	Entries     []domain.ArchiveEntry `json:"entries,omitempty"`
	Version     string                `json:"version,omitempty"`
	Input       *domain.DigestInput   `json:"input,omitempty"`
	Review      *domain.DigestReview  `json:"review,omitempty"`
	Usage       domain.Usage          `json:"usage,omitempty"`
	Model       string                `json:"model,omitempty"`
}

func Count(review domain.DigestReview) int {
	count := 0
	for _, section := range review.Sections {
		count += len(section.Items)
	}
	return count
}
