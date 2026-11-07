package personal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"go.uber.org/zap"
)

type personalToolReply struct {
	ActionID string   `json:"action_id,omitempty"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
	Queued   bool     `json:"queued,omitempty"`
	Task     *Task    `json:"task,omitempty"`
	Tasks    []Task   `json:"tasks,omitempty"`
	Run      *Run     `json:"run,omitempty"`
	Runs     []Run    `json:"runs,omitempty"`
	Memory   *Memory  `json:"memory,omitempty"`
	Memories []Memory `json:"memories,omitempty"`
}

type createTaskArgs struct {
	ActionID     string   `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. For corrections or retries of a failed operation reuse the returned action_id. Do not omit or change it; a different ID is a separate action and does not resolve the failure."`
	SourceRunIDs []int64  `json:"source_run_ids,omitempty"`
	MemoryRefs   []string `json:"memory_refs,omitempty"`
	Title        string   `json:"title"`
	Goal         string   `json:"goal"`
	Context      string   `json:"context"`
	Attachments  []string `json:"attachments,omitempty"`
}

type taskArgs struct {
	TaskID int64 `json:"task_id"`
}

type reviseTaskArgs struct {
	ActionID         string   `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. For corrections or retries of a failed operation reuse the returned action_id. Do not omit or change it; a different ID is a separate action and does not resolve the failure."`
	SourceRunIDs     []int64  `json:"source_run_ids,omitempty"`
	MemoryRefs       []string `json:"memory_refs,omitempty"`
	TaskID           int64    `json:"task_id"`
	ExpectedRevision int64    `json:"expected_revision"`
	Goal             string   `json:"goal"`
	Context          string   `json:"context"`
	Attachments      []string `json:"attachments,omitempty"`
}

type cancelTaskArgs struct {
	TaskID           int64 `json:"task_id"`
	ExpectedRevision int64 `json:"expected_revision"`
}

