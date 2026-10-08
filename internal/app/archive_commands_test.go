package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"kagari/internal/domain"
	"kagari/internal/store"
	"kagari/internal/telegram"
)

func archiveCommandWorker(t *testing.T) (*Worker, *store.Store) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &Worker{Store: s, Log: zap.NewNop()}, s
}

func addCommandArchive(t *testing.T, s *store.Store, userID int64, key, title string) int64 {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 10, 3, 23, 30, 0, 0, time.UTC)
	sub := domain.Submission{
		UserID: userID, ChatID: userID, Text: "原文文本 " + key, ForwardedText: "转发内容 " + key,
		Note: "备注 " + key, URLs: []string{"https://example.test/" + key}, ReceivedAt: at, CacheKey: "same-content",
	}
	payload, err := json.Marshal(sub)
	if err != nil {
		t.Fatal(err)
	}
	id, created, err := s.Enqueue(ctx, "analyze", fmt.Sprintf("archive:%d:%s", userID, key), payload, []domain.DeliveryTarget{{Channel: "telegram", Address: fmt.Sprint(sub.ChatID)}})
	if err != nil || !created {
		t.Fatalf("Enqueue() = (%d, %v, %v)", id, created, err)
	}
	if _, err := s.StartJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	source := domain.Source{ID: "article", URL: "https://shared.example/article", RequestedURL: "https://shared.example/article", Status: "ok", Title: title, Content: "来源正文"}
	result, err := json.Marshal(domain.Result{
		Body:    fmt.Sprintf("# %s\n\n## 概述\n分析概述 %s\n\n## 事实\n报告事实 %s [来源](%s)", title, key, key, source.URL),
		Sources: []domain.Source{source},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompletePublication(ctx, id, result, []domain.Delivery{{Target: domain.DeliveryTarget{Channel: "telegram", Address: fmt.Sprint(sub.ChatID)}, Part: 1, Text: "分析投递"}}, []domain.Source{source}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestArchiveCommandPaginationKeepsDuplicateSubmissionsAndScopesOwner(t *testing.T) {
	w, s := archiveCommandWorker(t)
	w.Config.Weekly.Timezone = "Asia/Shanghai"
	ctx := context.Background()
	ids := make([]int64, 12)
	for i := range ids {
		ids[i] = addCommandArchive(t, s, 7, fmt.Sprintf("item-%02d", i), fmt.Sprintf("归档标题 %02d", i))
	}
	otherID := addCommandArchive(t, s, 8, "private-item", "另一位用户的秘密标题")

	first := w.command(ctx, domain.Command{UserID: 7, Text: "/archive"})
	if !strings.Contains(first, "已完成归档（第 1 页）") || !strings.Contains(first, "下一页：/archive 2") || !strings.Contains(first, "/archive_show ") || !strings.Contains(first, "· 2026-10-04") {
		t.Fatalf("first page missing navigation or example: %s", first)
	}
	if strings.Contains(first, fmt.Sprintf("#%d ", ids[0])) || !strings.Contains(first, fmt.Sprintf("#%d ", ids[11])) || strings.Contains(first, "另一位用户的秘密标题") {
		t.Fatalf("first page scope/order is wrong: %s", first)
	}
	second := w.command(ctx, domain.Command{UserID: 7, Text: "/archive 2"})
	if !strings.Contains(second, fmt.Sprintf("#%d ", ids[0])) || !strings.Contains(second, fmt.Sprintf("#%d ", ids[1])) || strings.Contains(second, fmt.Sprintf("#%d ", ids[2])) || strings.Contains(second, "下一页") {
		t.Fatalf("second page is wrong: %s", second)
	}
	if reply := w.command(ctx, domain.Command{UserID: 7, Text: fmt.Sprintf("/archive_show %d", otherID)}); reply != "归档不存在或不属于你" || strings.Contains(reply, "秘密标题") {
		t.Fatalf("unowned title leaked: %s", reply)
	}
}

func TestArchiveShowAndConfirmedDeleteAreScoped(t *testing.T) {
	w, s := archiveCommandWorker(t)
	ctx := context.Background()
	targetID := addCommandArchive(t, s, 7, "target", "目标归档标题")
	siblingID := addCommandArchive(t, s, 7, "sibling", "同用户的另一条归档")
	otherID := addCommandArchive(t, s, 8, "other", "其他用户的归档")

	show := w.command(ctx, domain.Command{UserID: 7, Text: fmt.Sprintf("/archive_show %d", targetID)})
	for _, want := range []string{"原文文本 target", "转发内容 target", "备注 target", "https://example.test/target", "分析概述 target", "报告事实 target", "https://shared.example/article"} {
		if !strings.Contains(show, want) {
			t.Fatalf("archive show missing %q: %s", want, show)
		}
	}
	if entry, err := s.ArchiveEntry(ctx, 7, targetID); err != nil || entry == nil {
		t.Fatalf("show mutated archive: (%+v, %v)", entry, err)
	}

	preview := w.command(ctx, domain.Command{UserID: 7, Text: fmt.Sprintf("/archive_delete %d", targetID)})
	for _, want := range []string{"目标归档标题", "1 条投递记录", "新生成的周报", "已生成的周报快照", "共享来源缓存会保留", "不会撤回", fmt.Sprintf("/archive_delete %d confirm", targetID)} {
		if !strings.Contains(preview, want) {
			t.Fatalf("delete preview missing %q: %s", want, preview)
		}
	}
	if entry, err := s.ArchiveEntry(ctx, 7, targetID); err != nil || entry == nil {
		t.Fatalf("preview mutated archive: (%+v, %v)", entry, err)
	}
	badConfirm := w.command(ctx, domain.Command{UserID: 7, Text: fmt.Sprintf("/archive_delete %d CONFIRM", targetID)})
	if !strings.Contains(badConfirm, "用法") {
		t.Fatalf("non-exact confirmation accepted: %s", badConfirm)
	}

	unowned := w.command(ctx, domain.Command{UserID: 8, Text: fmt.Sprintf("/archive_delete %d confirm", targetID)})
	if unowned != "归档不存在或不属于你" || strings.Contains(unowned, "目标归档标题") {
		t.Fatalf("unowned confirm leaked title: %s", unowned)
	}
	if entry, err := s.ArchiveEntry(ctx, 7, targetID); err != nil || entry == nil {
		t.Fatalf("unowned confirm deleted archive: (%+v, %v)", entry, err)
	}

	deleted := w.command(ctx, domain.Command{UserID: 7, Text: fmt.Sprintf("/archive_delete %d confirm", targetID)})
	if !strings.Contains(deleted, "已删除") {
		t.Fatalf("confirmed delete reply = %s", deleted)
	}
	if entry, err := s.ArchiveEntry(ctx, 7, targetID); err != nil || entry != nil {
		t.Fatalf("confirmed archive still exists: (%+v, %v)", entry, err)
	}
	for _, tc := range []struct {
		userID int64
		id     int64
	}{{7, siblingID}, {8, otherID}} {
		if entry, err := s.ArchiveEntry(ctx, tc.userID, tc.id); err != nil || entry == nil {
			t.Fatalf("delete changed sibling/other archive %d: (%+v, %v)", tc.id, entry, err)
		}
		counts, err := s.DeliveryCounts(ctx, tc.id)
		if err != nil || counts["pending"] != 1 {
			t.Fatalf("delete changed sibling/other deliveries %d: (%v, %v)", tc.id, counts, err)
		}
	}
	if source, err := s.CachedSource(ctx, "https://shared.example/article"); err != nil || source == nil || source.Content != "来源正文" {
		t.Fatalf("delete removed shared source cache: (%+v, %v)", source, err)
	}
}

func TestArchiveCommandsRejectMalformedAndOverflowArguments(t *testing.T) {
	w, s := archiveCommandWorker(t)
	ctx := context.Background()
	id := addCommandArchive(t, s, 7, "keep", "保留的归档")
	for _, text := range []string{
		"/archive x", "/archive 0", "/archive -1", "/archive 922337203685477582", "/archive 1 extra",
		"/archive_show", "/archive_show 0", "/archive_show 9223372036854775808", "/archive_show 1 extra",
		"/archive_delete", "/archive_delete 0", "/archive_delete 1 confirm extra", "/archive_delete 1 Confirm",
	} {
		reply := w.command(ctx, domain.Command{UserID: 7, Text: text})
		if reply == "" || strings.Contains(reply, "已删除") {
			t.Fatalf("malformed command %q returned %q", text, reply)
		}
	}
	if reply := w.command(ctx, domain.Command{Text: "/archive"}); reply != "无法读取归档" {
		t.Fatalf("nonpositive command owner accepted: %s", reply)
	}
	if entry, err := s.ArchiveEntry(ctx, 7, id); err != nil || entry == nil {
		t.Fatalf("malformed command deleted archive: (%+v, %v)", entry, err)
	}
}

func TestArchiveDeleteWaitsForSendingDelivery(t *testing.T) {
	w, s := archiveCommandWorker(t)
	ctx := context.Background()
	id := addCommandArchive(t, s, 7, "sending", "正在投递的归档")
	delivery, err := s.ClaimDelivery(ctx)
	if err != nil || delivery == nil || delivery.JobID != id || delivery.Status != "sending" {
		t.Fatalf("ClaimDelivery() = (%+v, %v)", delivery, err)
	}
	reply := w.command(ctx, domain.Command{UserID: 7, Text: fmt.Sprintf("/archive_delete %d confirm", id)})
	if !strings.Contains(reply, "正在投递") || !strings.Contains(reply, "稍后重试") {
		t.Fatalf("sending delivery reply = %s", reply)
	}
	if entry, err := s.ArchiveEntry(ctx, 7, id); err != nil || entry == nil {
		t.Fatalf("active delivery delete removed archive: (%+v, %v)", entry, err)
	}
}

func TestArchiveListKeepsLongTitlesCompactWithoutChangingDetails(t *testing.T) {
	w, s := archiveCommandWorker(t)
	ctx := context.Background()
	title := strings.Repeat("🐈", 200)
	var id int64
	for i := 0; i < archivePageSize; i++ {
		id = addCommandArchive(t, s, 7, fmt.Sprintf("long-title-%d", i), title)
	}
	listing := w.command(ctx, domain.Command{UserID: 7, Text: "/archive"})
	parts, err := telegram.Adapter(nil).Prepare(domain.Content{Text: listing, Format: domain.ContentPlainText})
	if err != nil || len(parts) != 1 || !strings.Contains(listing, "…") || strings.Contains(listing, title) {
		t.Fatalf("archive page did not fit a compact message: %s", listing)
	}
	preview := w.command(ctx, domain.Command{UserID: 7, Text: fmt.Sprintf("/archive_delete %d", id)})
	if !strings.Contains(preview, "…") || strings.Contains(preview, title) {
		t.Fatalf("delete preview retained the entire long title: %s", preview)
	}
	shown := w.command(ctx, domain.Command{UserID: 7, Text: fmt.Sprintf("/archive_show %d", id)})
	if !strings.Contains(shown, title) {
		t.Fatalf("compact list changed the full archive detail: %s", shown)
	}
}
