package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

func openTestWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "task")
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, dir
}

func toolByName(t *testing.T, tools []tool.BaseTool, name string) tool.InvokableTool {
	t.Helper()
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Name == name {
			invokable, ok := candidate.(tool.InvokableTool)
			if !ok {
				t.Fatalf("%s is not invokable", name)
			}
			return invokable
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func callTool(t *testing.T, target tool.InvokableTool, args any) toolReply {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := target.InvokableRun(context.Background(), string(encoded))
	if err != nil {
		t.Fatalf("tool call aborted: %v", err)
	}
	var reply toolReply
	if err := json.Unmarshal([]byte(result), &reply); err != nil {
		t.Fatalf("invalid JSON reply %q: %v", result, err)
	}
	return reply
}

func TestToolsCreateReadEditListAndTrackFiles(t *testing.T) {
	w, _ := openTestWorkspace(t)
	tools, err := w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	ls := toolByName(t, tools, "ls")
	read := toolByName(t, tools, "read_file")
	write := toolByName(t, tools, "write_file")
	edit := toolByName(t, tools, "edit_file")

	if got := callTool(t, ls, map[string]any{}); got.Error != "" || got.Content != "" {
		t.Fatalf("empty workspace ls = %+v", got)
	}
	if got := callTool(t, write, map[string]string{"file_path": "notes/draft.txt", "content": "alpha beta"}); got.Error != "" {
		t.Fatalf("write failed: %+v", got)
	}
	if got := callTool(t, read, map[string]string{"file_path": "notes/draft.txt"}); got.Error != "" || got.Content != "alpha beta" {
		t.Fatalf("read = %+v", got)
	}
	if got := callTool(t, edit, map[string]string{"file_path": "notes/draft.txt", "old_string": "beta", "new_string": "gamma"}); got.Error != "" {
		t.Fatalf("edit failed: %+v", got)
	}
	if got := callTool(t, read, map[string]string{"file_path": "notes/draft.txt"}); got.Error != "" || got.Content != "alpha gamma" {
		t.Fatalf("edited read = %+v", got)
	}
	if got := callTool(t, ls, map[string]string{"path": "notes"}); got.Error != "" || got.Content != "draft.txt" {
		t.Fatalf("directory listing = %+v", got)
	}
	if got, want := w.Files(), []string{"notes/draft.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Files() = %v, want %v", got, want)
	}
}

func TestEditRequiresOneExactMatchAndLeavesFileUnchanged(t *testing.T) {
	w, _ := openTestWorkspace(t)
	tools, err := w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	write := toolByName(t, tools, "write_file")
	edit := toolByName(t, tools, "edit_file")
	read := toolByName(t, tools, "read_file")
	callTool(t, write, map[string]string{"file_path": "text.txt", "content": "one one"})
	for _, old := range []string{"one", "missing"} {
		if got := callTool(t, edit, map[string]string{"file_path": "text.txt", "old_string": old, "new_string": "x"}); got.Error == "" {
			t.Fatalf("edit %q unexpectedly succeeded", old)
		}
	}
	if got := callTool(t, edit, map[string]string{"file_path": "absent.txt", "old_string": "one", "new_string": "x"}); got.Error == "" {
		t.Fatal("edit of missing file unexpectedly succeeded")
	}
	if got := callTool(t, read, map[string]string{"file_path": "text.txt"}); got.Error != "" || got.Content != "one one" {
		t.Fatalf("failed edits changed file: %+v", got)
	}
	if got := callTool(t, write, map[string]string{"file_path": "text.txt", "content": "aaa"}); got.Error != "" {
		t.Fatal(got.Error)
	}
	if got := callTool(t, edit, map[string]string{"file_path": "text.txt", "old_string": "aa", "new_string": "x"}); got.Error == "" {
		t.Fatal("overlapping duplicate old_string unexpectedly succeeded")
	}
	if got := callTool(t, read, map[string]string{"file_path": "text.txt"}); got.Error != "" || got.Content != "aaa" {
		t.Fatalf("ambiguous edit changed file: %+v", got)
	}
}

func TestPathsAndOutboundSymlinksAreRejected(t *testing.T) {
	w, dir := openTestWorkspace(t)
	tools, err := w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	read := toolByName(t, tools, "read_file")
	write := toolByName(t, tools, "write_file")
	ls := toolByName(t, tools, "ls")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "outside-link.txt")); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Dir(outside)
	if err := os.Symlink(outsideDir, filepath.Join(dir, "outside-dir")); err != nil {
		t.Fatal(err)
	}

	for _, file := range []string{"../outside.txt", outside, "outside-link.txt", "outside-dir/outside.txt"} {
		if got := callTool(t, read, map[string]string{"file_path": file}); got.Error == "" {
			t.Errorf("read %q unexpectedly succeeded", file)
		}
	}
	for _, file := range []string{"../outside.txt", "outside-link.txt", "outside-dir/new.txt"} {
		if got := callTool(t, write, map[string]string{"file_path": file, "content": "changed"}); got.Error == "" {
			t.Errorf("write %q unexpectedly succeeded", file)
		}
	}
	if got := callTool(t, ls, map[string]string{"path": "outside-dir"}); got.Error == "" {
		t.Fatal("ls of outbound symlink unexpectedly succeeded")
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "untouched" {
		t.Fatalf("outside file changed to %q", data)
	}
}

