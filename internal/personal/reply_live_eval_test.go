package personal

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"kagari/internal/channel"
	"kagari/internal/domain"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestLivePersonalBareLinksReply(t *testing.T) {
	engine := livePersonalEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s := testStore(t, filepath.Join(t.TempDir(), "reading.db"), 2*time.Minute)
	main := &MainAgent{Model: engine.Model, Store: s, Profile: "请用中文回复。", MaxIterations: 5, Timeout: 2 * time.Minute}
	const overview = "https://example.org/fictional-meridian/overview"
	const offline = "https://example.org/fictional-meridian/offline"
	pages := map[string]string{
		overview: "虚构产品 Meridian 用于协作编辑，每月收费 17 元。适用前提请结合离线能力页判断：" + offline,
		offline:  "虚构产品 Meridian 必须联网使用，不支持离线编辑。需要离线工作的用户不适合选择它。",
	}
	read := map[string]bool{}
	engine.Read = func(_ context.Context, url string) (domain.Source, error) {
		body, ok := pages[url]
		if !ok {
			return domain.Source{URL: url, Status: "failed", Reason: "outside supplied material"}, nil
		}
		read[url] = true
		return domain.Source{URL: url, RequestedURL: url, Title: "Meridian 产品说明", Content: body, Status: "ok", ReadMethod: "fixture", FetchedAt: time.Now()}, nil
	}
	r := testRuntime(s)
	logs, observed := observer.New(zapcore.InfoLevel)
	r.Log = zap.New(logs)
	t.Cleanup(func() {
		if t.Failed() {
			for _, entry := range observed.FilterMessage("tool.finished").All() {
				fields := entry.ContextMap()
				payload, _ := json.Marshal(fields["payload"])
				t.Logf("fictional tool=%v outcome=%v payload=%s", fields["tool_name"], fields["outcome"], payload)
			}
		}
	})
	r.Think = func(ctx context.Context, input TurnInput) (TurnOutput, error) {
		return livePersonalThink(t, ctx, main, input), nil
	}
	r.Execute = TaskExecutor(engine, s, t.TempDir())
	acceptInput(t, s, "1", sourceInput("1", overview))
	acceptInput(t, s, "2", sourceInput("2", offline))
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("initial turn: %v, %v", worked, err)
	}
	if delivery, err := s.ClaimDelivery(ctx); err != nil || delivery != nil {
		t.Fatalf("bare links produced an immediate receipt: %+v, %v", delivery, err)
	}
	tasks, err := s.Tasks(ctx, 10)
	if err != nil || len(tasks) != 1 || tasks[0].Status != TaskQueued {
		t.Fatalf("related links did not create one queued reading task: %+v, %v", tasks, err)
	}
	var boundInputs int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_task_inputs WHERE task_id=?`, tasks[0].ID).Scan(&boundInputs); err != nil || boundInputs != 2 {
		t.Fatalf("related input binding: count=%d error=%v", boundInputs, err)
	}
	for stage := 0; stage < 3; stage++ {
		if worked, err := r.task(ctx); err != nil || !worked {
			t.Fatalf("reading stage: %v, %v", worked, err)
		}
		if worked, err := r.turn(ctx); err != nil || !worked {
			t.Fatalf("result turn: %v, %v", worked, err)
		}
		task, err := s.Task(ctx, tasks[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != TaskQueued {
			break
		}
	}
	task, err := s.Task(ctx, tasks[0].ID)
	if err != nil || task == nil || task.Status != TaskCompleted {
		t.Fatalf("bounded reading did not finish: %+v, %v", task, err)
	}
	if !read[overview] || !read[offline] {
		t.Fatalf("completed reading missed supplied pages: %v", read)
	}
	var reply strings.Builder
	for {
		delivery, err := s.ClaimDelivery(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if delivery == nil {
			break
		}
		reply.WriteString(delivery.Text)
		reply.WriteByte('\n')
		if err := s.UpdateDelivery(ctx, delivery.ID, "sent", channel.Receipt{MessageID: fmt.Sprint(delivery.ID)}, "", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	text := reply.String()
	if !strings.Contains(text, "17") || !(strings.Contains(text, "不支持离线") || strings.Contains(text, "不能离线") || strings.Contains(text, "必须联网")) {
		t.Fatalf("reply lacks price, limitation or evidence: %s", text)
	}
	if regexp.MustCompile(`(?i)(?:task|run|任务)\s*(?:编号|号|#)?\s*\d+`).MatchString(text) {
		t.Fatalf("ordinary reading exposes an internal ID: %s", text)
	}
	t.Logf("PASS: two related links → one silent task → both pages read → evidence-based reply: %s", text)
}
