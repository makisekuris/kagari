package personal

import (
	"context"
	"encoding/json"
	"errors"
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

type retryInputStore struct {
	*Store
	inputAttempts int
}

func (s *retryInputStore) AddTaskInputs(ctx context.Context, eventID int64, key string, taskID, revision int64, inputIDs []int64) (Task, error) {
	s.inputAttempts++
	if s.inputAttempts == 1 {
		return Task{}, errors.New("temporary input-storage failure before commit; retry this operation with the returned action_id")
	}
	return s.Store.AddTaskInputs(ctx, eventID, key, taskID, revision, inputIDs)
}

func TestLivePersonalRecoversTaskActionWithoutReceipt(t *testing.T) {
	engine := livePersonalEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	s := testStore(t, filepath.Join(t.TempDir(), "retry.db"), time.Minute)
	retrying := &retryInputStore{Store: s}
	main := &MainAgent{Model: engine.Model, Store: retrying, Profile: "请用中文回复。", MaxIterations: engine.Config.Agent.MaxIterations, Timeout: 2 * time.Minute}
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
	r.Think = main.Think
	original := openUserEvent(t, s, 1, 1, "阅读这份资料：https://example.org/fictional-meridian/overview")
	task, _, err := s.CreateTask(ctx, original.ID, "read", "Meridian 资料核对", TaskInput{Goal: "阅读用户提供的 Meridian 产品资料，并核对使用限制。"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskInputs(ctx, original.ID, "add-original-input", task.ID, task.Revision, []int64{original.ID}); err != nil {
		t.Fatal(err)
	}
	finishEvent(t, s, original, TurnOutput{TaskID: task.ID, Silent: true}, nil)
	run, err := s.ClaimRun(ctx, RunTask)
	if err != nil || run == nil {
		t.Fatalf("start original reading: %+v %v", run, err)
	}
	const offlineURL = "https://example.org/fictional-meridian/offline"
	const collaborationURL = "https://example.org/fictional-meridian/collaboration"
	supplement := sourceInput("2", offlineURL)
	supplement.ReplyToID = original.MessageID
	supplementID := acceptInput(t, s, "2", supplement)
	secondSupplement := sourceInput("3", collaborationURL)
	secondSupplement.ReplyToID = original.MessageID
	secondSupplementID := acceptInput(t, s, "3", secondSupplement)
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("intake turn=%v %v", worked, err)
	}
	if retrying.inputAttempts < 2 {
		t.Fatalf("material addition was not retried: %d attempts", retrying.inputAttempts)
	}
	var pending int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_task_inputs WHERE task_id=? AND event_id IN (?,?) AND status='pending'`, task.ID, supplementID, secondSupplementID).Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("late materials not pending for the existing task: count=%d error=%v", pending, err)
	}
	tasks, err := s.Tasks(ctx, 10)
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID || tasks[0].Revision != task.Revision || tasks[0].Status != TaskRunning {
		t.Fatalf("retry created extra tasks: count=%d error=%v", len(tasks), err)
	}
	active, err := s.Run(ctx, run.ID)
	if err != nil || active == nil || active.Input != run.Input || active.Status != RunRunning {
		t.Fatalf("late material changed the active run: %+v %v", active, err)
	}
	if delivery, err := s.ClaimDelivery(ctx); err != nil || delivery != nil {
		t.Fatalf("recovered action produced a receipt or stale failure: %+v %v", delivery, err)
	}

	var sawPendingLinks, sawSuccessfulAddition bool
	var injectedFailureActionID, retryActionID string
	for _, entry := range observed.All() {
		fields := entry.ContextMap()
		if fields["event_id"] != supplementID {
			continue
		}
		if entry.Message == "model.started" {
			payload, _ := json.Marshal(fields["payload"])
			request := string(payload)
			if strings.Contains(request, "pending_inputs") && strings.Contains(request, offlineURL) && strings.Contains(request, collaborationURL) {
				sawPendingLinks = true
			}
		}
		if entry.Message == "tool.finished" && fields["tool_name"] == "add_task_input" {
			payload, err := json.Marshal(fields["payload"])
			if err != nil {
				t.Fatalf("encode add_task_input trace: %v", err)
			}
			var trace struct {
				Input struct {
					ArgumentsInJSON string `json:"ArgumentsInJSON"`
				} `json:"input"`
				Output struct {
					Response string `json:"Response"`
				} `json:"output"`
			}
			if err := json.Unmarshal(payload, &trace); err != nil {
				t.Fatalf("decode add_task_input trace payload %s: %v", payload, err)
			}
			var args addTaskInputArgs
			if err := json.Unmarshal([]byte(trace.Input.ArgumentsInJSON), &args); err != nil {
				t.Fatalf("decode add_task_input arguments %q: %v", trace.Input.ArgumentsInJSON, err)
			}
			var result addTaskInputReply
			if err := json.Unmarshal([]byte(trace.Output.Response), &result); err != nil {
				t.Fatalf("decode add_task_input response %q: %v", trace.Output.Response, err)
			}
			switch fields["outcome"] {
			case "business_failure":
				if injectedFailureActionID == "" && strings.Contains(result.Error, "temporary input-storage failure") {
					injectedFailureActionID = result.ActionID
					if injectedFailureActionID == "" {
						t.Fatalf("injected add_task_input failure omitted action_id: %s", payload)
					}
				}
			case "success":
				if injectedFailureActionID != "" && retryActionID == "" {
					retryActionID = args.ActionID
					if retryActionID != injectedFailureActionID {
						t.Fatalf("first successful add_task_input correction did not reuse injected failure action_id: failed_action_id=%q retry_action_id=%q", injectedFailureActionID, retryActionID)
					}
				}
				if strings.Contains(string(payload), offlineURL) && strings.Contains(string(payload), collaborationURL) {
					sawSuccessfulAddition = true
				}
			}
		}
		if entry.Message == "tool.started" && fields["tool_name"] == "continue_task" {
			t.Fatal("model called continue_task while the existing task was running")
		}
	}
	if !sawPendingLinks || !sawSuccessfulAddition {
		t.Fatalf("real model did not see both pending links and successfully add them: pending_links=%t added=%t", sawPendingLinks, sawSuccessfulAddition)
	}
	if injectedFailureActionID == "" || retryActionID != injectedFailureActionID {
		t.Fatalf("successful add_task_input retry did not explicitly reuse injected failure action_id: failed_action_id=%q retry_action_id=%q", injectedFailureActionID, retryActionID)
	}

	if err := s.FinishStage(ctx, run.ID, StageResult{Summary: "Initial stage checkpoint", AppliedInputIDs: []int64{original.ID}}, ""); err != nil {
		t.Fatal(err)
	}
	var callbackID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM personal_events WHERE kind='task' AND run_id=? ORDER BY id DESC LIMIT 1`, run.ID).Scan(&callbackID); err != nil {
		t.Fatal(err)
	}
	if worked, err := r.turn(ctx); err != nil || !worked {
		t.Fatalf("task callback with pending inputs=%v %v", worked, err)
	}
	var sawCallbackInputs, continuedSuccessfully, continuationFailed bool
	for _, entry := range observed.All() {
		fields := entry.ContextMap()
		if fields["event_id"] != callbackID {
			continue
		}
		if entry.Message == "model.started" {
			payload, _ := json.Marshal(fields["payload"])
			request := string(payload)
			if strings.Contains(request, "pending_task_inputs") && strings.Contains(request, offlineURL) && strings.Contains(request, collaborationURL) {
				sawCallbackInputs = true
			}
		}
		if entry.Message == "tool.finished" && fields["tool_name"] == "continue_task" {
			switch fields["outcome"] {
			case "success":
				continuedSuccessfully = true
			case "business_failure", "failed":
				continuationFailed = true
			}
		}
	}
	if !sawCallbackInputs || !continuedSuccessfully || continuationFailed {
		t.Fatalf("callback did not continue from supplied pending inputs: inputs=%t succeeded=%t failed=%t", sawCallbackInputs, continuedSuccessfully, continuationFailed)
	}
	next, err := s.ClaimRun(ctx, RunTask)
	if err != nil || next == nil || next.TaskID != task.ID {
		t.Fatalf("continuation run=%+v error=%v", next, err)
	}
	var nextInput TaskInput
	if err := json.Unmarshal([]byte(next.Input), &nextInput); err != nil {
		t.Fatal(err)
	}
	if len(nextInput.InputEvents) != 2 || nextInput.InputEvents[0].EventID != supplementID || nextInput.InputEvents[1].EventID != secondSupplementID || !strings.Contains(nextInput.Previous, "Initial stage checkpoint") {
		t.Fatalf("continuation did not inherit both inputs and the saved checkpoint: %+v", nextInput)
	}
	t.Logf("PASS: two late links stay pending during the running stage, then are continued with its checkpoint (%d input attempts)", retrying.inputAttempts)
}

func TestLivePersonalBareLinksReply(t *testing.T) {
	engine := livePersonalEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s := testStore(t, filepath.Join(t.TempDir(), "reading.db"), 2*time.Minute)
	main := &MainAgent{Model: engine.Model, Store: s, Profile: "请用中文回复。", MaxIterations: engine.Config.Agent.MaxIterations, Timeout: 2 * time.Minute}
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
	if strings.Contains(text, "**") || strings.Contains(text, "__") || strings.Contains(text, "](") || regexp.MustCompile(`(?m)^#{1,6} `).MatchString(text) {
		t.Fatalf("chat reply contains Markdown syntax instead of plain text: %s", text)
	}
	if regexp.MustCompile(`(?i)(?:task|run|event|material|任务|事件)\s*(?:_?id|编号|号|#)?\s*[:：]?\s*\d+|\bm_[a-f0-9]{16,}\b|submit_progress|\breported\b`).MatchString(text) {
		t.Fatalf("ordinary reading exposes an internal ID: %s", text)
	}
	if !strings.Contains(text, overview) || !strings.Contains(text, offline) {
		t.Fatalf("reading result omitted supporting source URLs: %s", text)
	}
	t.Logf("PASS: two related links → one silent task → both pages read → evidence-based reply: %s", text)
}
