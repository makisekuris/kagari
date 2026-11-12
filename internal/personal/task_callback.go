package personal

import "errors"

// TaskCallbackState describes a persisted callback's relationship to the task's
// latest execution. Continued callbacks may finish their own handoff, but cannot
// make another task decision.
type TaskCallbackState string

const (
	TaskCallbackCurrent   TaskCallbackState = "current"
	TaskCallbackContinued TaskCallbackState = "continued"
	TaskCallbackStale     TaskCallbackState = "stale"
)

// ErrStaleTaskCallback is an expected result when a newer execution, revised
// goal, or cancellation has superseded the callback. Other authorization and
// persistence failures must remain errors in their own right.
var ErrStaleTaskCallback = errors.New("personal: stale task callback")
