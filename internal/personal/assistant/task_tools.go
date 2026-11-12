package assistant

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/personal"
	"kagari/internal/personal/agentctx"
)

// Task tools own direct-user task mutations and task/run queries.
func (p *personalTools) taskTools() ([]tool.BaseTool, error) {
	createTask, err := utils.InferTool("create_task", "为明确的新目标创建后台任务并排队；只允许直接用户事件。收到失败结果后修正参数并沿用返回的 action_id 重试。", func(ctx context.Context, a createTaskArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return taskActionToolError(a.ActionID, err), nil
		}
		actionArgs := a
		actionArgs.ActionID = ""
		attempt, err := p.beginTaskAction("create_task", a.ActionID, 0, taskActionTarget(a.Title), actionArgs)
		if err != nil {
			return taskActionToolError(attempt.actionID, err), nil
		}
		fail := func(err error) (personalToolReply, error) { return p.failTaskAction(attempt, err), nil }
		if strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Goal) == "" {
			return fail(fmt.Errorf("title and goal are required"))
		}
		attachments, err := agentctx.ResolveAttachments(ctx, p.store, a.Attachments)
		if err != nil {
			return fail(err)
		}
		key, err := p.requestKey("create_task", actionArgs)
		if err != nil {
			return fail(err)
		}
		task, run, err := p.store.CreateTask(ctx, p.event.ID, key, a.Title, personal.TaskInput{Goal: a.Goal, Context: a.Context, Attachments: attachments, SourceRunIDs: a.SourceRunIDs, MemoryRefs: a.MemoryRefs})
		if err != nil {
			return fail(err)
		}
		if task.ID <= 0 {
			return fail(fmt.Errorf("task creation returned no task"))
		}
		p.addTaskID(task.ID)
		p.taskResultCommitted = true
		purpose := personal.DeliveryStatus
		if run.Status != personal.RunQueued {
			purpose = personal.DeliveryResult
		}
		p.addSupplementalTask(task.ID, purpose)
		p.succeedTaskAction(attempt)
		return personalToolReply{ActionID: attempt.actionID, OK: true, Queued: run.Status == personal.RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	listTasks, err := utils.InferTool("list_tasks", "按关键词、更新时间及 ID 游标查找任务；后页同时传回 next_before_id、snapshot_max_id、query_key，并保持查询条件。goal/context 全文使用 read_task。", func(ctx context.Context, a personal.TaskQuery) (personal.TaskPage, error) {
		page, err := p.store.QueryTasks(ctx, personal.ReadScope{EventID: p.event.ID}, a)
		if err != nil {
			return personal.TaskPage{}, err
		}
		purpose := personal.DeliveryControl
		for _, task := range page.Tasks {
			p.addTaskID(task.ID)
			if task.Summary != "" {
				purpose = personal.DeliveryResult
			}
			p.addSupplementalTask(task.ID, purpose)
		}
		p.recordSupplementalWork(purpose)
		return page, nil
	})
	if err != nil {
		return nil, err
	}

	getTask, err := utils.InferTool("get_task", "按任务 ID 查看目标、版本、状态、阶段成果与剩余预算。", func(ctx context.Context, a taskArgs) (personalToolReply, error) {
		if a.TaskID <= 0 {
			return toolError(fmt.Errorf("task_id must be positive")), nil
		}
		task, err := p.store.Task(ctx, a.TaskID)
		if err != nil {
			return toolError(err), nil
		}
		if task == nil {
			return toolError(fmt.Errorf("task not found")), nil
		}
		p.addTaskID(task.ID)
		purpose := personal.DeliveryControl
		if task.Summary != "" {
			purpose = personal.DeliveryResult
		}
		p.addSupplementalTask(task.ID, purpose)
		card := personal.TaskCard(*task)
		return personalToolReply{OK: true, Task: &card}, nil
	})
	if err != nil {
		return nil, err
	}

	updateTask, err := utils.InferTool("update_task", "由直接用户事件更新任务目标，并排队新版本。使用当前 revision；省略 attachments 保留原材料，空数组清除选定材料。收到失败结果后修正参数并沿用返回的 action_id 重试。", func(ctx context.Context, a reviseTaskArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return taskActionToolError(a.ActionID, err), nil
		}
		actionArgs := a
		actionArgs.ActionID = ""
		attempt, err := p.beginTaskAction("update_task", a.ActionID, a.TaskID, p.taskActionName(a.TaskID), actionArgs)
		if err != nil {
			return taskActionToolError(attempt.actionID, err), nil
		}
		fail := func(err error) (personalToolReply, error) { return p.failTaskAction(attempt, err), nil }
		if a.TaskID <= 0 || a.ExpectedRevision <= 0 || strings.TrimSpace(a.Goal) == "" {
			return fail(fmt.Errorf("task_id, expected_revision and goal are required"))
		}
		attachments, err := agentctx.ResolveAttachments(ctx, p.store, a.Attachments)
		if err != nil {
			return fail(err)
		}
		if a.Attachments == nil {
			current, err := p.store.Task(ctx, a.TaskID)
			if err != nil {
				return fail(err)
			}
			if current == nil {
				return fail(fmt.Errorf("task not found"))
			}
			attachments = current.Attachments
		}
		key, err := p.requestKey("update_task", actionArgs)
		if err != nil {
			return fail(err)
		}
		task, run, err := p.store.ReviseTask(ctx, p.event.ID, key, a.TaskID, a.ExpectedRevision, personal.TaskInput{Goal: a.Goal, Context: a.Context, Attachments: attachments, SourceRunIDs: a.SourceRunIDs, MemoryRefs: a.MemoryRefs})
		if err != nil {
			return fail(err)
		}
		if task.ID <= 0 {
			return fail(fmt.Errorf("task update returned no task"))
		}
		p.addTaskID(task.ID)
		p.taskResultCommitted = true
		purpose := personal.DeliveryStatus
		if run.Status != personal.RunQueued {
			purpose = personal.DeliveryResult
		}
		p.addSupplementalTask(task.ID, purpose)
		p.succeedTaskAction(attempt)
		return personalToolReply{ActionID: attempt.actionID, OK: true, Queued: run.Status == personal.RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	cancelTask, err := utils.InferTool("cancel_task", "由直接用户事件取消任务执行并保留已生成成果及待发交付。取消计算不等于停止投递；用户明确要求不要再发送时另用 stop_delivery。结果中的 deliveries 是本事件已保存的全部控制确认，可能包含同轮其他控制操作；不要假定其中某一项专属于取消任务。", func(ctx context.Context, a cancelTaskArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		p.mustNotify = true
		if a.TaskID <= 0 || a.ExpectedRevision <= 0 {
			return toolError(fmt.Errorf("task_id and expected_revision are required")), nil
		}
		task, err := p.store.CancelTask(ctx, p.event.ID, a.TaskID, a.ExpectedRevision)
		if err != nil {
			return toolError(err), nil
		}
		if task.ID <= 0 {
			return toolError(fmt.Errorf("task cancellation returned no task")), nil
		}
		p.addTaskID(task.ID)
		p.persistedControl = true
		p.mustNotify = true
		if err := p.captureTaskCancellationDeliveries(ctx); err != nil {
			return toolError(err), nil
		}
		deliveries := p.cancellationDeliveryPage()
		return personalToolReply{OK: true, Task: &task, Deliveries: &deliveries}, nil
	})
	if err != nil {
		return nil, err
	}

	continueTask, err := utils.InferTool("continue_task", "仅在任务状态允许时于原目标内排队下一阶段。先检查 task.status：queued 或 running 已有阶段，不要重复继续；completed 不能继续。waiting_budget 不能从任务回调恢复。extend_budget=true 仅在直接用户明确授权后才能设置；任务回调只能继续自己的任务且不能扩展预算。收到失败结果后修正参数并沿用返回的 action_id 重试。", func(ctx context.Context, a continueTaskArgs) (personalToolReply, error) {
		var attempt taskActionAttempt
		tracking := false
		replyActionID := a.ActionID
		switch p.event.Kind {
		case personal.EventUser:
			if err := p.directUserMutation(); err != nil {
				return taskActionToolError(a.ActionID, err), nil
			}
			actionArgs := a
			actionArgs.ActionID = ""
			var err error
			attempt, err = p.beginTaskAction("continue_task", a.ActionID, a.TaskID, p.taskActionName(a.TaskID), actionArgs)
			if err != nil {
				return taskActionToolError(attempt.actionID, err), nil
			}
			tracking = true
			replyActionID = attempt.actionID
		case personal.EventTask:
			if err := p.ownTask(a.TaskID); err != nil {
				return taskActionToolError(a.ActionID, err), nil
			}
			if a.ExtendBudget {
				p.callbackFailure = p.callbackContinuationFailure(a.TaskID)
				return taskActionToolError(a.ActionID, fmt.Errorf("task callbacks cannot extend the budget")), nil
			}
			if task := p.taskByID(a.TaskID); task != nil && (task.Status == personal.TaskWaitingBudget || task.BudgetMS > 0 && task.UsedMS >= task.BudgetMS) {
				p.callbackFailure = p.callbackContinuationFailure(a.TaskID)
				return taskActionToolError(a.ActionID, fmt.Errorf("task budget is exhausted; ask the user to continue")), nil
			}
		default:
			return taskActionToolError(a.ActionID, fmt.Errorf("continuing tasks requires a direct user event or task callback")), nil
		}
		fail := func(err error) (personalToolReply, error) {
			if tracking {
				return p.failTaskAction(attempt, err), nil
			}
			if p.event.Kind == personal.EventTask {
				p.callbackFailure = p.callbackContinuationFailure(a.TaskID)
			}
			return taskActionToolError(a.ActionID, err), nil
		}
		if a.TaskID <= 0 || a.ExpectedRevision <= 0 || strings.TrimSpace(a.Step) == "" {
			return fail(fmt.Errorf("task_id, expected_revision and step are required"))
		}
		actionArgs := a
		actionArgs.ActionID = ""
		key, err := p.requestKey("continue_task", actionArgs)
		if err != nil {
			return fail(err)
		}
		task, run, err := p.store.ContinueTask(ctx, p.event.ID, key, a.TaskID, a.ExpectedRevision, a.Step, a.ExtendBudget)
		if err != nil {
			return fail(err)
		}
		if task.ID <= 0 {
			return fail(fmt.Errorf("task continuation returned no task"))
		}
		p.addTaskID(task.ID)
		p.taskResultCommitted = true
		purpose := personal.DeliveryStatus
		if run.Status != personal.RunQueued {
			purpose = personal.DeliveryResult
		}
		p.addSupplementalTask(task.ID, purpose)
		if tracking {
			p.succeedTaskAction(attempt)
		} else if p.event.Kind == personal.EventTask {
			p.callbackFailure = nil
		}
		return personalToolReply{ActionID: replyActionID, OK: true, Queued: run.Status == personal.RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	settleTask, err := utils.InferTool("settle_task", "在同一事务中结束当前任务并保存交付意图。status 只能为 completed、waiting_input 或 failed。summary 是保存在任务上的内部成果摘要，必须与 delivery 分开填写。delivery.text 是面向用户的结案消息，写实际结论、失败原因或必要问题及来源链接；delivery.attachments 必须显式提供有序文件 ID 列表，无附件时传 []。仅附件交付可将 text 设为空，但 summary 仍不能为空。先显式选择要发送的文件；保存过的文件不会自动成为交付物。此操作保存生成意图，不代表渠道已接受发送。", func(ctx context.Context, a settleTaskArgs) (personalToolReply, error) {
		if err := p.ownTask(a.TaskID); err != nil {
			return toolError(err), nil
		}
		if a.ExpectedRevision <= 0 || strings.TrimSpace(a.Summary) == "" {
			return toolError(fmt.Errorf("expected_revision and a nonblank task summary are required")), nil
		}
		if a.Status != personal.TaskCompleted && a.Status != personal.TaskWaitingInput && a.Status != personal.TaskFailed {
			return toolError(fmt.Errorf("status must be completed, waiting_input or failed")), nil
		}
		if a.Delivery.Attachments == nil {
			return toolError(fmt.Errorf("delivery.attachments is required; pass [] when no files should be sent")), nil
		}
		deliveryText := a.Delivery.Text
		if required := p.requiredTaskReply(); required != "" {
			if strings.TrimSpace(deliveryText) == "" {
				deliveryText = required
			} else {
				deliveryText += "\n\n" + required
			}
		}
		if strings.TrimSpace(deliveryText) == "" {
			if len(a.Delivery.Attachments) == 0 {
				return toolError(fmt.Errorf("delivery needs text or at least one selected attachment")), nil
			}
			deliveryText = ""
		}
		files, err := agentctx.ResolveAttachments(ctx, p.store, a.Delivery.Attachments)
		if err != nil {
			return toolError(fmt.Errorf("delivery attachment: %w", err)), nil
		}
		deliveryAttachments := make([]string, 0, len(files))
		for _, file := range files {
			deliveryAttachments = append(deliveryAttachments, file.ID)
		}
		settlement := personal.TaskSettlement{
			Status:     a.Status,
			Summary:    a.Summary,
			Delivery:   personal.DeliveryDraft{Text: deliveryText, Attachments: deliveryAttachments},
			Assessment: a.Assessment,
		}
		task, err := p.store.SettleTask(ctx, p.event.ID, a.TaskID, a.ExpectedRevision, settlement)
		if err != nil {
			return toolError(err), nil
		}
		if task.ID <= 0 {
			return toolError(fmt.Errorf("task settlement returned no task")), nil
		}
		p.addTaskID(task.ID)
		p.taskResultCommitted = true
		p.mustNotify = true
		draft := settlement.Delivery
		draft.Attachments = append([]string{}, settlement.Delivery.Attachments...)
		p.settledDelivery = &draft
		return personalToolReply{OK: true, Task: &task}, nil
	})
	if err != nil {
		return nil, err
	}

	getRun, err := utils.InferTool("get_run", "按 Run ID 查看一次后台执行结果。", func(ctx context.Context, a runArgs) (personalToolReply, error) {
		if a.RunID <= 0 {
			return toolError(fmt.Errorf("run_id must be positive")), nil
		}
		run, err := p.store.Run(ctx, a.RunID)
		if err != nil {
			return toolError(err), nil
		}
		if run == nil {
			return toolError(fmt.Errorf("run not found")), nil
		}
		p.addTaskID(run.TaskID)
		purpose := personal.DeliveryControl
		if run.Result != "" {
			purpose = personal.DeliveryResult
		}
		p.addSupplementalTask(run.TaskID, purpose)
		card := personal.RunCard(*run)
		return personalToolReply{OK: true, Run: &card}, nil
	})
	if err != nil {
		return nil, err
	}

	listRuns, err := utils.InferTool("list_runs", "按 ID 游标列出执行摘要；后页同时传回 next_before_id、snapshot_max_id、query_key。全文使用 read_run，来源使用 list_materials。", func(ctx context.Context, a struct {
		TaskID int64 `json:"task_id"`
		personal.ListPage
	}) (personal.RunPage, error) {
		page, err := p.store.QueryRuns(ctx, personal.ReadScope{EventID: p.event.ID}, a.TaskID, a.ListPage)
		if err != nil {
			return personal.RunPage{}, err
		}
		p.addTaskID(a.TaskID)
		purpose := personal.DeliveryControl
		for _, run := range page.Runs {
			if run.Result != "" {
				purpose = personal.DeliveryResult
			}
		}
		p.addSupplementalTask(a.TaskID, purpose)
		return page, nil
	})
	if err != nil {
		return nil, err
	}
	taskTools := []tool.BaseTool{createTask, listTasks, getTask, updateTask, cancelTask, continueTask}
	if p.event.Kind == personal.EventTask {
		taskTools = append(taskTools, settleTask)
	}
	taskTools = append(taskTools, getRun, listRuns)
	return taskTools, nil
}
