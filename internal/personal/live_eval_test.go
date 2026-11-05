package personal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/logging"
)

// Run only this suite: KAGARI_LIVE_EVAL=1 KAGARI_EVAL_CONFIG_DIR=/path/to/config go test ./internal/personal -run '^TestLivePersonal' -count=1 -v.
// Core scenarios use eleven model turns; tool iterations and one formatting repair are bounded separately.
// All owner data is fictional.
func TestLivePersonalMemoryLifecycle(t *testing.T) {
	a := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := testStore(t, filepath.Join(t.TempDir(), "memory.db"), 10*time.Minute)
	a.Store = s
	runtime := &Runtime{Store: s, ReviewMemory: a.ReviewMemory}
	var original Memory
	var stableReview MemoryReview

	if !t.Run("automatic_stable_preference", func(t *testing.T) {
		event := openUserEvent(t, s, 1, 1, "我平时一贯喜欢喝无糖绿茶。刚刚结束了一段散步。")
		output := livePersonalThink(t, ctx, a, livePersonalInput(t, ctx, s, event))
		finishEvent(t, s, event, output, nil)
		runtime.ReviewMemory = func(ctx context.Context, input MemoryReviewInput) (MemoryReview, error) {
			review, err := a.ReviewMemory(ctx, input)
			stableReview = review
			return review, err
		}
		livePersonalMemoryStep(t, ctx, runtime)
		memories, err := s.Memories(ctx, "", 20)
		if err != nil || len(stableReview.Changes) != 1 || len(memories) != 1 || !strings.Contains(memories[0].Value, "无糖绿茶") {
			t.Fatalf("semantic FAIL: ordinary stable preference was not persisted as one memory (active=%d error=%s)", len(memories), logging.ErrorReason(err))
		}
		original = memories[0]
		t.Log("mechanism PASS: Think → CompleteEvent → memory worker → real SQLite; semantic PASS: automatic stable preference")
	}) {
		return
	}
	runtime.ReviewMemory = a.ReviewMemory

	t.Run("temporary_change_does_not_overwrite", func(t *testing.T) {
		event := openUserEvent(t, s, 2, 2, "今天只是这一次改喝加糖红茶，明天恢复平时习惯；这不是长期偏好的改变。")
		// Isolate the background extractor: the owner event is completed without another conversational model call.
		finishEvent(t, s, event, TurnOutput{Text: "收到。"}, nil)
		livePersonalMemoryStep(t, ctx, runtime)
		got, err := s.Memory(ctx, original.Key)
		active, listErr := s.Memories(ctx, "", 20)
		if err != nil || listErr != nil || got == nil || got.Version != original.Version || got.Value != original.Value || len(active) != 1 {
			t.Fatal("semantic FAIL: temporary change replaced or added a durable preference")
		}
		t.Log("mechanism PASS: automatic review completed; semantic PASS: temporary preference did not alter durable memory")
	})

	t.Run("correction_reuses_key", func(t *testing.T) {
		event := openUserEvent(t, s, 3, 3, "纠正一下我之前说的长期饮料偏好：我平时一贯喜欢的是无糖乌龙茶，不是无糖绿茶。")
		finishEvent(t, s, event, TurnOutput{Text: "收到更正。"}, nil)
		livePersonalMemoryStep(t, ctx, runtime)
		got, err := s.Memory(ctx, original.Key)
		active, listErr := s.Memories(ctx, "", 20)
		if err != nil || listErr != nil || got == nil || got.Version != original.Version+1 || !strings.Contains(got.Value, "无糖乌龙茶") || len(active) != 1 {
			t.Fatal("semantic FAIL: explicit correction did not update the original key exactly once")
		}
		t.Log("mechanism PASS: versioned SQLite update; semantic PASS: correction reused original memory key")
	})
}

