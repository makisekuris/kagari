package personal

import "time"

const (
	DefaultReminderPageSize = 20
	MaxReminderPageSize     = 100
	MaxReminderSteps        = 32
	MaxReminderTitleChars   = 200
	// 200 runes fit under the channel's 2000-byte part limit even when every
	// character uses four UTF-8 bytes.
	MaxReminderTextChars = 200
)

// ReminderStep is one notification in a relative sequence. AfterMinutes is
// the delay before this step: from StartAt for the first step, and from the
// preceding step for every later step. Calendar reminders use one step with a
// fixed text and AfterMinutes set to zero.
type ReminderStep struct {
	AfterMinutes int    `json:"after_minutes"`
	Text         string `json:"text"`
}

type ReminderInput struct {
	Title    string         `json:"title"`
	Steps    []ReminderStep `json:"steps"`
	Repeat   bool           `json:"repeat"`
	StartAt  *time.Time     `json:"start_at,omitempty"`
	Cron     string         `json:"cron,omitempty"`
	Timezone string         `json:"timezone,omitempty"`
}

type Reminder struct {
	ID             int64          `json:"id"`
	Title          string         `json:"title"`
	Steps          []ReminderStep `json:"steps"`
	Repeat         bool           `json:"repeat"`
	StartAt        time.Time      `json:"start_at"`
	Status         string         `json:"status"`
	NextRunAt      *time.Time     `json:"next_run_at,omitempty"`
	NextStep       int            `json:"next_step"`
	CreatedEventID int64          `json:"created_event_id"`
	LastDeliveryID int64          `json:"last_delivery_id,omitempty"`
	Cron           string         `json:"cron,omitempty"`
	Timezone       string         `json:"timezone,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}
