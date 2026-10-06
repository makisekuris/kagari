package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/distribution"
	"kagari/internal/domain"
	"kagari/internal/render"
	"kagari/internal/store"
	"kagari/internal/telegram"
)

func TestPublicationFansOutWithoutReanalysisAndFailureIsIsolated(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sub := domain.Submission{UserID: 7, ChatID: 7, CacheKey: "shared"}
	payload, _ := json.Marshal(sub)
	result := domain.Result{AnalysisVersion: agent.AnalysisVersion, CreatedAt: time.Now(), Body: strings.Repeat("正文", 2000)}
	raw, _ := json.Marshal(result)
	seed, _, err := s.Enqueue(ctx, "analyze", "cached", payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartJob(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if err := s.CompletePublication(ctx, seed, raw, nil, nil); err != nil {
		t.Fatal(err)
	}
	targets := telegram.Targets([]int64{7, -100123})
	id, _, err := s.Enqueue(ctx, "analyze", "publication", payload, targets)
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: s, Log: zap.NewNop(), Config: config.Config{MaxAttempts: 3, Reader: config.Reader{CacheTTL: time.Hour}}}
	job, err := s.StartJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// 没有 Engine；缓存复用和多目标投递都不能重新调用模型。
	if err := w.Process(ctx, job); err != nil {
		t.Fatal(err)
	}
	parts := telegram.Adapter(nil).Prepare(fmt.Sprintf("任务 #%d\n%s", id, render.Analysis(result)))
	var channelParts []string
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := distribution.Dispatcher{"telegram": telegram.Adapter(func(_ context.Context, chatID int64, text string) (int64, error) {
		if chatID == 7 {
			return 0, &distribution.SendError{Reason: "timeout", Uncertain: true}
		}
		if chatID != -100123 {
			t.Fatalf("unexpected target %d", chatID)
		}
		channelParts = append(channelParts, text)
		if len(channelParts) == len(parts) {
			cancel()
		}
		return int64(len(channelParts)), nil
	})}
	if err := deliver(sendCtx, w, d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(channelParts, parts) {
		t.Fatalf("channel parts: got %d, want %d", len(channelParts), len(parts))
	}
	status, err := w.Status(ctx, id)
	if err != nil || !strings.Contains(status, "telegram/7 uncertain：1") || !strings.Contains(status, "telegram/-100123 sent："+strconv.Itoa(len(parts))) {
		t.Fatalf("per-target status %q, %v", status, err)
	}
	if err := s.RetryDeliveries(ctx, id); err != nil {
		t.Fatal(err)
	}
	retry, err := s.ClaimDelivery(ctx)
	if err != nil || retry == nil || retry.Target != targets[0] || retry.Part != 1 {
		t.Fatalf("retry = %+v, %v", retry, err)
	}

	// 命令回复只投递到提交私聊，不使用文章分发目标。
	w.Config.Telegram.TargetChatIDs = []int64{7, -100123}
	command, _ := json.Marshal(domain.Command{UserID: 7, ChatID: 7, Text: "/help"})
	commandID, _, err := s.Enqueue(ctx, "command", "help", command, telegram.Targets([]int64{7}))
	if err != nil {
		t.Fatal(err)
	}
	commandJob, err := s.StartJob(ctx, commandID)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Process(ctx, commandJob); err != nil {
		t.Fatal(err)
	}
	counts, err := s.DeliveryTargetCounts(ctx, commandID)
	if err != nil || len(counts) != 1 || counts[0].Target != targets[0] {
		t.Fatalf("command targets = %+v, %v", counts, err)
	}
	w.Config.Weekly.Timezone = "UTC"
	if reply := w.command(ctx, domain.Command{UserID: 7, ChatID: 7, Text: "/weekly"}); !strings.Contains(reply, "周报已排队") {
		t.Fatalf("weekly command: %s", reply)
	}
	weekly, err := s.ClaimJob(ctx)
	if err != nil || weekly == nil || weekly.Kind != "digest" || !reflect.DeepEqual(weekly.Targets, targets) {
		t.Fatalf("weekly targets = %+v, %v", weekly, err)
	}
}
