package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/personal"
)

type addTaskInputArgs struct {
	ActionID         string  `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. For corrections or retries of a failed operation reuse the returned action_id. Do not omit or change it; a different ID is a separate action and does not resolve the failure."`
	TaskID           int64   `json:"task_id"`
	ExpectedRevision int64   `json:"expected_revision"`
	InputEventIDs    []int64 `json:"input_event_ids"`
}

type addTaskInputTask struct {
	ID       int64               `json:"id"`
	Revision int64               `json:"revision"`
	Status   personal.TaskStatus `json:"status"`
}

type addTaskInputReply struct {
	ActionID string            `json:"action_id,omitempty"`
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Task     *addTaskInputTask `json:"task,omitempty"`
	Inputs   []personal.Event  `json:"inputs,omitempty"`
}

// inputTools exposes only explicit same-task input addition.
func (p *personalTools) inputTools() ([]tool.BaseTool, error) {
	addInput, err := utils.InferTool("add_task_input", "为同一尚未完成、也未取消的任务补充当前消息和用户刚发送的纯链接消息；只选择 PendingInputs 中相关链接，不创建新目标。检查返回的 task.status：queued 或 running 时不要调用 continue_task，等待现有阶段结束；其他可继续状态下，成功添加的材料需要安排后续阶段。收到失败结果后修正参数并沿用返回的 action_id 重试。", func(ctx context.Context, args addTaskInputArgs) (addTaskInputReply, error) {
		if err := p.directUserMutation(); err != nil {
			return addTaskInputToolErrorWithID(args.ActionID, err), nil
		}
		actionArgs := args
		actionArgs.ActionID = ""
		attempt, err := p.beginTaskAction("add_task_input", args.ActionID, args.TaskID, p.taskActionName(args.TaskID), actionArgs)
		if err != nil {
			return addTaskInputToolErrorWithID(attempt.actionID, err), nil
		}
		fail := func(err error) (addTaskInputReply, error) {
			p.recordTaskActionFailure(attempt, err)
			return addTaskInputToolErrorWithID(attempt.actionID, err), nil
		}
		if len(args.InputEventIDs) == 0 || len(args.InputEventIDs) > personal.MaxPendingInputs+1 {
			return fail(fmt.Errorf("input_event_ids must include the current event and up to %d pending inputs", personal.MaxPendingInputs))
		}
		allowed := make(map[int64]personal.Event, len(p.input.PendingInputs))
		for _, event := range p.input.PendingInputs {
			allowed[event.ID] = event
		}
		selected := make([]personal.Event, 0, len(args.InputEventIDs))
		seen := make(map[int64]bool, len(args.InputEventIDs))
		includesCurrent := false
		for _, id := range args.InputEventIDs {
			if id <= 0 || seen[id] {
				return fail(fmt.Errorf("input event IDs must be positive and unique"))
			}
			seen[id] = true
			if id == p.event.ID {
				includesCurrent = true
				selected = append(selected, p.event)
				continue
			}
			event, ok := allowed[id]
			if !ok {
				return fail(fmt.Errorf("event %d is not an available pending URL input", id))
			}
			selected = append(selected, event)
		}
		if !includesCurrent {
			return fail(fmt.Errorf("input_event_ids must include the current event %d", p.event.ID))
		}
		preview := addTaskInputReply{
			ActionID: attempt.actionID,
			OK:       true,
			Task:     &addTaskInputTask{ID: args.TaskID, Revision: math.MaxInt64, Status: personal.TaskAwaitingReview},
			Inputs:   selected,
		}
		encodedReply, err := json.Marshal(preview)
		if err != nil {
			return fail(err)
		}
		if utf8.RuneCountInString(string(encodedReply)) > personal.MaxTaskChars {
			return fail(fmt.Errorf("selected input messages exceed the tool reply size limit"))
		}
		key, err := p.requestKey("add_task_input", actionArgs)
		if err != nil {
			return fail(err)
		}
		task, err := p.store.AddTaskInputs(ctx, p.event.ID, key, args.TaskID, args.ExpectedRevision, args.InputEventIDs)
		if err != nil {
			return fail(err)
		}
		p.addTaskID(task.ID)
		p.succeedTaskAction(attempt)
		return addTaskInputReply{ActionID: attempt.actionID, OK: true, Task: &addTaskInputTask{ID: task.ID, Revision: task.Revision, Status: task.Status}, Inputs: selected}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{addInput}, nil
}

func addTaskInputToolErrorWithID(actionID string, err error) addTaskInputReply {
	return addTaskInputReply{ActionID: actionID, OK: false, Error: taskActionErrorMessage(actionID, err)}
}
