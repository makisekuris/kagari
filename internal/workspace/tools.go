package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"kagari/internal/textpage"
)

type lsArgs struct {
	Path      string `json:"path,omitempty"`
	AfterName string `json:"after_name,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Version   string `json:"version,omitempty"`
}

type readFileArgs struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Version  string `json:"version,omitempty"`
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

// Tools returns the file tools available to this workspace.
func (w *Workspace) Tools(before func(context.Context) error) ([]tool.BaseTool, error) {
	ls, err := utils.InferTool("ls", "按名称分页列出工作区目录；path 默认为 '.'，目录名以 / 结尾。后续页传入返回的 next_after_name 和 version。", func(ctx context.Context, a lsArgs) (toolReply, error) {
		return w.call(ctx, before, func() (toolReply, error) {
			path := a.Path
			if path == "" {
				path = "."
			}
			rel, err := cleanPath(path, true)
			if err != nil {
				return workspaceError(ctx, err), nil
			}
			limit, err := textpage.ValidateList(a.AfterName, a.Limit)
			if err != nil {
				return workspaceError(ctx, err), nil
			}
			if a.AfterName != "" && a.Version == "" {
				return workspaceError(ctx, textpage.ErrVersionRequired), nil
			}
			if err := ctx.Err(); err != nil {
				return workspaceError(ctx, err), nil
			}
			names, version, err := w.directoryNames(rel)
			if err != nil {
				return workspaceError(ctx, err), nil
			}
			if a.Version != "" && a.Version != version {
				return toolReply{Error: "version_changed", Directory: &directoryPage{Version: version}}, nil
			}
			start := sort.Search(len(names), func(i int) bool { return names[i] > a.AfterName })
			count := min(limit, len(names)-start)
			makePage := func() *directoryPage {
				end := start + count
				page := &directoryPage{Entries: append([]string(nil), names[start:end]...), HasMore: end < len(names), Version: version}
				if page.HasMore && len(page.Entries) > 0 {
					page.NextAfterName = page.Entries[len(page.Entries)-1]
				}
				return page
			}
			reply := &toolReply{Directory: makePage()}
			return boundedToolReply(ctx, reply, func() bool {
				if count <= 1 {
					return false
				}
				count--
				reply.Directory = makePage()
				return true
			}), nil
		}), nil
	})
	if err != nil {
		return nil, err
	}

	read, err := utils.InferTool("read_file", "分页读取工作区中的 UTF-8 文本文件（最大 1 MiB）；偏移按 Unicode 码点计算。后续页必须传入上一页的 version。", func(ctx context.Context, a readFileArgs) (toolReply, error) {
		rel := ""
		reply := w.call(ctx, before, func() (toolReply, error) {
			cleaned, err := cleanPath(a.FilePath, false)
			if err != nil {
				return workspaceError(ctx, err), nil
			}
			rel = cleaned
			page, err := w.readFilePage(ctx, rel, textpage.Request{Offset: a.Offset, Limit: a.Limit, Version: a.Version})
			if err != nil {
				var changed *textpage.VersionChangedError
				if errors.As(err, &changed) {
					return toolReply{Error: err.Error(), Page: &textpage.Page{Version: changed.Version}}, nil
				}
				return workspaceError(ctx, err), nil
			}
			reply := &toolReply{Page: &page}
			return boundedToolReply(ctx, reply, page.Shrink), nil
		})
		if reply.Error == "" && reply.Page != nil && w.ObserveReads != nil {
			w.ObserveReads(rel, *reply.Page)
		}
		return reply, nil
	})
	if err != nil {
		return nil, err
	}
	if w.readOnly {
		return []tool.BaseTool{ls, read}, nil
	}

	write, err := utils.InferTool("write_file", "创建或完整覆盖工作区中的 UTF-8 文本文件（最大 1 MiB）；覆盖前先读取文件。", func(ctx context.Context, a writeFileArgs) (toolReply, error) {
		return w.call(ctx, before, func() (toolReply, error) {
			rel, err := cleanPath(a.FilePath, false)
			if err != nil {
				return workspaceError(ctx, err), nil
			}
			if err := w.writeFile(ctx, rel, []byte(a.Content)); err != nil {
				return workspaceError(ctx, err), nil
			}
			return toolReply{Content: "Wrote " + rel, Files: []string{rel}}, nil
		}), nil
	})
	if err != nil {
		return nil, err
	}

	edit, err := utils.InferTool("edit_file", "将工作区文件中的 old_string 精确唯一替换为 new_string；旧文本必须恰好出现一次。", func(ctx context.Context, a editFileArgs) (toolReply, error) {
		return w.call(ctx, before, func() (toolReply, error) {
			rel, err := cleanPath(a.FilePath, false)
			if err != nil {
				return workspaceError(ctx, err), nil
			}
			if a.OldString == "" {
				return workspaceError(ctx, errors.New("old_string must not be empty")), nil
			}
			content, err := w.readFile(rel)
			if err != nil {
				return workspaceError(ctx, err), nil
			}
			index := strings.Index(content, a.OldString)
			if index < 0 {
				return workspaceError(ctx, errors.New("old_string was not found")), nil
			}
			if strings.Index(content[index+1:], a.OldString) >= 0 {
				return workspaceError(ctx, errors.New("old_string is ambiguous; it must appear exactly once")), nil
			}
			updated := content[:index] + a.NewString + content[index+len(a.OldString):]
			if updated == content {
				return toolReply{Content: "No changes made"}, nil
			}
			if err := w.writeFile(ctx, rel, []byte(updated)); err != nil {
				return workspaceError(ctx, err), nil
			}
			return toolReply{Content: "Edited " + rel, Files: []string{rel}}, nil
		}), nil
	})
	if err != nil {
		return nil, err
	}

	return []tool.BaseTool{ls, read, write, edit}, nil
}

func (w *Workspace) directoryNames(name string) ([]string, string, error) {
	root, err := w.root.OpenRoot(filepath.FromSlash(name))
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, "", err
	}
	defer dir.Close()
	entries, err := dir.Readdir(-1)
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !utf8.ValidString(name) {
			return nil, "", errors.New("directory contains a non-UTF-8 name")
		}
		if entry.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	encoded, err := json.Marshal(names)
	if err != nil {
		return nil, "", err
	}
	return names, textpage.Version(string(encoded)), nil
}