type continueTaskArgs struct {
	ActionID         string `json:"action_id,omitempty" jsonschema:"description=Omit for a new operation. For corrections or retries of a failed operation reuse the returned action_id. Do not omit or change it; a different ID is a separate action and does not resolve the failure."`
	TaskID           int64  `json:"task_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Step             string `json:"step"`
	ExtendBudget     bool   `json:"extend_budget"`
}

type settleTaskArgs struct {
	Assessment       *CheckAssessment `json:"check_assessment,omitempty"`
	TaskID           int64            `json:"task_id"`
	ExpectedRevision int64            `json:"expected_revision"`
	Status           TaskStatus       `json:"status"`
	Summary          string           `json:"summary" jsonschema:"description=The task closing message sent directly to the user. Write plain text with direct source URLs; do not use Markdown headings or bold or link syntax. Do not claim internal stage findings were already reported unless the chat history contains that report."`
}

type runArgs struct {
	RunID int64 `json:"run_id"`
}

type listRunsArgs struct {
	TaskID int64 `json:"task_id"`
}

type memorySearchArgs struct {
	Query string `json:"query"`
}

type memoryKeyArgs struct {
	Key string `json:"key"`
}

type memoryPutArgs struct {
	Key             string `json:"key"`
	Value           string `json:"value"`
	ExpectedVersion int64  `json:"expected_version"`
	Evidence        string `json:"evidence"`
}

type memoryForgetArgs struct {
	Key             string `json:"key"`
	ExpectedVersion int64  `json:"expected_version"`
	Evidence        string `json:"evidence"`
}

type memoryHistoryArgs struct {
	Key string `json:"key,omitempty"`
}

type personalTools struct {
	timezone         string
	scheduleIDs      map[int64]struct{}
	scheduleChanged  bool
	store            Repository
	log              *zap.Logger
	input            TurnInput
	event            Event
	taskIDs          map[int64]struct{}
	mustNotify       bool
	questions        []string
	taskActions      map[string]*taskActionState
	taskActionOrder  []string
	nextTaskActionID uint64
}

func (p *personalTools) addScheduleID(id int64, changed bool) {
	if id <= 0 {
		return
	}
	if changed && !p.scheduleChanged {
		p.scheduleIDs = nil
		p.scheduleChanged = true
	} else if !changed && p.scheduleChanged {
		return
	}
	if p.scheduleIDs == nil {
		p.scheduleIDs = make(map[int64]struct{})
	}
	p.scheduleIDs[id] = struct{}{}
}

func (p *personalTools) scheduleID() int64 {
	if len(p.scheduleIDs) == 0 {
		if p.taskID() != p.event.TaskID {
			return 0
		}
		return p.event.ScheduleID
	}
	if len(p.scheduleIDs) == 1 {
		for id := range p.scheduleIDs {
			return id
		}
	}
	return 0
}

func (p *personalTools) taskID() int64 {
	if len(p.taskIDs) == 0 && len(p.scheduleIDs) > 0 {
		if _, sameSchedule := p.scheduleIDs[p.event.ScheduleID]; len(p.scheduleIDs) != 1 || !sameSchedule {
			return 0
		}
	}
	if p.event.TaskID > 0 {
		if len(p.taskIDs) == 0 {
			return p.event.TaskID
		}
		if _, ok := p.taskIDs[p.event.TaskID]; !ok {
			return 0
		}
	}
	if len(p.taskIDs) == 0 {
		return p.event.TaskID
	}
	if len(p.taskIDs) == 1 {
		for id := range p.taskIDs {
			return id
		}
	}
	return 0
}

func (p *personalTools) addTaskID(id int64) {
	if id > 0 {
		p.taskIDs[id] = struct{}{}
	}
}

func (p *personalTools) directUserMutation() error {
	if p.event.Kind != EventUser || p.event.ID <= 0 {
		return fmt.Errorf("this operation requires a persisted direct user event")
	}
	return nil
}

func (p *personalTools) ownTask(taskID int64) error {
	if p.event.Kind != EventTask || p.event.ID <= 0 || p.event.TaskID <= 0 || taskID != p.event.TaskID {
		return fmt.Errorf("task callbacks may only change their own task")
	}
	return nil
}

func (p *personalTools) requestKey(name string, input any) (string, error) {
	canonical, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode tool input: %w", err)
	}
	hash := sha256.Sum256(canonical)
	return fmt.Sprintf("event:%d:%s:%s", p.event.ID, name, hex.EncodeToString(hash[:])), nil
}

func toolError(err error) personalToolReply {
	return personalToolReply{OK: false, Error: err.Error()}
}

func (p *personalTools) memoryError(key, operation string, err error) personalToolReply {
	if p.log != nil {
		p.log.Warn("personal memory change failed", zap.Int64("event_id", p.event.ID), zap.String("key", key), zap.String("operation", operation), zap.Error(err))
	}
	return toolError(err)
}

func (p *personalTools) taskByID(id int64) *Task {
	for i := range p.input.Tasks {
		if p.input.Tasks[i].ID == id {
			return &p.input.Tasks[i]
		}
	}
	return nil
}

func (p *personalTools) build() ([]tool.BaseTool, error) {
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
		attachments, err := resolveAttachments(ctx, p.store, a.Attachments)
		if err != nil {
			return fail(err)
		}
		key, err := p.requestKey("create_task", actionArgs)
		if err != nil {
			return fail(err)
		}
		task, run, err := p.store.CreateTask(ctx, p.event.ID, key, a.Title, TaskInput{Goal: a.Goal, Context: a.Context, Attachments: attachments, SourceRunIDs: a.SourceRunIDs, MemoryRefs: a.MemoryRefs})
		if err != nil {
			return fail(err)
		}
		if task.ID <= 0 {
			return fail(fmt.Errorf("task creation returned no task"))
		}
		p.addTaskID(task.ID)
		p.succeedTaskAction(attempt)
		return personalToolReply{ActionID: attempt.actionID, OK: true, Queued: run.Status == RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	listTasks, err := utils.InferTool("list_tasks", "按关键词、更新时间及 ID 游标查找任务；后页同时传回 next_before_id、snapshot_max_id、query_key，并保持查询条件。goal/context 全文使用 read_task。", func(ctx context.Context, a TaskQuery) (TaskPage, error) {
		return p.store.QueryTasks(ctx, ReadScope{EventID: p.event.ID}, a)
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
		card := taskCard(*task)
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
		attachments, err := resolveAttachments(ctx, p.store, a.Attachments)
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
		task, run, err := p.store.ReviseTask(ctx, p.event.ID, key, a.TaskID, a.ExpectedRevision, TaskInput{Goal: a.Goal, Context: a.Context, Attachments: attachments, SourceRunIDs: a.SourceRunIDs, MemoryRefs: a.MemoryRefs})
		if err != nil {
			return fail(err)
		}
		if task.ID <= 0 {
			return fail(fmt.Errorf("task update returned no task"))
		}
		p.addTaskID(task.ID)
		p.succeedTaskAction(attempt)
		return personalToolReply{ActionID: attempt.actionID, OK: true, Queued: run.Status == RunQueued, Task: &task, Run: &run}, nil
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
		case EventUser:
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
		case EventTask:
			if err := p.ownTask(a.TaskID); err != nil {
				return taskActionToolError(a.ActionID, err), nil
			}
			if a.ExtendBudget {
				return taskActionToolError(a.ActionID, fmt.Errorf("task callbacks cannot extend the budget")), nil
			}
			if task := p.taskByID(a.TaskID); task != nil && (task.Status == TaskWaitingBudget || task.BudgetMS > 0 && task.UsedMS >= task.BudgetMS) {
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
		return personalToolReply{ActionID: replyActionID, OK: true, Queued: run.Status == RunQueued, Task: &task, Run: &run}, nil
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
		if a.Status != TaskCompleted && a.Status != TaskWaitingInput && a.Status != TaskFailed {
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
		card := runCard(*run)
		return personalToolReply{OK: true, Run: &card}, nil
	})
	if err != nil {
		return nil, err
	}

	listRuns, err := utils.InferTool("list_runs", "按 ID 游标列出执行摘要；后页同时传回 next_before_id、snapshot_max_id、query_key。全文使用 read_run，来源使用 list_materials。", func(ctx context.Context, a struct {
		TaskID int64 `json:"task_id"`
		ListPage
	}) (RunPage, error) {
		p.addTaskID(a.TaskID)
		return p.store.QueryRuns(ctx, ReadScope{EventID: p.event.ID}, a.TaskID, a.ListPage)
	})
	if err != nil {
		return nil, err
	}

	memorySearch, err := utils.InferTool("memory_search", "搜索当前生效的长期事实和偏好，不返回已遗忘的值。", func(ctx context.Context, a memorySearchArgs) (personalToolReply, error) {
		if strings.TrimSpace(a.Query) == "" {
			return toolError(fmt.Errorf("query is required")), nil
		}
		memories, err := p.store.Memories(ctx, a.Query, 20)
		if err != nil {
			return toolError(err), nil
		}
		active := memories[:0]
		for _, memory := range memories {
			if !memory.Deleted {
				active = append(active, memory)
			}
		}
		return personalToolReply{OK: true, Memories: active}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryGet, err := utils.InferTool("memory_get", "按键读取记忆。已遗忘的键返回 tombstone 版本，不含旧值。", func(ctx context.Context, a memoryKeyArgs) (personalToolReply, error) {
		if strings.TrimSpace(a.Key) == "" {
			return toolError(fmt.Errorf("key is required")), nil
		}
		memory, err := p.store.Memory(ctx, a.Key)
		if err != nil {
			return toolError(err), nil
		}
		if memory == nil {
			return toolError(fmt.Errorf("memory not found")), nil
		}
		if memory.Deleted {
			copy := *memory
			copy.Value = ""
			memory = &copy
		}
		return personalToolReply{OK: true, Memory: memory}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryPut, err := utils.InferTool("memory_put", "仅按用户当前消息中的明确记忆请求保存或更正，成功后立即持久化。evidence 必须是当前消息的原文子串。", func(ctx context.Context, a memoryPutArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return p.memoryError(a.Key, "put", err), nil
		}
		p.mustNotify = true
		if strings.TrimSpace(a.Key) == "" || strings.TrimSpace(a.Value) == "" || a.ExpectedVersion < 0 || !strings.Contains(p.event.Text, a.Evidence) || strings.TrimSpace(a.Evidence) == "" {
			return p.memoryError(a.Key, "put", fmt.Errorf("key, value, nonnegative expected_version and an exact current-message evidence quote are required")), nil
		}
		key, err := p.requestKey("memory_put", a)
		if err != nil {
			return p.memoryError(a.Key, "put", err), nil
		}
		memory, err := p.store.WriteMemory(ctx, p.event.ID, key, MemoryChange{Operation: "put", Key: a.Key, Value: a.Value, ExpectedVersion: a.ExpectedVersion, Evidence: a.Evidence})
		if err != nil {
			return p.memoryError(a.Key, "put", err), nil
		}
		return personalToolReply{OK: true, Memory: &memory}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryForget, err := utils.InferTool("memory_forget", "仅按用户当前消息中的明确遗忘请求删除。提供当前版本；evidence 必须是当前消息原文。成功后立即持久化。", func(ctx context.Context, a memoryForgetArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return p.memoryError(a.Key, "forget", err), nil
		}
		p.mustNotify = true
		if strings.TrimSpace(a.Key) == "" || a.ExpectedVersion <= 0 || !strings.Contains(p.event.Text, a.Evidence) || strings.TrimSpace(a.Evidence) == "" {
			return p.memoryError(a.Key, "forget", fmt.Errorf("key, positive expected_version and an exact current-message evidence quote are required")), nil
		}
		key, err := p.requestKey("memory_forget", a)
		if err != nil {
			return p.memoryError(a.Key, "forget", err), nil
		}
		memory, err := p.store.WriteMemory(ctx, p.event.ID, key, MemoryChange{Operation: "forget", Key: a.Key, ExpectedVersion: a.ExpectedVersion, Evidence: a.Evidence})
		if err != nil {
			return p.memoryError(a.Key, "forget", err), nil
		}
		return personalToolReply{OK: true, Memory: &memory}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryHistory, err := utils.InferTool("memory_history", "查看当前键的历史记忆变更；不使用已删除旧值恢复主动记忆。", func(ctx context.Context, a memoryHistoryArgs) (personalToolReply, error) {
		history, err := p.store.MemoryHistory(ctx, strings.TrimSpace(a.Key), 50)
		if err != nil {
			return toolError(err), nil
		}
		return personalToolReply{OK: true, Memories: history}, nil
	})
	if err != nil {
		return nil, err
	}

	scheduled, err := p.buildScheduleTools()
	if err != nil {
		return nil, err
	}
	suggestions, err := p.buildScheduleSuggestionTools()
	if err != nil {
		return nil, err
	}
	scheduled = append(scheduled, suggestions...)
	inputs, err := p.inputTools()
	if err != nil {
		return nil, err
	}
	scheduled = append(scheduled, inputs...)
	taskTools := []tool.BaseTool{createTask, listTasks, getTask, updateTask, cancelTask, continueTask}
	if p.event.Kind == EventTask {
		taskTools = append(taskTools, settleTask)
	}
	taskTools = append(taskTools, getRun, listRuns, memorySearch, memoryGet, memoryPut, memoryForget, memoryHistory)
	return append(taskTools, scheduled...), nil
}