func TestLivePersonalForgetAndHistory(t *testing.T) {
	a := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := testStore(t, filepath.Join(t.TempDir(), "forget.db"), 10*time.Minute)
	a.Store = s
	runtime := &Runtime{Store: s, ReviewMemory: a.ReviewMemory}
	seed := openUserEvent(t, s, 1, 1, "我平时一贯喜欢喝无糖乌龙茶。")
	finishEvent(t, s, seed, TurnOutput{}, nil)
	stableRun, err := s.ClaimRun(ctx, RunMemoryReview)
	if err != nil || stableRun == nil {
		t.Fatal("cannot claim fictional seed review")
	}
	stableReview := MemoryReview{Changes: []MemoryChange{{Operation: "put", Key: "drink.preference", Value: "无糖乌龙茶", Evidence: seed.Text}}}
	if err := s.FinishMemoryReview(ctx, stableRun.ID, stableReview, ""); err != nil {
		t.Fatal(err)
	}
	original, err := s.Memory(ctx, "drink.preference")
	if err != nil || original == nil || original.Deleted {
		t.Fatal("cannot persist fictional seed preference")
	}
	if !t.Run("forget_and_old_review_replay", func(t *testing.T) {
		event := openUserEvent(t, s, 2, 2, "请忘掉我的长期饮料偏好，以后不要从旧聊天恢复它。")
		output := livePersonalThink(t, ctx, a, livePersonalInput(t, ctx, s, event))
		forgotten, err := s.Memory(ctx, original.Key)
		if err != nil || forgotten == nil || !forgotten.Deleted || forgotten.Value != "" || forgotten.Version != original.Version+1 {
			t.Fatal("mechanism FAIL: explicit forgetting was not persisted by the tool call")
		}
		finishEvent(t, s, event, output, nil)
		livePersonalMemoryStep(t, ctx, runtime)
		got, err := s.Memory(ctx, original.Key)
		if err != nil || got == nil || !got.Deleted || got.Value != "" || got.Version != original.Version+1 {
			t.Fatal("semantic FAIL: explicit forgetting did not persist an empty tombstone")
		}
		if err := s.FinishMemoryReview(ctx, stableRun.ID, stableReview, ""); err != nil {
			t.Fatal(err)
		}
		got, err = s.Memory(ctx, original.Key)
		if err != nil || got == nil || !got.Deleted || got.Value != "" {
			t.Fatal("mechanism FAIL: replaying a completed old review resurrected forgotten data")
		}
		t.Log("mechanism PASS: forget tool → synchronous write → tombstone; semantic PASS: old review replay did not restore the value")
	}) {
		return
	}

	t.Run("old_chat_is_not_active_recall", func(t *testing.T) {
		event := openUserEvent(t, s, 3, 3, "请只根据当前有效的记忆告诉我，我的长期饮料偏好是什么？如果已遗忘，就说已经遗忘，不要从聊天历史推回。")
		output := livePersonalThink(t, ctx, a, livePersonalInput(t, ctx, s, event))
		finishEvent(t, s, event, output, nil)
		if strings.Contains(output.Text, "乌龙") || strings.Contains(output.Text, "绿茶") || !strings.Contains(output.Text, "忘") {
			t.Fatal("semantic FAIL: answer reconstructed a forgotten preference or failed to acknowledge forgetting")
		}
		active, err := s.Memories(ctx, "", 20)
		if err != nil || len(active) != 0 {
			t.Fatal("mechanism FAIL: forgotten preference remains in active recall")
		}
		t.Log("mechanism PASS: old conversation remained available; semantic PASS: forgotten value was not reconstructed")
	})
}

