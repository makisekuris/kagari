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
	Title   string `json:"title"`
	Goal    string `json:"goal"`
	Context string `json:"context"`
}

type taskArgs struct {
	TaskID int64 `json:"task_id"`
}

type reviseTaskArgs struct {
	TaskID           int64  `json:"task_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Goal             string `json:"goal"`
	Context          string `json:"context"`
}

type cancelTaskArgs struct {
	TaskID           int64 `json:"task_id"`
	ExpectedRevision int64 `json:"expected_revision"`
}

type continueTaskArgs struct {
	TaskID           int64  `json:"task_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Step             string `json:"step"`
	ExtendBudget     bool   `json:"extend_budget"`
}

type settleTaskArgs struct {
	TaskID           int64      `json:"task_id"`
	ExpectedRevision int64      `json:"expected_revision"`
	Status           TaskStatus `json:"status"`
	Summary          string     `json:"summary"`
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
	store      Repository
	log        *zap.Logger
	input      TurnInput
	event      Event
	taskIDs    map[int64]struct{}
	mustNotify bool
}

func (p *personalTools) taskID() int64 {
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
	createTask, err := utils.InferTool("create_task", "为明确的新目标创建后台任务并排队；只允许直接用户事件。", func(ctx context.Context, a createTaskArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		if strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Goal) == "" {
			return toolError(fmt.Errorf("title and goal are required")), nil
		}
		key, err := p.requestKey("create_task", a)
		if err != nil {
			return toolError(err), nil
		}
		task, run, err := p.store.CreateTask(ctx, p.event.ID, key, a.Title, TaskInput{Goal: a.Goal, Context: a.Context})
		if err != nil {
			return toolError(err), nil
		}
		if task.ID <= 0 {
			return toolError(fmt.Errorf("task creation returned no task")), nil
		}
		p.addTaskID(task.ID)
		return personalToolReply{OK: true, Queued: run.Status == RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	listTasks, err := utils.InferTool("list_tasks", "列出最近的后台任务。", func(ctx context.Context, _ struct{}) (personalToolReply, error) {
		tasks, err := p.store.Tasks(ctx, 50)
		if err != nil {
			return toolError(err), nil
		}
		return personalToolReply{OK: true, Tasks: tasks}, nil
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
		return personalToolReply{OK: true, Task: task}, nil
	})
	if err != nil {
		return nil, err
	}

	updateTask, err := utils.InferTool("update_task", "由直接用户事件更新任务目标，并排队新版本。使用当前 revision。", func(ctx context.Context, a reviseTaskArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		if a.TaskID <= 0 || a.ExpectedRevision <= 0 || strings.TrimSpace(a.Goal) == "" {
			return toolError(fmt.Errorf("task_id, expected_revision and goal are required")), nil
		}
		key, err := p.requestKey("update_task", a)
		if err != nil {
			return toolError(err), nil
		}
		task, run, err := p.store.ReviseTask(ctx, p.event.ID, key, a.TaskID, a.ExpectedRevision, TaskInput{Goal: a.Goal, Context: a.Context})
		if err != nil {
			return toolError(err), nil
		}
		if task.ID <= 0 {
			return toolError(fmt.Errorf("task update returned no task")), nil
		}
		p.addTaskID(task.ID)
		return personalToolReply{OK: true, Queued: run.Status == RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	cancelTask, err := utils.InferTool("cancel_task", "由直接用户事件取消指定版本的任务。", func(ctx context.Context, a cancelTaskArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
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

	continueTask, err := utils.InferTool("continue_task", "在原目标内排队继续任务。任务回调只能继续自己的任务且不能扩展预算；直接用户明确授权后才可将 extend_budget 设为 true。", func(ctx context.Context, a continueTaskArgs) (personalToolReply, error) {
		if a.TaskID <= 0 || a.ExpectedRevision <= 0 || strings.TrimSpace(a.Step) == "" {
			return toolError(fmt.Errorf("task_id, expected_revision and step are required")), nil
		}
		switch p.event.Kind {
		case EventUser:
			if err := p.directUserMutation(); err != nil {
				return toolError(err), nil
			}
		case EventTask:
			if err := p.ownTask(a.TaskID); err != nil {
				return toolError(err), nil
			}
			if a.ExtendBudget {
				return toolError(fmt.Errorf("task callbacks cannot extend the budget")), nil
			}
			if task := p.taskByID(a.TaskID); task != nil && (task.Status == TaskWaitingBudget || task.BudgetMS > 0 && task.UsedMS >= task.BudgetMS) {
				return toolError(fmt.Errorf("task budget is exhausted; ask the user to continue")), nil
			}
		default:
			return toolError(fmt.Errorf("continuing tasks requires a direct user event or task callback")), nil
		}
		key, err := p.requestKey("continue_task", a)
		if err != nil {
			return toolError(err), nil
		}
		task, run, err := p.store.ContinueTask(ctx, p.event.ID, key, a.TaskID, a.ExpectedRevision, a.Step, a.ExtendBudget)
		if err != nil {
			return toolError(err), nil
		}
		if task.ID <= 0 {
			return toolError(fmt.Errorf("task continuation returned no task")), nil
		}
		p.addTaskID(task.ID)
		return personalToolReply{OK: true, Queued: run.Status == RunQueued, Task: &task, Run: &run}, nil
	})
	if err != nil {
		return nil, err
	}

	settleTask, err := utils.InferTool("settle_task", "结束当前任务回调的任务。status 只能为 completed、waiting_input 或 failed。", func(ctx context.Context, a settleTaskArgs) (personalToolReply, error) {
		if err := p.ownTask(a.TaskID); err != nil {
			return toolError(err), nil
		}
		if a.ExpectedRevision <= 0 || strings.TrimSpace(a.Summary) == "" {
			return toolError(fmt.Errorf("expected_revision and summary are required")), nil
		}
		if a.Status != TaskCompleted && a.Status != TaskWaitingInput && a.Status != TaskFailed {
			return toolError(fmt.Errorf("status must be completed, waiting_input or failed")), nil
		}
		task, err := p.store.SettleTask(ctx, p.event.ID, a.TaskID, a.ExpectedRevision, a.Status, a.Summary)
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
		return personalToolReply{OK: true, Run: run}, nil
	})
	if err != nil {
		return nil, err
	}

	listRuns, err := utils.InferTool("list_runs", "按任务 ID 列出最近的执行记录。", func(ctx context.Context, a listRunsArgs) (personalToolReply, error) {
		if a.TaskID <= 0 {
			return toolError(fmt.Errorf("task_id must be positive")), nil
		}
		runs, err := p.store.Runs(ctx, a.TaskID, 30)
		if err != nil {
			return toolError(err), nil
		}
		if len(runs) == 0 {
			if task, taskErr := p.store.Task(ctx, a.TaskID); taskErr != nil {
				return toolError(taskErr), nil
			} else if task == nil {
				return toolError(fmt.Errorf("task not found")), nil
			}
		}
		p.addTaskID(a.TaskID)
		return personalToolReply{OK: true, Runs: runs}, nil
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

	return []tool.BaseTool{createTask, listTasks, getTask, updateTask, cancelTask, continueTask, settleTask, getRun, listRuns, memorySearch, memoryGet, memoryPut, memoryForget, memoryHistory}, nil
}
