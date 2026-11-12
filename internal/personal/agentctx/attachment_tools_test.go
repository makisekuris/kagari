package agentctx

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"kagari/internal/personal/internal/testkit"
	"kagari/internal/textpage"
)

func TestAttachmentToolsRejectUnselectedAndBinaryFiles(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Minute)
	file, err := store.SaveAttachment(ctx, "binary.bin", "application/octet-stream", strings.NewReader("a\x00b"))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := AttachmentTools(store, map[string]bool{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	unselected, _ := invokeAttachmentToolContext(t, ctx, tools, map[string]any{"id": file.ID})
	if !strings.Contains(unselected.Error, "not part of this task") {
		t.Fatalf("unselected attachment was readable: %+v", unselected)
	}

	tools, err = AttachmentTools(store, map[string]bool{file.ID: true}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	binary, _ := invokeAttachmentToolContext(t, ctx, tools, map[string]any{"id": file.ID})
	if !strings.Contains(binary.Error, "not supported UTF-8 text") {
		t.Fatalf("binary attachment was accepted as text: %+v", binary)
	}
}

func TestReadAttachmentPagesStayBoundedAndRecheckDeletion(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Minute)
	content := strings.Repeat("<", 8000)
	file, err := store.SaveAttachment(ctx, "large.txt", "text/plain", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	observed := []textpage.Page{}
	tools, err := AttachmentTools(store, map[string]bool{file.ID: true}, nil, nil, func(id string, page textpage.Page) {
		if id != file.ID {
			t.Fatalf("observed attachment id = %s", id)
		}
		observed = append(observed, page)
	})
	if err != nil {
		t.Fatal(err)
	}
	budget := textpage.WithMaxBytes(ctx, 1024)
	first, raw := invokeAttachmentToolContext(t, budget, tools, map[string]any{"id": file.ID, "limit": 8000})
	if len(raw) > textpage.MaxBytes(budget) || first.Error != "" || first.Page == nil || first.Page.TotalRunes != 8000 || first.Page.End != len([]rune(first.Page.Text)) || !first.Page.HasMore {
		t.Fatalf("first attachment page bytes=%d reply=%+v", len(raw), first)
	}
	if len(observed) != 1 || !reflect.DeepEqual(observed[0], *first.Page) {
		t.Fatalf("observed page = %+v, reply = %+v", observed, first.Page)
	}
	page := *first.Page
	var got strings.Builder
	got.WriteString(page.Text)
	for page.HasMore {
		next, raw := invokeAttachmentToolContext(t, budget, tools, map[string]any{"id": file.ID, "offset": page.End, "limit": 4000, "version": page.Version})
		if len(raw) > textpage.MaxBytes(budget) || next.Error != "" || next.Page == nil || next.Page.Offset != page.End || next.Page.End <= page.End {
			t.Fatalf("attachment page bytes=%d reply=%+v", len(raw), next)
		}
		page = *next.Page
		got.WriteString(page.Text)
	}
	if got.String() != content {
		t.Fatal("bounded attachment pages did not preserve the complete text")
	}
	if err := store.DeleteAttachment(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	deleted, _ := invokeAttachmentToolContext(t, budget, tools, map[string]any{"id": file.ID, "offset": first.Page.End, "version": first.Page.Version})
	if deleted.Error == "" {
		t.Fatal("later page read succeeded after deletion")
	}
}

func invokeAttachmentToolContext(t *testing.T, ctx context.Context, tools []tool.BaseTool, args any) (attachmentReply, string) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range tools {
		info, err := candidate.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.Name != "read_attachment" {
			continue
		}
		invokable, ok := candidate.(tool.InvokableTool)
		if !ok {
			t.Fatal("read_attachment is not invokable")
		}
		output, err := invokable.InvokableRun(ctx, string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		var reply attachmentReply
		if err := json.Unmarshal([]byte(output), &reply); err != nil {
			t.Fatalf("decode read_attachment reply %q: %v", output, err)
		}
		return reply, output
	}
	t.Fatal("read_attachment tool not found")
	return attachmentReply{}, ""
}
