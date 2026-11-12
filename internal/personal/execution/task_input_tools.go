package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/textclip"
	"kagari/internal/textpage"
)

type getTaskInputArgs struct {
	EventID int64  `json:"event_id"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit,omitempty"`
	Version string `json:"version,omitempty"`
}

type getTaskInputReply struct {
	OK    bool                         `json:"ok"`
	Error string                       `json:"error,omitempty"`
	Input *personal.TaskInputReference `json:"input,omitempty"`
	Page  textpage.Page                `json:"page,omitempty"`
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
func taskInputProcessingTools(store personal.TaskInputProcessingRepository, run personal.Run, applied *[]int64) ([]tool.BaseTool, error) {
	if store == nil || run.ID <= 0 || run.TaskID <= 0 || run.Revision <= 0 || run.Kind != personal.RunTask || applied == nil {
		return nil, fmt.Errorf("personal: task input processing requires a task run, reader, and result slot")
	}
	var input personal.TaskInput
	if err := json.Unmarshal([]byte(run.Input), &input); err != nil {
		return nil, fmt.Errorf("personal: decode task input references: %w", err)
	}
	allowed := make(map[int64]personal.TaskInputReference, len(input.InputEvents))
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
			if errors.Is(err, personal.ErrTaskInputAccess) {
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
	return getTaskInputReply{OK: false, Error: textclip.Truncate(err.Error(), 512)}
}

func reportTaskInputsToolError(err error) reportTaskInputsReply {
	return reportTaskInputsReply{OK: false, Error: textclip.Truncate(err.Error(), 512)}
}
