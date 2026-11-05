package personal

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/components/tool"

	"kagari/internal/agent"
	"kagari/internal/workspace"
)

// TaskExecutor runs a stage with persistent files; Runtime serializes each task's stages.
func TaskExecutor(engine *agent.Engine, store Repository, directory string) ExecuteFunc {
	return func(ctx context.Context, run Run, progress func(string) error) (result StageResult, err error) {
		if err := checkTaskRun(ctx, store, run); err != nil {
			return StageResult{Summary: "Task stage could not start: its run is no longer active."}, err
		}
		if engine == nil {
			return StageResult{Summary: "Task stage could not start: reading engine is unavailable."}, errors.New("personal: reading engine is unavailable")
		}
		if strings.TrimSpace(directory) == "" {
			return StageResult{Summary: "Task stage could not start: task workspace is unavailable."}, errors.New("personal: task workspace directory is required")
		}
		taskPath := filepath.Join(directory, "tasks", strconv.FormatInt(run.TaskID, 10))
		ws, openErr := workspace.Open(taskPath)
		if openErr != nil {
			return StageResult{Summary: "Task stage could not open its workspace."}, fmt.Errorf("personal: open task workspace: %w", openErr)
		}
		defer func() {
			closeErr := ws.Close()
			result.Files = append(result.Files, ws.Files()...)
			if closeErr != nil {
				if err == nil {
					result.Summary = "Task stage could not close its workspace."
				}
				err = errors.Join(err, fmt.Errorf("personal: close task workspace: %w", closeErr))
			}
		}()

		workspaceTools, err := ws.Tools(func(toolCtx context.Context) error {
			return checkTaskRun(toolCtx, store, run)
		})
		if err != nil {
			return StageResult{Summary: "Task stage could not prepare its workspace tools."}, fmt.Errorf("personal: prepare task workspace tools: %w", err)
		}
		stageEngine := *engine
		stageEngine.AdditionalTools = append(append([]tool.BaseTool(nil), engine.AdditionalTools...), workspaceTools...)
		stageEngine.AdditionalInstruction = strings.TrimSpace(engine.AdditionalInstruction + "\n\n" + workspace.Instruction)
		result, err = readingExecutor(&stageEngine, store)(ctx, run, progress)
		return result, err
	}
}

func checkTaskRun(ctx context.Context, store Repository, run Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil || run.Kind != RunTask || run.ID <= 0 || run.TaskID <= 0 || run.Revision <= 0 {
		return errors.New("personal: an active task run and store are required")
	}
	current, err := store.Run(ctx, run.ID)
	if err != nil {
		return err
	}
	if current == nil || current.Kind != RunTask || current.Status != RunRunning || current.TaskID != run.TaskID || current.Revision != run.Revision {
		return fmt.Errorf("personal: task run %d is no longer active", run.ID)
	}
	task, err := store.Task(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if task == nil || task.Status != TaskRunning || task.Revision != run.Revision {
		return fmt.Errorf("personal: task %d revision %d is no longer active", run.TaskID, run.Revision)
	}
	return nil
}