func TestLivePersonalRecallAfterRestart(t *testing.T) {
	a := livePersonalAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	path := filepath.Join(t.TempDir(), "recall.db")
	s := testStore(t, path, 10*time.Minute)
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	code := hex.EncodeToString(random)
	event := openUserEvent(t, s, 1, 1, "请记住我的虚构编辑器配色代号是 "+code+"。")
	if _, err := s.WriteMemory(ctx, event.ID, "seed-editor", MemoryChange{Operation: "put", Key: "editor.color_code", Value: "虚构编辑器配色代号 " + code, Evidence: event.Text}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, event, TurnOutput{}, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = testStore(t, path, 10*time.Minute)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	spy := &livePersonalRecallStore{Store: s}
	a.Store = spy
	event = openUserEvent(t, s, 2, 2, "我的编辑器配色代号是什么？请查询长期记忆后原样回答。")
	// Omit both memory preload and history: the random answer is available only through real repository tools.
	output := livePersonalThink(t, ctx, a, TurnInput{Event: event})
	finishEvent(t, s, event, output, nil)
	if spy.reads == 0 {
		t.Fatal("mechanism FAIL: no memory retrieval tool reached the restarted Store")
	}
	if !strings.Contains(output.Text, code) {
		t.Fatal("semantic FAIL: retrieved answer did not contain the persisted random value")
	}
	t.Logf("mechanism PASS: restarted SQLite and %d memory tool reads; semantic PASS: correct persisted value recalled", spy.reads)
}

func TestLivePersonalTaskCallbacks(t *testing.T) {
	a := livePersonalAgent(t)
	for _, outcome := range []string{"continue_original_goal", "settle_completed_goal"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s := testStore(t, filepath.Join(t.TempDir(), "tasks.db"), 10*time.Minute)
			a.Store = s
			seed := openUserEvent(t, s, 1, 1, "比较虚构产品甲和乙的月费与离线能力，两项都需要核实，最后给我一个对照结论。")
			task, run := createTask(t, s, seed, "seed-task", "虚构产品对比", seed.Text, "仅处理本测试提供的虚构产品材料。")
			finishEvent(t, s, seed, TurnOutput{TaskID: task.ID}, nil)
			claimed, err := s.ClaimRun(ctx, RunTask)
			if err != nil || claimed == nil || claimed.ID != run.ID {
				t.Fatal("mechanism FAIL: task seed was not claimable")
			}
			result := StageResult{Summary: "已核实月费：甲每月10元，乙每月20元。离线能力尚未核实，原目标尚未完成。下一步需继续核实甲、乙的离线能力，再给出包含月费与离线能力的对照结论。"}
			if outcome == "settle_completed_goal" {
				result = StageResult{Summary: "核实已全部完成：甲月费10元，支持离线；乙月费20元，不支持离线。完整对照结论：甲更便宜且有离线能力；乙月费更高且无离线能力。原目标要求的两项已全部有明确依据，无后续事项。"}
			}
			if err := s.FinishStage(ctx, run.ID, result, ""); err != nil {
				t.Fatal(err)
			}
			event := livePersonalTaskEvent(t, ctx, s, task.ID)
			output := livePersonalThink(t, ctx, a, livePersonalInput(t, ctx, s, event))
			finishEvent(t, s, event, output, nil)
			got, err := s.Task(ctx, task.ID)
			runs, listErr := s.Runs(ctx, task.ID, 10)
			if err != nil || listErr != nil || got == nil || got.Goal != task.Goal || got.Revision != task.Revision || got.BudgetMS != task.BudgetMS {
				t.Fatal("mechanism FAIL: callback lost or changed the original task, goal, revision, or budget")
			}
			if outcome == "continue_original_goal" {
				if got.Status != TaskQueued || len(runs) != 2 {
					t.Fatalf("semantic FAIL: callback did not continue its own original task (status=%s runs=%d)", got.Status, len(runs))
				}
				t.Log("mechanism PASS: callback queued another run; semantic PASS: original task, goal, revision, and budget preserved")
			} else {
				if got.Status != TaskCompleted || len(runs) != 1 || output.Silent {
					t.Fatalf("semantic FAIL: complete callback evidence was not settled and notified (status=%s runs=%d silent=%v)", got.Status, len(runs), output.Silent)
				}
				t.Log("mechanism PASS: settle_task persisted completed and notified; semantic PASS: completion stayed within original goal")
			}
		})
	}
}

