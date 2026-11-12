package personal

import "time"

// Deleted entries retain a tombstone so an older memory review cannot resurrect them.
type Memory struct {
	Key           string    `json:"key"`
	Value         string    `json:"value"`
	Version       int64     `json:"version"`
	SourceEventID int64     `json:"source_event_id"`
	Deleted       bool      `json:"deleted"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type MemoryChange struct {
	Operation       string `json:"operation"` // put or forget
	Key             string `json:"key"`
	Value           string `json:"value"`
	ExpectedVersion int64  `json:"expected_version"` // 0 creates a key that has never existed.
	Evidence        string `json:"evidence"`         // Exact supporting quote from the source user event.
}

type MemoryReviewInput struct {
	Event    Event    `json:"event"`
	Memories []Memory `json:"memories"` // Includes tombstones to preserve identity and versions.
}

type MemoryReview struct {
	Changes  []MemoryChange `json:"changes"`
	Question string         `json:"question"` // Only when clarification is necessary.
}
