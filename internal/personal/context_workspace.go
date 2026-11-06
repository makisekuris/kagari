package personal

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"kagari/internal/textpage"
	"kagari/internal/workspace"
	"path/filepath"
	"strconv"
)

func (a *MainAgent) workspaceTools() ([]tool.BaseTool, error) {
	invoke := func(ctx context.Context, taskID int64, name string, args any) (json.RawMessage, error) {
		if taskID <= 0 || a.WorkspaceDirectory == "" {
			return nil, errors.New("personal: task workspace is unavailable")
		}
		task, err := a.Store.Task(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if task == nil {
			return nil, errors.New("personal: task not found")
		}
		ws, err := workspace.OpenExisting(filepath.Join(a.WorkspaceDirectory, "tasks", strconv.FormatInt(taskID, 10)))
		if err != nil {
			return nil, err
		}
		defer ws.Close()
		tools, err := ws.Tools(nil)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		for _, t := range tools {
			info, err := t.Info(ctx)
			if err != nil {
				return nil, err
			}
			if info.Name == name {
				reply, err := t.(tool.InvokableTool).InvokableRun(ctx, string(raw))
				return json.RawMessage(reply), err
			}
		}
		return nil, errors.New("personal: workspace read tool unavailable")
	}
	read, err := utils.InferTool("read_file", "按 task_id 只读查看工作区文本页；后页必须携带版本，不能写入或创建工作区。", func(ctx context.Context, a struct {
		TaskID   int64  `json:"task_id"`
		FilePath string `json:"file_path"`
		textpage.Request
	}) (json.RawMessage, error) {
		return invoke(ctx, a.TaskID, "read_file", a)
	})
	if err != nil {
		return nil, err
	}
	ls, err := utils.InferTool("ls", "按 task_id 只读列出工作区目录页。", func(ctx context.Context, a struct {
		TaskID    int64  `json:"task_id"`
		Path      string `json:"path,omitempty"`
		AfterName string `json:"after_name,omitempty"`
		Version   string `json:"version,omitempty"`
		Limit     int    `json:"limit,omitempty"`
	}) (json.RawMessage, error) {
		return invoke(ctx, a.TaskID, "ls", a)
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{read, ls}, nil
}
