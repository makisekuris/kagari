package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"kagari/internal/channel"
	"kagari/internal/textpage"
)

// TaskInputReference points to an immutable user event assigned to one run.
type TaskInputReference struct {
	EventID     int64                `json:"event_id"`
	Kind        string               `json:"kind"`
	Order       int                  `json:"order"`
	TextRunes   int                  `json:"text_runes"`
	TextPreview string               `json:"text_preview,omitempty"`
	Attachments []channel.Attachment `json:"attachments,omitempty"`
}

type addTaskInputArgs struct {
	ActionID         string  `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. For corrections or retries of a failed operation reuse the returned action_id. Do not omit or change it; a different ID is a separate action and does not resolve the failure."`
	TaskID           int64   `json:"task_id"`
	ExpectedRevision int64   `json:"expected_revision"`
	InputEventIDs    []int64 `json:"input_event_ids"`
}

type addTaskInputTask struct {
	ID       int64      `json:"id"`
	Revision int64      `json:"revision"`
	Status   TaskStatus `json:"status"`
}

type addTaskInputReply struct {
	ActionID string            `json:"action_id,omitempty"`
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Task     *addTaskInputTask `json:"task,omitempty"`
	Inputs   []Event           `json:"inputs,omitempty"`
}

type TaskInputProcessingRepository interface {
	ReadTaskInput(context.Context, int64, int64) (Event, error)
}

type getTaskInputArgs struct {
	EventID int64  `json:"event_id"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit,omitempty"`
	Version string `json:"version,omitempty"`
}

type getTaskInputReply struct {
	OK    bool                `json:"ok"`
	Error string              `json:"error,omitempty"`
	Input *TaskInputReference `json:"input,omitempty"`
	Page  textpage.Page       `json:"page,omitempty"`
}

type reportTaskInputsArgs struct {
	EventIDs []int64 `json:"event_ids"`
}

type reportTaskInputsReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Count int    `json:"count,omitempty"`
}

// taskInputProcessingTools expose only assigned task input and explicit completion reports.
func taskInputProcessingTools(store TaskInputProcessingRepository, run Run, applied *[]int64) ([]tool.BaseTool, error) {
	if store == nil || run.ID <= 0 || run.TaskID <= 0 || run.Revision <= 0 || run.Kind != RunTask || applied == nil {
		return nil, fmt.Errorf("personal: task input processing requires a task run, reader, and result slot")
	}
	var input TaskInput
	if err := json.Unmarshal([]byte(run.Input), &input); err != nil {
		return nil, fmt.Errorf("personal: decode task input references: %w", err)
	}
	allowed := make(map[int64]TaskInputReference, len(input.InputEvents))
	for _, ref := range input.InputEvents {
		if ref.EventID <= 0 || ref.Kind == "" {
			return nil, fmt.Errorf("personal: invalid task input reference in run %d", run.ID)
		}
		if _, exists := allowed[ref.EventID]; exists {
			return nil, fmt.Errorf("personal: duplicate task input reference in run %d", run.ID)
		}
		allowed[ref.EventID] = ref
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	var appliedMu sync.Mutex
	readTool, err := utils.InferTool("get_task_input", "按 event_id 读取分配给当前 Run 的用户输入正文。默认读取首页；offset/limit 按 Unicode 码点分页，version 用于校验后续页。附件仅返回引用，使用 read_attachment 读取。", func(ctx context.Context, args getTaskInputArgs) (getTaskInputReply, error) {
		ref, ok := allowed[args.EventID]
		if !ok {
			return getTaskInputToolError(fmt.Errorf("event %d is not assigned to run %d", args.EventID, run.ID)), nil
		}
		event, err := store.ReadTaskInput(ctx, run.ID, args.EventID)
		if err != nil {
			var accessErr taskInputAccessError
			if errors.As(err, &accessErr) {
				return getTaskInputToolError(err), nil
			}
			return getTaskInputReply{}, err
		}
		page, err := textpage.Read(event.Text, textpage.Request{Offset: args.Offset, Limit: args.Limit, Version: args.Version}, false)
		if err != nil {
			return getTaskInputToolError(err), nil
		}
		ref.Attachments = event.Attachments
		reply := getTaskInputReply{OK: true, Input: &ref, Page: page}
		_, err = textpage.EncodeBounded(&reply, textpage.MaxBytes(ctx), reply.Page.Shrink)
		if err != nil {
			return getTaskInputToolError(err), nil
		}
		return reply, nil
	})
	if err != nil {
		return nil, err
	}
	processTool, err := utils.InferTool("report_task_inputs", "报告本阶段已经处理的 event_id。只能报告当前 Run 的 input_events；仅在阶段成功保存时计为 applied。未报告的输入会回到 pending。", func(_ context.Context, args reportTaskInputsArgs) (reportTaskInputsReply, error) {
		if len(args.EventIDs) == 0 {
			return reportTaskInputsToolError(fmt.Errorf("event_ids must not be empty")), nil
		}
		seen := make(map[int64]bool, len(args.EventIDs))
		for _, id := range args.EventIDs {
			if id <= 0 || seen[id] {
				return reportTaskInputsToolError(fmt.Errorf("event_ids must be positive and unique")), nil
			}
			if _, ok := allowed[id]; !ok {
				return reportTaskInputsToolError(fmt.Errorf("event %d is not assigned to run %d", id, run.ID)), nil
			}
			seen[id] = true
		}
		appliedMu.Lock()
		defer appliedMu.Unlock()
		reported := make(map[int64]bool, len(*applied)+len(args.EventIDs))
		for _, id := range *applied {
			reported[id] = true
		}
		for _, id := range args.EventIDs {
			if !reported[id] {
				*applied = append(*applied, id)
				reported[id] = true
			}
		}
		return reportTaskInputsReply{OK: true, Count: len(*applied)}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{readTool, processTool}, nil
}

func getTaskInputToolError(err error) getTaskInputReply {
	return getTaskInputReply{OK: false, Error: boundedTaskInputError(err)}
}

func reportTaskInputsToolError(err error) reportTaskInputsReply {
	return reportTaskInputsReply{OK: false, Error: boundedTaskInputError(err)}
}

func boundedTaskInputError(err error) string {
	message := err.Error()
	if utf8.RuneCountInString(message) <= 512 {
		return message
	}
	runes := []rune(message)
	return string(runes[:511]) + "…"
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
		if len(args.InputEventIDs) == 0 || len(args.InputEventIDs) > maxPendingInputs+1 {
			return fail(fmt.Errorf("input_event_ids must include the current event and up to %d pending inputs", maxPendingInputs))
		}
		allowed := make(map[int64]Event, len(p.input.PendingInputs))
		for _, event := range p.input.PendingInputs {
			allowed[event.ID] = event
		}
		selected := make([]Event, 0, len(args.InputEventIDs))
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
			Task:     &addTaskInputTask{ID: args.TaskID, Revision: math.MaxInt64, Status: TaskAwaitingReview},
			Inputs:   selected,
		}
		encodedReply, err := json.Marshal(preview)
		if err != nil {
			return fail(err)
		}
		if runeLen(string(encodedReply)) > maxTaskChars {
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
