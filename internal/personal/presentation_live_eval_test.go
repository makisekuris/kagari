package personal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"kagari/internal/logging"
)

func TestLivePersonalFinalPresentation(t *testing.T) {
	a := livePersonalAgent(t)
	for _, tc := range []struct {
		name, request, draft string
		want, absent         []string
	}{
		{
			name:    "closing result",
			request: "请比较白桦方案，告诉我价格、限制和来源。",
			draft:   "「白桦对照」已经处理完了。\n\n「白桦对照」任务已全部完成喵～\n\n**已完成内容**\n白桦方案每月 17 元，必须联网，校验码 BIRCH-7Q9。\nTask #3、Run #11 的 checkpoint 已保存，工具调用全部结束。\n\n**保留来源**\n- https://example.invalid/birch\n- https://example.invalid/limits\n\n「白桦对照」任务已全部完成。",
			want:    []string{"白桦", "17", "月", "联网", "BIRCH-7Q9", "https://example.invalid/birch", "https://example.invalid/limits"},
			absent:  []string{"**已完成", "**保留", "Task #", "Run #", "checkpoint", "工具调用"},
		},
		{
			name:    "schedule receipt",
			request: "请登记每天 09:00 UTC 的练习计划，每次独立生成，最多执行 10 分钟。",
			draft:   "那是我不小心重复调用工具，实际只注册了一个计划喵。create_schedule 返回 replayed=true。\n\n**登记好了**\n练习计划已建立，每天 09:00 UTC，首次 2026-12-01 09:00 UTC；window_mode=none，每次独立生成，task_budget_ms=600000，也就是每次最多执行 10 分钟。现在不会立即执行。\n\n练习计划已经登记好了。需要改时间、停掉或调整，随时吩咐喵。",
			want:    []string{"09:00", "UTC", "2026-12-01", "10", "分钟", "独立"},
			absent:  []string{"**", "create_schedule", "replayed", "window_mode", "task_budget_ms", "不小心", "重复调用", "随时吩咐"},
		},
		{
			name:    "short empty inventory",
			request: "你现在的定时任务",
			draft:   "当前没有定时任务，列表为空。",
			want:    []string{"没有", "定时任务"},
			absent:  []string{"列表", "随时", "吩咐"},
		},
		{
			name:    "schedule inventory",
			request: "你现在的定时任务",
			draft:   "小菲查过了喵，目前你名下没有任何正在生效的定时任务啦。\n\n你之前登记的几个临时计划（E2E 每日练习、E2E 文案回归、E2E 文案回归复测、E2E 最终文案检查）现在全部都是已取消状态，也就是都不再自动执行了，每天 09:00 UTC 的 Go 练习题触发也都一并移除了。\n\n现在定时任务列表是空的，没有任何计划在跑，小菲这边完全空闲喵～ 之后想重新登记或者新建计划，随时吩咐就好喵。",
			want:    []string{"没有", "E2E 每日练习", "E2E 文案回归", "E2E 文案回归复测", "E2E 最终文案检查", "已取消", "09:00", "UTC"},
			absent:  []string{"随时", "吩咐", "空闲", "重新登记", "列表是空的"},
		},
		{
			name:    "required question and failure",
			request: "请核对我的账户价格。",
			draft:   "**还不能确认**\n来源访问失败，未能确认价格，检查不完整。需要你提供账户地区才能继续核对。\n本阶段来源：https://example.invalid/account\n\n需要你提供账户地区才能继续核对。",
			want:    []string{"账户地区", "价格", "访问", "https://example.invalid/account"},
			absent:  []string{"**", "已确认价格", "检查完整"},
		},
		{
			name:    "requested technical literals",
			request: "请解释 window_mode=none 和 since_success，并原样展示代码 `**literal**` 与 foo_bar(x)。",
			draft:   "`window_mode=none` 表示每次独立生成；`since_success` 表示从上次成功检查之后继续。代码原样如下：\n```text\n**literal**\nfoo_bar(x)\n```\n\n这就是上述两个示例，按你的要求只做文字展示，没有附代码语言注释或额外加工喵。",
			want:    []string{"window_mode=none", "since_success", "**literal**", "foo_bar(x)"},
			absent:  []string{"```", "`window_mode", "按你的要求", "额外加工"},
		},
		{
			name:    "existing finding with queued followup",
			request: "请重新阅读 https://example.invalid/birch 和 https://example.invalid/pricing，核实最新价格并附上来源。",
			draft:   "旧摘要记录白桦方案每月 17 元。本次后台复核已排队，尚无新结果，完成后再汇报。",
			want:    []string{"17", "月"},
			absent:  []string{"已重新阅读", "已完成核对", "https://"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			core, logs := observer.New(zap.WarnLevel)
			ctx = logging.WithLogger(ctx, zap.New(core))
			s := testStore(t, filepath.Join(t.TempDir(), "presentation.db"), time.Minute)
			a.Store = s
			r := testRuntime(s)
			r.PresentReply = func(ctx context.Context, event Event, draft string) (string, error) {
				text, err := a.PresentReply(ctx, event, draft)
				t.Logf("presenter draft=%q", text)
				if err != nil {
					t.Errorf("presentation failed: %s (configuration withheld)", logging.ErrorReason(err))
				}
				return text, err
			}
			event := openUserEvent(t, s, 1, 1, tc.request)
			if err := r.publish(ctx, event, TurnOutput{Text: tc.draft}); err != nil {
				t.Fatal(err)
			}
			if failures := logs.FilterMessage("conversation.reply.presentation_failed").All(); len(failures) != 0 {
				t.Logf("presentation rejected; validating the retained draft: %v", failures)
			}
			var text strings.Builder
			rows, err := s.db.QueryContext(ctx, `SELECT text FROM personal_outbox WHERE event_id=? ORDER BY part`, event.ID)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var part string
				if err := rows.Scan(&part); err != nil {
					t.Fatal(err)
				}
				text.WriteString(part)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			got := text.String()
			t.Logf("final persisted reply=%q", got)
			for _, value := range tc.want {
				if !strings.Contains(got, value) {
					t.Errorf("reply omitted %q", value)
				}
			}
			for _, value := range tc.absent {
				if strings.Contains(got, value) {
					t.Errorf("reply retained %q", value)
				}
			}
			if tc.name == "required question and failure" && !strings.Contains(got, "失败") && !strings.Contains(got, "无法") && !strings.Contains(got, "未能") {
				t.Error("reply lost the failed-source state")
			}
			if tc.name == "required question and failure" && strings.Count(got, "账户地区") != 1 {
				t.Error("reply repeats or drops the required question")
			}
			if tc.name == "closing result" && strings.Count(got, "白桦对照") > 1 {
				t.Error("reply repeats the same task-closing announcement")
			}
			if tc.name == "existing finding with queued followup" && !strings.Contains(got, "排队") && !strings.Contains(got, "等待") {
				t.Error("reply lost the queued follow-up state")
			}
			var history string
			if err := s.db.QueryRowContext(ctx, `SELECT text FROM personal_history WHERE event_id=? AND role='assistant'`, event.ID).Scan(&history); err != nil || history != got {
				t.Fatalf("history and delivered text differ: history=%q reply=%q error=%v", history, got, err)
			}
		})
	}
}