func TestCanceledAndGuardRejectedCallsDoNotWrite(t *testing.T) {
	w, dir := openTestWorkspace(t)
	var beforeCalls int
	tools, err := w.Tools(func(context.Context) error {
		beforeCalls++
		return errors.New("stale run")
	})
	if err != nil {
		t.Fatal(err)
	}
	write := toolByName(t, tools, "write_file")
	if got := callTool(t, write, map[string]string{"file_path": "stale.txt", "content": "no"}); got.Error != "stale run" {
		t.Fatalf("guard failure reply = %+v", got)
	}
	if beforeCalls != 1 {
		t.Fatalf("before called %d times", beforeCalls)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guard failure wrote a file: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	tools, err = w.Tools(func(context.Context) error { cancel(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	encoded := `{"file_path":"canceled.txt","content":"no"}`
	result, err := toolByName(t, tools, "write_file").InvokableRun(canceledCtx, encoded)
	if err != nil {
		t.Fatalf("canceled tool aborted model loop: %v", err)
	}
	var reply toolReply
	if err := json.Unmarshal([]byte(result), &reply); err != nil || reply.Error == "" {
		t.Fatalf("canceled reply = %q, error %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "canceled.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled call wrote a file: %v", err)
	}
}

func TestReopenPreservesFilesButResetsSessionFileList(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "task")
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	callTool(t, toolByName(t, tools, "write_file"), map[string]string{"file_path": "saved.txt", "content": "persisted"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	tools, err = w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := callTool(t, toolByName(t, tools, "read_file"), map[string]string{"file_path": "saved.txt"}); got.Error != "" || got.Content != "persisted" {
		t.Fatalf("reopened file = %+v", got)
	}
	if got := w.Files(); len(got) != 0 {
		t.Fatalf("Files() includes prior-session files: %v", got)
	}
}

func TestFailedWritePreservesExistingFile(t *testing.T) {
	w, dir := openTestWorkspace(t)
	tools, err := w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	write := toolByName(t, tools, "write_file")
	if got := callTool(t, write, map[string]string{"file_path": "locked/value.txt", "content": "original"}); got.Error != "" {
		t.Fatal(got.Error)
	}
	if err := os.Chmod(filepath.Join(dir, "locked"), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "locked"), 0700) })
	if got := callTool(t, write, map[string]string{"file_path": "locked/value.txt", "content": "replacement"}); got.Error == "" {
		t.Fatal("write in read-only directory unexpectedly succeeded")
	}
	if err := os.Chmod(filepath.Join(dir, "locked"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "locked", "value.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Fatalf("failed write replaced original content: %q", data)
	}
}

func TestToolErrorsAreJSONReplies(t *testing.T) {
	w, _ := openTestWorkspace(t)
	tools, err := w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	reply := callTool(t, toolByName(t, tools, "read_file"), map[string]string{"file_path": "missing.txt"})
	if !strings.Contains(reply.Error, "no such file") {
		t.Fatalf("missing-file reply = %+v", reply)
	}
}

func TestTextLimitsPreserveExistingContent(t *testing.T) {
	w, _ := openTestWorkspace(t)
	ctx := context.Background()
	if err := w.writeFile(ctx, "report.md", []byte("original")); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{[]byte(strings.Repeat("x", maxFileSize+1)), {0xff}} {
		if err := w.writeFile(ctx, "report.md", data); err == nil {
			t.Fatal("invalid or oversized text was written")
		}
		if content, err := w.readFile("report.md"); err != nil || content != "original" {
			t.Fatalf("rejected write changed file: %q, %v", content, err)
		}
	}
}
