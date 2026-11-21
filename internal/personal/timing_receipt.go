package personal

import "time"

// TimingReceipt describes a plan or timer as recorded by a user event.
type TimingReceipt struct {
	ID       int64
	Reminder bool
	Title    string
	Status   string
	Next     *time.Time
	Timezone string
}
