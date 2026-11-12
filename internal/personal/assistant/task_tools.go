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
		p.succeedTaskAction(attempt)
		return personalToolReply{ActionID: attempt.actionID, OK: true, Queued: run.Status == personal.RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	listTasks, err := utils.InferTool("list_tasks", "按关键词、更新时间及 ID 游标查找任务；后页同时传回 next_before_id、snapshot_max_id、query_key，并保持查询条件。goal/context 全文使用 read_task。", func(ctx context.Context, a personal.TaskQuery) (personal.TaskPage, error) {
		return p.store.QueryTasks(ctx, personal.ReadScope{EventID: p.event.ID}, a)
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
		p.succeedTaskAction(attempt)
		return personalToolReply{ActionID: attempt.actionID, OK: true, Queued: run.Status == personal.RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	cancelTask, err := utils.InferTool("cancel_task", "由直接用户事件取消指定版本的任务。", func(ctx context.Context, a cancelTaskArgs) (personalToolReply, error) {
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
		return personalToolReply{OK: true, Task: &task}, nil
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
				return taskActionToolError(a.ActionID, fmt.Errorf("task callbacks cannot extend the budget")), nil
			}
			if task := p.taskByID(a.TaskID); task != nil && (task.Status == personal.TaskWaitingBudget || task.BudgetMS > 0 && task.UsedMS >= task.BudgetMS) {
				return taskActionToolError(a.ActionID, fmt.Errorf("task budget is exhausted; ask the user to continue")), nil
			}
		default:
			return taskActionToolError(a.ActionID, fmt.Errorf("continuing tasks requires a direct user event or task callback")), nil
		}
		fail := func(err error) (personalToolReply, error) {
			if tracking {
				return p.failTaskAction(attempt, err), nil
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
		if tracking {
			p.succeedTaskAction(attempt)
		}
		return personalToolReply{ActionID: replyActionID, OK: true, Queued: run.Status == personal.RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	settleTask, err := utils.InferTool("settle_task", "结束当前任务回调的任务。status 只能为 completed、waiting_input 或 failed。summary 将直接发给用户：写实际结论、失败原因或必要问题及来源链接，不复制内部编号、工具调用或检查点日志。", func(ctx context.Context, a settleTaskArgs) (personalToolReply, error) {
		if err := p.ownTask(a.TaskID); err != nil {
			return toolError(err), nil
		}
		if a.ExpectedRevision <= 0 || strings.TrimSpace(a.Summary) == "" {
			return toolError(fmt.Errorf("expected_revision and summary are required")), nil
		}
		if a.Status != personal.TaskCompleted && a.Status != personal.TaskWaitingInput && a.Status != personal.TaskFailed {
			return toolError(fmt.Errorf("status must be completed, waiting_input or failed")), nil
		}
		task, err := p.store.SettleTask(ctx, p.event.ID, a.TaskID, a.ExpectedRevision, a.Status, a.Summary, a.Assessment)
		if err != nil {
			return toolError(err), nil
		}
		if task.ID <= 0 {
			return toolError(fmt.Errorf("task settlement returned no task")), nil
		}
		p.addTaskID(task.ID)
		p.mustNotify = true
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
		p.addTaskID(a.TaskID)
		return p.store.QueryRuns(ctx, personal.ReadScope{EventID: p.event.ID}, a.TaskID, a.ListPage)
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