func TestLivePersonalReadingStage(t *testing.T) {
	engine := livePersonalEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := testStore(t, filepath.Join(t.TempDir(), "reading.db"), 10*time.Minute)
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	code := hex.EncodeToString(random)
	const pageURL = "https://example.org/personal-eval/evidence"
	reads := 0
	engine.Read = func(_ context.Context, url string) (domain.Source, error) {
		reads++
		if url != pageURL {
			return domain.Source{URL: url, Status: "failed", Reason: "unknown fixture URL"}, nil
		}
		return domain.Source{ID: "eval_evidence", URL: url, RequestedURL: url, Title: "虚构证据页", Content: "本次虚构实验唯一校验码是：" + code, Status: "ok", ReadMethod: "fixture", FetchedAt: time.Now().UTC()}, nil
	}
	engine.Config.Agent.MaxIterations = 5
	seed := openUserEvent(t, s, 1, 1, "读取 "+pageURL+"，告诉我页面中的本次实验唯一校验码，并引用证据页；不能猜测。")
	task, _ := createTask(t, s, seed, "seed-reading", "虚构页面读取", seed.Text, "仅使用本测试的受控页面。")
	finishEvent(t, s, seed, TurnOutput{TaskID: task.ID}, nil)
	run, err := s.ClaimRun(ctx, RunTask)
	if err != nil || run == nil {
		t.Fatal("cannot claim fictional reading task")
	}
	started := time.Now()
	result, executeErr := TaskExecutor(engine, s, t.TempDir())(ctx, *run, nil)
	if err := s.FinishStage(ctx, run.ID, result, logging.ErrorReason(executeErr)); err != nil {
		t.Fatal(err)
	}
	t.Logf("reading stage duration=%s reads=%d sources=%d error=%s", time.Since(started).Round(time.Millisecond), reads, len(result.Sources), logging.ErrorReason(executeErr))
	if reads == 0 {
		t.Fatal("mechanism FAIL: TaskExecutor did not invoke read_url against the controlled page")
	}
	persisted, err := s.Run(ctx, run.ID)
	if executeErr != nil || err != nil || persisted == nil || persisted.Status != RunCompleted || !strings.Contains(result.Summary, code) || len(result.Sources) != 1 || !strings.Contains(persisted.Result, code) {
		t.Fatal("semantic FAIL: reading stage did not return and persist the random evidence value")
	}
	t.Log("mechanism PASS: TaskExecutor read_url → controlled page → persisted real Store stage; semantic PASS: answer contains random source evidence")
}

func livePersonalAgent(t *testing.T) *MainAgent {
	t.Helper()
	engine := livePersonalEngine(t)
	return &MainAgent{Model: engine.Model, Profile: engine.Profile, MaxIterations: 5, Timeout: 2 * time.Minute}
}

func livePersonalEngine(t *testing.T) *agent.Engine {
	t.Helper()
	if os.Getenv("KAGARI_LIVE_EVAL") != "1" {
		t.Skip("opt-in real-model personal evaluation")
	}
	dir := os.Getenv("KAGARI_EVAL_CONFIG_DIR")
	if dir == "" {
		t.Fatal("set KAGARI_EVAL_CONFIG_DIR to a usable local configuration directory")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal("cannot resolve evaluation working directory")
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal("cannot read evaluation configuration directory")
	}
	cfg, loadErr := config.Load("")
	chdirErr := os.Chdir(cwd)
	if loadErr != nil || chdirErr != nil {
		t.Fatal("evaluation configuration is unusable; check local config.yaml and .env (contents withheld)")
	}
	cfg.ProfilePath = filepath.Join(t.TempDir(), "empty-profile.md")
	if err := os.WriteFile(cfg.ProfilePath, nil, 0600); err != nil {
		t.Fatal("cannot create isolated empty profile")
	}
	cfg.Model.Timeout = 45 * time.Second
	cfg.Model.MaxOutputTokens = 2500
	cfg.Agent.Timeout = 2 * time.Minute
	cfg.Agent.Streaming = false
	transport := http.DefaultTransport
	http.DefaultTransport = &livePersonalTransport{next: transport, t: t}
	t.Cleanup(func() { http.DefaultTransport = transport })
	engine, err := agent.New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatalf("cannot construct live evaluation model: %s", logging.ErrorReason(err))
	}
	return engine
}

func livePersonalThink(t *testing.T, ctx context.Context, a *MainAgent, input TurnInput) TurnOutput {
	t.Helper()
	started := time.Now()
	output, err := a.Think(ctx, input)
	t.Logf("real-model Think duration=%s success=%v event_kind=%s event_id=%d task_id=%d memories=%d", time.Since(started).Round(time.Millisecond), err == nil, input.Event.Kind, input.Event.ID, input.Event.TaskID, len(input.Memories))
	if err == nil {
		t.Logf("fictional evaluation response=%q", output.Text)
	}
	if err != nil {
		reason := logging.ErrorReason(err)
		if strings.Contains(err.Error(), "personal response") || strings.Contains(err.Error(), "model returned blank") {
			reason = err.Error()
		}
		t.Fatalf("real-model Think failed: %s (configuration withheld)", reason)
	}
	return output
}

