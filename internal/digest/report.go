package digest

import (
	"time"

	"kagari/internal/domain"
)

type Report struct {
	UserID      int64               `json:"user_id"`
	Start       time.Time           `json:"start"`
	End         time.Time           `json:"end"`
	GeneratedAt time.Time           `json:"generated_at"`
	Version     string              `json:"version"`
	Input       *domain.DigestInput `json:"input,omitempty"`
	Body        string              `json:"body"`
	Usage       domain.Usage        `json:"usage,omitempty"`
	Model       string              `json:"model,omitempty"`
}
