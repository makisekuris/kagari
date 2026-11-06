package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"kagari/internal/textpage"
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
	result := callToolJSON(t, target, args)
	var reply toolReply
	if err := json.Unmarshal([]byte(result), &reply); err != nil {
		t.Fatalf("invalid JSON reply %q: %v", result, err)
	}
	return reply
}

func callToolJSON(t *testing.T, target tool.InvokableTool, args any) string {
	return callToolJSONContext(t, context.Background(), target, args)
}

func callToolJSONContext(t *testing.T, ctx context.Context, target tool.InvokableTool, args any) string {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := target.InvokableRun(ctx, string(encoded))
	if err != nil {
		t.Fatalf("tool call aborted: %v", err)
	}
	return result
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

	if got := callTool(t, ls, map[string]any{}); got.Error != "" || got.Directory == nil || len(got.Directory.Entries) != 0 {
		t.Fatalf("empty workspace ls = %+v", got)
	}
	if got := callTool(t, write, map[string]string{"file_path": "notes/draft.txt", "content": "alpha beta"}); got.Error != "" {
		t.Fatalf("write failed: %+v", got)
	}
	if got := callTool(t, read, map[string]string{"file_path": "notes/draft.txt"}); got.Error != "" || got.Page == nil || got.Page.Text != "alpha beta" {
		t.Fatalf("read = %+v", got)
	}
	if got := callTool(t, edit, map[string]string{"file_path": "notes/draft.txt", "old_string": "beta", "new_string": "gamma"}); got.Error != "" {
		t.Fatalf("edit failed: %+v", got)
	}
	if got := callTool(t, read, map[string]string{"file_path": "notes/draft.txt"}); got.Error != "" || got.Page == nil || got.Page.Text != "alpha gamma" {
		t.Fatalf("edited read = %+v", got)
	}
	if got := callTool(t, ls, map[string]string{"path": "notes"}); got.Error != "" || got.Directory == nil || !reflect.DeepEqual(got.Directory.Entries, []string{"draft.txt"}) {
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
	if got := callTool(t, read, map[string]string{"file_path": "text.txt"}); got.Error != "" || got.Page == nil || got.Page.Text != "one one" {
		t.Fatalf("failed edits changed file: %+v", got)
	}
	if got := callTool(t, write, map[string]string{"file_path": "text.txt", "content": "aaa"}); got.Error != "" {
		t.Fatal(got.Error)
	}
	if got := callTool(t, edit, map[string]string{"file_path": "text.txt", "old_string": "aa", "new_string": "x"}); got.Error == "" {
		t.Fatal("overlapping duplicate old_string unexpectedly succeeded")
	}
	if got := callTool(t, read, map[string]string{"file_path": "text.txt"}); got.Error != "" || got.Page == nil || got.Page.Text != "aaa" {
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
	if got := callTool(t, toolByName(t, tools, "read_file"), map[string]string{"file_path": "saved.txt"}); got.Error != "" || got.Page == nil || got.Page.Text != "persisted" {
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

func TestReadFilePagesUseRunesAndDetectChanges(t *testing.T) {
	w, _ := openTestWorkspace(t)
	tools, err := w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	write := toolByName(t, tools, "write_file")
	read := toolByName(t, tools, "read_file")
	if got := callTool(t, write, map[string]string{"file_path": "unicode.txt", "content": "猫🧪abc"}); got.Error != "" {
		t.Fatal(got.Error)
	}
	first := callTool(t, read, map[string]any{"file_path": "unicode.txt", "limit": 2})
	if first.Error != "" || first.Page == nil || first.Page.Text != "猫🧪" || first.Page.End != 2 || first.Page.TotalRunes != 5 || !first.Page.HasMore || first.Page.NextOffset == nil || *first.Page.NextOffset != 2 {
		t.Fatalf("first page = %+v", first)
	}
	second := callTool(t, read, map[string]any{"file_path": "unicode.txt", "offset": 2, "limit": 2, "version": first.Page.Version})
	if second.Error != "" || second.Page == nil || second.Page.Text != "ab" || second.Page.Offset != 2 || second.Page.End != 4 || !second.Page.HasMore {
		t.Fatalf("second page = %+v", second)
	}
	last := callTool(t, read, map[string]any{"file_path": "unicode.txt", "offset": 5, "version": first.Page.Version})
	if last.Error != "" || last.Page == nil || last.Page.Text != "" || last.Page.End != 5 || last.Page.HasMore {
		t.Fatalf("empty end page = %+v", last)
	}
	missingVersion := callTool(t, read, map[string]any{"file_path": "unicode.txt", "offset": 1})
	if missingVersion.Error != "version_required" {
		t.Fatalf("later page without version = %+v", missingVersion)
	}
	if got := callTool(t, write, map[string]string{"file_path": "unicode.txt", "content": "changed"}); got.Error != "" {
		t.Fatal(got.Error)
	}
	changed := callTool(t, read, map[string]any{"file_path": "unicode.txt", "offset": 2, "version": first.Page.Version})
	if changed.Error != "version_changed" || changed.Page == nil || changed.Page.Version == first.Page.Version {
		t.Fatalf("changed file response = %+v", changed)
	}
}

func TestDirectoryPagesDetectChangesAndKeepEscapedNamesWhole(t *testing.T) {
	w, dir := openTestWorkspace(t)
	for i := range 50 {
		name := strings.Repeat("<", 240) + "-" + fmt.Sprintf("%02d", i)
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	ls := toolByName(t, tools, "ls")
	raw := callToolJSON(t, ls, map[string]any{"limit": 50})
	if len(raw) > textpage.MaxJSONBytes {
		t.Fatalf("directory response is %d bytes", len(raw))
	}
	var capped toolReply
	if err := json.Unmarshal([]byte(raw), &capped); err != nil || capped.Directory == nil || len(capped.Directory.Entries) >= 50 || !capped.Directory.HasMore || capped.Directory.NextAfterName == "" {
		t.Fatalf("oversized directory page was not shortened by whole entries: %+v, %v", capped, err)
	}
	first := callTool(t, ls, map[string]any{"limit": 2})
	if first.Error != "" || first.Directory == nil || len(first.Directory.Entries) != 2 || !first.Directory.HasMore || first.Directory.NextAfterName != first.Directory.Entries[1] {
		t.Fatalf("first directory page = %+v", first)
	}
	if err := os.WriteFile(filepath.Join(dir, "new-file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	missingVersion := callTool(t, ls, map[string]any{"after_name": first.Directory.NextAfterName})
	if missingVersion.Error != "version_required" {
		t.Fatalf("directory continuation without version = %+v", missingVersion)
	}
	changed := callTool(t, ls, map[string]any{"after_name": first.Directory.NextAfterName, "version": first.Directory.Version})
	if changed.Error != "version_changed" || changed.Directory == nil || changed.Directory.Version == first.Directory.Version {
		t.Fatalf("changed directory response = %+v", changed)
	}
}

func TestReadFileResponseCapShrinksNULsAtRuneBoundaries(t *testing.T) {
	w, _ := openTestWorkspace(t)
	tools, err := w.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	write := toolByName(t, tools, "write_file")
	read := toolByName(t, tools, "read_file")
	if got := callTool(t, write, map[string]string{"file_path": "nul.txt", "content": strings.Repeat("\x00", 8000)}); got.Error != "" {
		t.Fatal(got.Error)
	}
	ctx := textpage.WithMaxBytes(context.Background(), 1024)
	raw := callToolJSONContext(t, ctx, read, map[string]any{"file_path": "nul.txt", "limit": 8000})
	if len(raw) > textpage.MaxBytes(ctx) {
		t.Fatalf("text response is %d bytes", len(raw))
	}
	var reply toolReply
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != "" || reply.Page == nil || utf8.RuneCountInString(reply.Page.Text) >= reply.Page.TotalRunes || !reply.Page.HasMore || reply.Page.End != utf8.RuneCountInString(reply.Page.Text) {
		t.Fatalf("bounded NUL page = %+v", reply)
	}
}

func TestOpenExistingDoesNotCreateAndOnlyExposesReadTools(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing")
	if _, err := OpenExisting(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting missing = %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting created missing path: %v", err)
	}
	w, dir := openTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "saved.txt"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	readonly, err := OpenExisting(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	tools, err := readonly.Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("read-only tools = %d", len(tools))
	}
	if got := callTool(t, toolByName(t, tools, "read_file"), map[string]string{"file_path": "saved.txt"}); got.Error != "" || got.Page == nil || got.Page.Text != "saved" {
		t.Fatalf("read-only file = %+v", got)
	}
	for _, name := range []string{"write_file", "edit_file"} {
		for _, candidate := range tools {
			info, err := candidate.Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if info.Name == name {
				t.Fatalf("read-only workspace exposes %s", name)
			}
		}
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
