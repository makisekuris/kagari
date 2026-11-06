package personal

import (
	"encoding/json"
	"fmt"
	"strings"
)

type taskActionFailure struct {
	ActionID  string `json:"action_id"`
	Operation string `json:"operation"`
	TaskID    int64  `json:"task_id,omitempty"`
	Target    string `json:"target"`
	Error     string `json:"error"`
}

type taskActionState struct {
	failure              taskActionFailure
	failurePayload       string
	failureArgs          any
	failureInputEventIDs map[int64]struct{}
	successPayload       string
	succeeded            bool
}

type taskActionAttempt struct {
	actionID      string
	payload       string
	args          any
	inputEventIDs map[int64]struct{}
}

func (p *personalTools) beginTaskAction(operation, actionID string, taskID int64, target string, args any) (taskActionAttempt, error) {
	actionID = p.taskActionID(actionID)
	attempt := taskActionAttempt{actionID: actionID, args: args, inputEventIDs: taskActionInputEventIDs(operation, args, p.event.ID)}
	encoded, err := taskActionPayload(operation, args)
	if err != nil {
		return attempt, fmt.Errorf("encode task action: %w", err)
	}
	attempt.payload = string(encoded)
	if p.taskActions == nil {
		p.taskActions = make(map[string]*taskActionState)
	}
	if previous := p.taskActions[actionID]; previous != nil {
		if previous.failure.Operation != operation {
			err := fmt.Errorf("action_id %q already belongs to %s", actionID, previous.failure.Operation)
			return p.rejectTaskAction(operation, taskID, target, attempt, err), err
		}
		if previous.failure.TaskID > 0 && taskID > 0 && previous.failure.TaskID != taskID {
			err := fmt.Errorf("action_id %q already targets another task", actionID)
			return p.rejectTaskAction(operation, taskID, target, attempt, err), err
		}
		if previous.succeeded && previous.successPayload != attempt.payload {
			err := fmt.Errorf("action_id %q already succeeded with different arguments", actionID)
			return p.rejectTaskAction(operation, taskID, target, attempt, err), err
		}
		if previous.failure.TaskID == 0 && taskID > 0 {
			previous.failure.TaskID = taskID
		}
		if target != "" && target != "该任务" && target != "新任务" {
			previous.failure.Target = target
		}
		previous.failurePayload = attempt.payload
		previous.failureArgs = attempt.args
		previous.failureInputEventIDs = attempt.inputEventIDs
		return attempt, nil
	}
	if target == "" {
		target = "该任务"
	}
	p.taskActions[actionID] = &taskActionState{
		failure:              taskActionFailure{ActionID: actionID, Operation: operation, TaskID: taskID, Target: target},
		failurePayload:       attempt.payload,
		failureArgs:          attempt.args,
		failureInputEventIDs: attempt.inputEventIDs,
	}
	p.taskActionOrder = append(p.taskActionOrder, actionID)
	return attempt, nil
}

func taskActionPayload(operation string, args any) ([]byte, error) {
	if operation == "update_task" {
		updateArgs := args.(reviseTaskArgs)
		return json.Marshal(struct {
			reviseTaskArgs
			Attachments  []string `json:"attachments"`
			SourceRunIDs []int64  `json:"source_run_ids"`
			MemoryRefs   []string `json:"memory_refs"`
		}{reviseTaskArgs: updateArgs, Attachments: updateArgs.Attachments, SourceRunIDs: updateArgs.SourceRunIDs, MemoryRefs: updateArgs.MemoryRefs})
	}
	return json.Marshal(args)
}

func (p *personalTools) taskActionID(actionID string) string {
	if actionID != "" {
		return actionID
	}
	for {
		p.nextTaskActionID++
		id := fmt.Sprintf("task-action-%d", p.nextTaskActionID)
		if p.taskActions[id] == nil {
			return id
		}
	}
}

func (p *personalTools) rejectTaskAction(operation string, taskID int64, target string, attempt taskActionAttempt, err error) taskActionAttempt {
	if target == "" {
		target = "该任务"
	}
	attempt.actionID = p.taskActionID("")
	p.taskActions[attempt.actionID] = &taskActionState{
		failure:              taskActionFailure{ActionID: attempt.actionID, Operation: operation, TaskID: taskID, Target: target, Error: err.Error()},
		failurePayload:       attempt.payload,
		failureArgs:          attempt.args,
		failureInputEventIDs: attempt.inputEventIDs,
	}
	p.taskActionOrder = append(p.taskActionOrder, attempt.actionID)
	return attempt
}

