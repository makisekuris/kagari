package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

type lsArgs struct {
	Path string `json:"path,omitempty"`
}

type readFileArgs struct {
	FilePath string `json:"file_path"`
}

type writeFileArgs struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

type editFileArgs struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// Tools returns the four task-local text file tools.
func (w *Workspace) Tools(before func(context.Context) error) ([]tool.BaseTool, error) {
	ls, err := utils.InferTool("ls", "列出工作区目录内容；path 默认为 '.'。目录和文件均显示，目录名以 / 结尾。", func(ctx context.Context, a lsArgs) (toolReply, error) {
		return w.call(ctx, before, func() (string, []string, error) {
			path := a.Path
			if path == "" {
				path = "."
			}
			rel, err := cleanPath(path, true)
			if err != nil {
				return "", nil, err
			}
			root, err := w.root.OpenRoot(filepath.FromSlash(rel))
			if err != nil {
				return "", nil, err
			}
			defer root.Close()
			dir, err := root.Open(".")
			if err != nil {
				return "", nil, err
			}
			defer dir.Close()
			entries, err := dir.Readdir(-1)
			if err != nil {
				return "", nil, err
			}
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() {
					name += "/"
				}
				names = append(names, name)
			}
			sort.Strings(names)
			if err := ctx.Err(); err != nil {
				return "", nil, err
			}
			return strings.Join(names, "\n"), nil, nil
		}), nil
	})
	if err != nil {
		return nil, err
	}

	read, err := utils.InferTool("read_file", "读取工作区中的 UTF-8 文本文件（最大 1 MiB）。", func(ctx context.Context, a readFileArgs) (toolReply, error) {
		return w.call(ctx, before, func() (string, []string, error) {
			rel, err := cleanPath(a.FilePath, false)
			if err != nil {
				return "", nil, err
			}
			content, err := w.readFile(rel)
			if err != nil {
				return "", nil, err
			}
			if err := ctx.Err(); err != nil {
				return "", nil, err
			}
			return content, nil, nil
		}), nil
	})
	if err != nil {
		return nil, err
	}

	write, err := utils.InferTool("write_file", "创建或完整覆盖工作区中的 UTF-8 文本文件（最大 1 MiB）；覆盖前先读取文件。", func(ctx context.Context, a writeFileArgs) (toolReply, error) {
		return w.call(ctx, before, func() (string, []string, error) {
			rel, err := cleanPath(a.FilePath, false)
			if err != nil {
				return "", nil, err
			}
			if err := w.writeFile(ctx, rel, []byte(a.Content)); err != nil {
				return "", nil, err
			}
			return "Wrote " + rel, []string{rel}, nil
		}), nil
	})
	if err != nil {
		return nil, err
	}

	edit, err := utils.InferTool("edit_file", "将工作区文件中的 old_string 精确唯一替换为 new_string；旧文本必须恰好出现一次。", func(ctx context.Context, a editFileArgs) (toolReply, error) {
		return w.call(ctx, before, func() (string, []string, error) {
			rel, err := cleanPath(a.FilePath, false)
			if err != nil {
				return "", nil, err
			}
			if a.OldString == "" {
				return "", nil, errors.New("old_string must not be empty")
			}
			content, err := w.readFile(rel)
			if err != nil {
				return "", nil, err
			}
			index := strings.Index(content, a.OldString)
			if index < 0 {
				return "", nil, errors.New("old_string was not found")
			}
			if strings.Index(content[index+1:], a.OldString) >= 0 {
				return "", nil, errors.New("old_string is ambiguous; it must appear exactly once")
			}
			updated := content[:index] + a.NewString + content[index+len(a.OldString):]
			if updated == content {
				return "No changes made", nil, nil
			}
			if err := w.writeFile(ctx, rel, []byte(updated)); err != nil {
				return "", nil, err
			}
			return "Edited " + rel, []string{rel}, nil
		}), nil
	})
	if err != nil {
		return nil, err
	}

	return []tool.BaseTool{ls, read, write, edit}, nil
}