func livePersonalInput(t *testing.T, ctx context.Context, s *Store, event Event) TurnInput {
	t.Helper()
	input, err := (&Runtime{Store: s}).context(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func livePersonalMemoryStep(t *testing.T, ctx context.Context, runtime *Runtime) {
	t.Helper()
	started := time.Now()
	worked, err := runtime.memory(ctx)
	if err != nil || !worked {
		t.Fatalf("memory worker did not finish: worked=%v error=%s", worked, logging.ErrorReason(err))
	}
	var failed int
	if err := runtime.Store.(*Store).db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_runs WHERE kind IN ('memory','memory_review') AND status='failed'`).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatal("mechanism FAIL: memory worker persisted a failed run (model output withheld)")
	}
	t.Logf("memory worker duration=%s", time.Since(started).Round(time.Millisecond))
}

func livePersonalTaskEvent(t *testing.T, ctx context.Context, s *Store, taskID int64) Event {
	t.Helper()
	event, err := s.ClaimEvent(ctx)
	if err != nil || event == nil || event.Kind != EventTask || event.TaskID != taskID {
		t.Fatal("mechanism FAIL: expected persisted callback for current task")
	}
	return *event
}

type livePersonalRecallStore struct {
	*Store
	reads int
}

func (s *livePersonalRecallStore) Memory(ctx context.Context, key string) (*Memory, error) {
	s.reads++
	return s.Store.Memory(ctx, key)
}

func (s *livePersonalRecallStore) Memories(ctx context.Context, query string, limit int) ([]Memory, error) {
	s.reads++
	return s.Store.Memories(ctx, query, limit)
}

// Observe protocol metadata and fictional output; never log input, headers, endpoint, or model configuration.
type livePersonalTransport struct {
	next          http.RoundTripper
	t             *testing.T
	printedSchema bool
}

func (transport *livePersonalTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.GetBody != nil {
		body, err := request.GetBody()
		if err == nil {
			var metadata struct {
				ToolChoice json.RawMessage `json:"tool_choice"`
				Text       struct {
					Format struct {
						Type   string `json:"type"`
						Strict *bool  `json:"strict"`
					} `json:"format"`
				} `json:"text"`
				Tools []struct {
					Name       string `json:"name"`
					Strict     *bool  `json:"strict"`
					Parameters struct {
						Required []string `json:"required"`
					} `json:"parameters"`
				} `json:"tools"`
			}
			err = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&metadata)
			_ = body.Close()
			if err == nil {
				names := make([]string, 0, len(metadata.Tools))
				for _, tool := range metadata.Tools {
					names = append(names, tool.Name)
					strict := "omitted"
					if tool.Strict != nil {
						if *tool.Strict {
							strict = "true"
						} else {
							strict = "false"
						}
					}
					if !transport.printedSchema {
						transport.t.Logf("request tool=%s strict=%s required=%v", tool.Name, strict, tool.Parameters.Required)
					}
				}
				if !transport.printedSchema {
					transport.t.Logf("request tool names=%v", names)
					transport.printedSchema = true
				}
				transport.t.Logf("real HTTP request tools=%d tool_choice=%s text.format.type=%s", len(names), metadata.ToolChoice, metadata.Text.Format.Type)
			}
		}
	}
	response, err := transport.next.RoundTrip(request)
	if err != nil || !strings.Contains(response.Header.Get("Content-Type"), "application/json") {
		return response, err
	}
	raw, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(raw))
	if readErr == nil {
		var metadata struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(raw, &metadata) == nil {
			types := make([]string, 0, len(metadata.Output))
			for _, item := range metadata.Output {
				types = append(types, item.Type)
			}
			transport.t.Logf("real HTTP response output types=%v", types)
			if !strings.Contains(strings.Join(types, ","), "function_call") {
				for _, item := range metadata.Output {
					for _, content := range item.Content {
						if content.Type == "output_text" {
							valid := json.Valid([]byte(content.Text))
							transport.t.Logf("fictional final text JSON valid=%v fenced=%v", valid, strings.HasPrefix(strings.TrimSpace(content.Text), "```"))
							if !valid {
								transport.t.Logf("fictional final raw text=%q", content.Text)
							}
						}
					}
				}
			}
		}
	}
	return response, readErr
}