func (p *personalTools) failTaskAction(attempt taskActionAttempt, err error) personalToolReply {
	p.recordTaskActionFailure(attempt, err)
	return taskActionToolError(attempt.actionID, err)
}

func (p *personalTools) recordTaskActionFailure(attempt taskActionAttempt, err error) {
	action := p.taskActions[attempt.actionID]
	if !action.succeeded {
		action.failure.Error = err.Error()
	}
}

func (p *personalTools) succeedTaskAction(attempt taskActionAttempt) {
	action := p.taskActions[attempt.actionID]
	action.failure.Error = ""
	action.successPayload = attempt.payload
	action.succeeded = true
	for _, actionID := range p.taskActionOrder {
		previous := p.taskActions[actionID]
		if previous.failure.Error == "" || previous.failure.Operation != action.failure.Operation || previous.failure.TaskID != action.failure.TaskID {
			continue
		}
		if previous.failurePayload == attempt.payload || taskActionCorrectionMatches(previous.failureArgs, attempt.args) || action.failure.Operation == "add_task_input" && taskActionInputsCovered(previous.failureInputEventIDs, attempt.inputEventIDs) {
			previous.failure.Error = ""
		}
	}
}

func taskActionCorrectionMatches(failed, succeeded any) bool {
	switch old := failed.(type) {
	case createTaskArgs:
		current, ok := succeeded.(createTaskArgs)
		if !ok || old.Title != current.Title && !missingRequiredValue(old.Title, current.Title) || old.Goal != current.Goal && !missingRequiredValue(old.Goal, current.Goal) {
			return false
		}
		return (strings.TrimSpace(old.Title) != "" && old.Title == current.Title || strings.TrimSpace(old.Goal) != "" && old.Goal == current.Goal || strings.TrimSpace(old.Context) != "" && old.Context == current.Context) &&
			old.Context == current.Context && sameStringSlice(old.Attachments, current.Attachments) && sameInt64Slice(old.SourceRunIDs, current.SourceRunIDs) && sameStringSlice(old.MemoryRefs, current.MemoryRefs)
	case reviseTaskArgs:
		current, ok := succeeded.(reviseTaskArgs)
		return ok && old.TaskID == current.TaskID && (old.Goal == current.Goal || missingRequiredValue(old.Goal, current.Goal)) && old.Context == current.Context &&
			sameStringSlice(old.Attachments, current.Attachments) && sameInt64Slice(old.SourceRunIDs, current.SourceRunIDs) && sameStringSlice(old.MemoryRefs, current.MemoryRefs)
	case continueTaskArgs:
		current, ok := succeeded.(continueTaskArgs)
		return ok && old.TaskID == current.TaskID && (old.Step == current.Step || missingRequiredValue(old.Step, current.Step)) && old.ExtendBudget == current.ExtendBudget
	default:
		return false
	}
}

func missingRequiredValue(old, current string) bool {
	return strings.TrimSpace(old) == "" && strings.TrimSpace(current) != ""
}

func sameStringSlice(left, right []string) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sameInt64Slice(left, right []int64) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func taskActionInputEventIDs(operation string, args any, currentEventID int64) map[int64]struct{} {
	if operation != "add_task_input" {
		return nil
	}
	inputArgs := args.(addTaskInputArgs)
	ids := make(map[int64]struct{}, len(inputArgs.InputEventIDs)+1)
	if currentEventID > 0 {
		ids[currentEventID] = struct{}{}
	}
	for _, id := range inputArgs.InputEventIDs {
		if id > 0 {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func taskActionInputsCovered(required, selected map[int64]struct{}) bool {
	for id := range required {
		if _, ok := selected[id]; !ok {
			return false
		}
	}
	return true
}

func (p *personalTools) unresolvedTaskActions() []taskActionFailure {
	var failures []taskActionFailure
	for _, actionID := range p.taskActionOrder {
		action := p.taskActions[actionID]
		if action.failure.Error != "" {
			failures = append(failures, action.failure)
		}
	}
	return failures
}

func (p *personalTools) taskActionName(taskID int64) string {
	if task := p.taskByID(taskID); task != nil && strings.TrimSpace(task.Title) != "" {
		return strings.TrimSpace(task.Title)
	}
	return ""
}

func taskActionTarget(title string) string {
	if title = strings.TrimSpace(title); title != "" {
		return title
	}
	return "新任务"
}

func taskActionToolError(actionID string, err error) personalToolReply {
	return personalToolReply{ActionID: actionID, OK: false, Error: err.Error()}
}
