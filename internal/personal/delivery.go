package personal

import (
	"fmt"
	"time"
)

type DeliveryPurpose string

const (
	DeliveryResult     DeliveryPurpose = "result"
	DeliveryStatus     DeliveryPurpose = "status"
	DeliveryControl    DeliveryPurpose = "control"
	DeliveryHistorical DeliveryPurpose = "historical"
)

// DeliveryDraft fixes the requested content before settlement commits. An empty
// Text is valid for an attachment-only delivery; attachment order is significant.
type DeliveryDraft struct {
	Text        string   `json:"text"`
	Attachments []string `json:"attachments"`
}

type TaskSettlement struct {
	Status     TaskStatus       `json:"status"`
	Summary    string           `json:"summary"`
	Delivery   DeliveryDraft    `json:"delivery"`
	Assessment *CheckAssessment `json:"check_assessment,omitempty"`
}

// SupplementalReply carries a separate answer from a turn that also committed
// a control. It has its own scope; it never replaces the saved control payload.
// Runtime prepares Chunks before Store completes the source event.
type SupplementalReply struct {
	Draft      DeliveryDraft   `json:"draft"`
	TaskIDs    []int64         `json:"task_ids,omitempty"`
	ScheduleID int64           `json:"schedule_id,omitempty"`
	Purpose    DeliveryPurpose `json:"purpose"`
	Chunks     []string        `json:"-"`
}

// DeliveryScope captures the task version when content was committed. RunID is
// absent when the source is a direct user action without an execution result.
type DeliveryScope struct {
	TaskID   int64 `json:"task_id"`
	Revision int64 `json:"revision"`
	RunID    int64 `json:"run_id,omitempty"`
}

type DeliveryIntentState string

const (
	DeliveryIntentPending    DeliveryIntentState = "pending"
	DeliveryIntentRendered   DeliveryIntentState = "rendered"
	DeliveryIntentSuppressed DeliveryIntentState = "suppressed"
)

// DeliveryIntent records generation and preparation, not channel acceptance.
// Each outbox item owns its own send outcome and receipt.
type DeliveryIntent struct {
	ID                 int64               `json:"id"`
	EventID            int64               `json:"event_id"`
	ScheduleID         int64               `json:"schedule_id,omitempty"`
	OriginalDeliveryID int64               `json:"original_delivery_id,omitempty"`
	Purpose            DeliveryPurpose     `json:"purpose"`
	Draft              DeliveryDraft       `json:"draft"`
	Scopes             []DeliveryScope     `json:"scopes"`
	State              DeliveryIntentState `json:"state"`
	Detail             string              `json:"detail,omitempty"`
	PreparationError   string              `json:"preparation_error,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
}

// RenderedDelivery can change presentation, never the persisted file selection.
// AttachmentFailure requests the unavailable-file fallback; Store verifies the
// failure and preserves the original attachment IDs and its reason.
type RenderedDelivery struct {
	DeliveryID        int64
	Text              string
	Chunks            []string
	AttachmentFailure bool
}

// DeliveryAttachmentError identifies which saved intent needs the unavailable
// attachment reply when an event contains several independent deliveries.
type DeliveryAttachmentError struct {
	DeliveryID int64
	Err        error
}

func (e *DeliveryAttachmentError) Error() string {
	return fmt.Sprintf("personal: delivery %d attachment: %v", e.DeliveryID, e.Err)
}

func (e *DeliveryAttachmentError) Unwrap() error { return e.Err }
