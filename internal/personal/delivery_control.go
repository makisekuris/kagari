package personal

import "time"

type DeliveryControlAction string

const (
	DeliveryStop   DeliveryControlAction = "stop"
	DeliveryResume DeliveryControlAction = "resume"
	DeliveryResend DeliveryControlAction = "resend"
)

// A Task stop applies across revisions until an explicit resume. Resume permits
// future deliveries; already suppressed content requires a new explicit resend.
// A delivery target supports stop or resend, never resurrection of its old items.
type DeliveryControlRequest struct {
	Action     DeliveryControlAction `json:"action"`
	TaskID     int64                 `json:"task_id,omitempty"`
	DeliveryID int64                 `json:"delivery_id,omitempty"`
}

type DeliveryControlResult struct {
	ID         int64                  `json:"id"`
	Request    DeliveryControlRequest `json:"request"`
	DeliveryID int64                  `json:"delivery_id"`
	CreatedAt  time.Time              `json:"created_at"`
}

type DeliveryQuery struct {
	TaskID   int64 `json:"task_id,omitempty"`
	BeforeID int64 `json:"before_id,omitempty"`
	Limit    int   `json:"limit,omitempty"`
}

const (
	DefaultDeliveryPageSize = 20
	MaxDeliveryPageSize     = 100
)

type DeliveryPage struct {
	Items          []DeliveryIntent `json:"items"`
	StoppedTaskIDs []int64          `json:"stopped_task_ids"`
	NextBeforeID   int64            `json:"next_before_id,omitempty"`
}
