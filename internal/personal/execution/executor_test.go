package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kagari/internal/agent"
	"kagari/internal/browser"
	"kagari/internal/domain"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
	"kagari/internal/personal/sqlite"
	"kagari/internal/textpage"
)

func runTaskExecutor(t *testing.T, ctx context.Context, engine *agent.Engine, store *sqlite.Store, run personal.Run, progress func(string) error) (personal.StageResult, error) {
	t.Helper()
	return TaskExecutor(engine, store, t.TempDir())(ctx, run, progress)
}

func createRunningTask(t *testing.T, input personal.TaskInput) (*sqlite.Store, personal.Run) {
	t.Helper()
	store := testkit.Store(t, ":memory:", time.Minute)
	return store, testkit.CreateRunningTask(t, store, "execution", input)
}

func savedSource(t *testing.T, store *sqlite.Store, runID int64, ref domain.SourceRef) domain.Source {
	t.Helper()
	text, err := store.ReadMaterial(context.Background(), personal.ReadScope{RunID: runID}, ref.ID, "body", textpage.Request{})
	if err != nil {
		t.Fatal(err)
	}
	return domain.Source{ID: text.Material.ID, Content: text.Page.Text, URL: text.Material.Source.URL, Title: text.Material.Source.Title, Status: text.Material.Source.Status}
}

func TestTaskExecutorFramesTaskAndInjectsOnlyReferencedMemory(t *testing.T) {
	ctx := context.Background()
	store := testkit.Store(t, ":memory:", time.Minute)
	event := testkit.OpenUserEvent(t, store, 1, 1, "Remember test preferences")
	if _, err := store.WriteMemory(ctx, event.ID, "answer-style", personal.MemoryChange{Operation: "put", Key: "answer_style", Value: "Concise answers are preferred.", Evidence: event.Text}); err != nil {
		t.Fatal(err)
	}
	unrelated, err := store.WriteMemory(ctx, event.ID, "unrelated", personal.MemoryChange{Operation: "put", Key: "forgotten_fact", Value: "must not be injected", Evidence: event.Text})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteMemory(ctx, event.ID, "forget-unrelated", personal.MemoryChange{Operation: "forget", Key: unrelated.Key, ExpectedVersion: unrelated.Version, Evidence: event.Text}); err != nil {
		t.Fatal(err)
	}
	input := personal.TaskInput{
		Goal: "Compare the two official recovery guides.", Context: "Use official sources and keep the comparison concise.",
		Step: "Read the first guide and record its recovery steps.", Previous: "Earlier evidence: the second guide mentions checkpoints.",
		MemoryRefs: []string{"answer_style"},
	}
	task, _, err := store.CreateTask(ctx, event.ID, "task", "task", input)
	if err != nil {
		t.Fatal(err)
	}
	testkit.FinishEvent(t, store, event, personal.TurnOutput{TaskID: task.ID}, nil)
	run, err := store.ClaimRun(ctx, personal.RunTask)
	if err != nil || run == nil {
		t.Fatalf("ClaimRun() = (%+v, %v)", run, err)
	}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{testkit.ResponseText("Stage report.")}}
	result, err := runTaskExecutor(t, ctx, testkit.ReadingEngine(fixture, nil), store, *run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "Stage report." || len(fixture.Requests) != 1 {
		t.Fatalf("result=%+v requests=%d", result, len(fixture.Requests))
	}
	request := testkit.RequestInput(fixture.Requests[0])
	for _, want := range []string{input.Goal, input.Context, input.Step, input.Previous, "answer_style", "Concise answers are preferred.", "PROFILE_READING"} {
		if !strings.Contains(request, want) {
			t.Fatalf("task frame did not include %q: %s", want, request)
		}
	}
	if strings.Contains(request, "must not be injected") || strings.Contains(request, "forgotten_fact") {
		t.Fatalf("unreferenced memory was injected: %s", request)
	}
}

func TestTaskExecutorReportsReadsAndClosesBrowser(t *testing.T) {
	t.Run("HTTP", func(t *testing.T) {
		store, run := createRunningTask(t, personal.TaskInput{Goal: "Read the guide."})
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("read_url", map[string]string{"url": "https://example.org/guide"}, "http-read"),
			testkit.ResponseText("Read the guide."),
		}}
		engine := testkit.ReadingEngine(fixture, func(_ context.Context, rawURL string) (domain.Source, error) {
			return domain.Source{URL: rawURL, Title: "Guide", Status: "ok", Content: "Step one."}, nil
		})
		var progress []string
		result, err := runTaskExecutor(t, context.Background(), engine, store, run, func(message string) error {
			progress = append(progress, message)
			return nil
		})
		if err != nil || len(result.Sources) != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if source := savedSource(t, store, run.ID, result.Sources[0]); source.Content != "Step one." {
			t.Fatalf("saved source=%+v", source)
		}
		if len(progress) != 1 || !strings.Contains(progress[0], "https://example.org/guide") {
			t.Fatalf("progress=%v", progress)
		}
	})

	t.Run("browser cleanup", func(t *testing.T) {
		store, run := createRunningTask(t, personal.TaskInput{Goal: "Read the rendered page."})
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("browse_url", map[string]string{"url": "https://example.org/rendered"}, "browser-read"),
			testkit.ResponseText("The browser returned a restricted result."),
		}}
		engine := testkit.ReadingEngine(fixture, nil)
		closed := 0
		readErr := errors.New("browser returned partial content")
		engine.OpenBrowser = func(context.Context) (browser.Browse, func(), error) {
			return func(_ context.Context, request browser.Request) (domain.Source, error) {
				return domain.Source{URL: request.URL, Title: "Rendered page", Status: "restricted", Content: "visible heading"}, readErr
			}, func() { closed++ }, nil
		}
		var progress []string
		result, err := runTaskExecutor(t, context.Background(), engine, store, run, func(message string) error {
			progress = append(progress, message)
			return nil
		})
		if err != nil || closed != 1 || len(result.Sources) != 1 {
			t.Fatalf("result=%+v closed=%d err=%v", result, closed, err)
		}
		if len(progress) != 1 || !strings.Contains(progress[0], "https://example.org/rendered") {
			t.Fatalf("progress=%v", progress)
		}
		source, err := store.ExpandSource(context.Background(), result.Sources[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if source.Status != "restricted" || source.Content != "visible heading" || source.Reason != "operation failed" {
			t.Fatalf("partial browser evidence lost: %+v", source)
		}
	})
}

func TestTaskExecutorPassesBrowserRequestAndHidesTypedTextFromProgress(t *testing.T) {
	store, run := createRunningTask(t, personal.TaskInput{Goal: "Search for the guide."})
	request := browser.Request{Action: "type", Ref: "e8", Text: "private search terms"}
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("browse_url", map[string]any{"action": request.Action, "ref": request.Ref, "text": request.Text}, "browser-type"),
		testkit.ResponseText("Entered the search terms."),
	}}
	engine := testkit.ReadingEngine(fixture, nil)
	var received browser.Request
	engine.OpenBrowser = func(context.Context) (browser.Browse, func(), error) {
		return func(_ context.Context, got browser.Request) (domain.Source, error) {
			received = got
			return domain.Source{URL: "https://example.org/search", Status: "ok", Content: "Search page"}, nil
		}, func() {}, nil
	}
	var progress []string
	result, err := runTaskExecutor(t, context.Background(), engine, store, run, func(message string) error {
		progress = append(progress, message)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.Action != request.Action || received.Ref != request.Ref || received.Text != request.Text {
		t.Fatalf("browser request=%+v, want %+v", received, request)
	}
	if len(result.Sources) != 1 || savedSource(t, store, run.ID, result.Sources[0]).Content != "Search page" {
		t.Fatalf("result=%+v", result)
	}
	if len(progress) != 1 || !strings.Contains(progress[0], "type") || strings.Contains(progress[0], request.Text) {
		t.Fatalf("browser progress exposed typed text or omitted action: %v", progress)
	}
}

func TestTaskExecutorReturnsBrowserProgressFailure(t *testing.T) {
	store, run := createRunningTask(t, personal.TaskInput{Goal: "Read the rendered page."})
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("browse_url", map[string]string{"url": "https://example.org/rendered"}, "browser-read"),
		testkit.ResponseText("The progress update failed."),
	}}
	engine := testkit.ReadingEngine(fixture, nil)
	called := false
	engine.OpenBrowser = func(context.Context) (browser.Browse, func(), error) {
		return func(context.Context, browser.Request) (domain.Source, error) {
			called = true
			return domain.Source{URL: "https://example.org/rendered", Status: "ok", Content: "page"}, nil
		}, func() {}, nil
	}
	progressErr := errors.New("progress callback failed")
	result, err := runTaskExecutor(t, context.Background(), engine, store, run, func(string) error { return progressErr })
	if !errors.Is(err, progressErr) || called {
		t.Fatalf("result=%+v err=%v browser_called=%v", result, err, called)
	}
}

func TestTaskExecutorCancellationKeepsPartialEvidence(t *testing.T) {
	store, run := createRunningTask(t, personal.TaskInput{Goal: "Read both pages."})
	fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
		testkit.ResponseTool("read_url", map[string]string{"url": "https://example.org/first"}, "first-read"),
		testkit.ResponseTool("read_url", map[string]string{"url": "https://example.org/second"}, "second-read"),
		testkit.ResponseText("This response should not be reached."),
	}}
	engine := testkit.ReadingEngine(fixture, func(_ context.Context, rawURL string) (domain.Source, error) {
		return domain.Source{URL: rawURL, Status: "ok", Content: "Actual evidence from the first page."}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var progress []string
	result, err := runTaskExecutor(t, ctx, engine, store, run, func(message string) error {
		progress = append(progress, message)
		if strings.Contains(message, "/second") {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || len(result.Sources) == 0 {
		t.Fatalf("partial result=%+v error=%v", result, err)
	}
	joinedSources := ""
	for _, ref := range result.Sources {
		joinedSources += savedSource(t, store, run.ID, ref).Content + " " + ref.ID
	}
	if !strings.Contains(joinedSources, "Actual evidence from the first page.") || !strings.Contains(result.Summary, "Actual evidence from the first page.") || !strings.Contains(result.Summary, "did not complete") || len(progress) != 2 {
		t.Fatalf("partial result was incomplete: result=%+v progress=%v", result, progress)
	}
}

func TestReadingExecutorRejectsTrailingTaskInputAndRetainsEvidenceOnModelError(t *testing.T) {
	t.Run("bad task input", func(t *testing.T) {
		store := testkit.Store(t, ":memory:", time.Minute)
		fixture := &testkit.ResponsesFixture{T: t}
		run := personal.Run{ID: 1, TaskID: 1, Revision: 1, Kind: personal.RunTask, Input: `{"goal":"x"} {}`}
		result, err := readingExecutor(testkit.ReadingEngine(fixture, nil), store)(context.Background(), run, nil)
		if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("model error", func(t *testing.T) {
		store, run := createRunningTask(t, personal.TaskInput{Goal: "Read the evidence."})
		fixture := &testkit.ResponsesFixture{T: t, Responses: []testkit.Response{
			testkit.ResponseTool("read_url", map[string]string{"url": "https://example.org/evidence"}, "partial-read"),
			{Status: "incomplete", Output: testkit.ResponseText("incomplete task-stage output").Output},
		}}
		engine := testkit.ReadingEngine(fixture, func(_ context.Context, rawURL string) (domain.Source, error) {
			return domain.Source{URL: rawURL, Status: "ok", Content: "Observed text only."}, nil
		})
		result, err := runTaskExecutor(t, context.Background(), engine, store, run, nil)
		if err == nil {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(result.Sources) != 1 || !strings.Contains(result.Summary, "Observed text only.") || strings.Contains(result.Summary, "successfully completed") {
			t.Fatalf("partial evidence was not preserved accurately: %+v", result)
		}
		if got := savedSource(t, store, run.ID, result.Sources[0]).Content; got != "Observed text only." {
			t.Fatalf("saved evidence=%q", got)
		}
	})
}

func TestTaskInputDecoderRejectsUnknownFields(t *testing.T) {
	var runInput map[string]any
	if err := json.Unmarshal([]byte(`{"goal":"ok","unexpected":true}`), &runInput); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(runInput)
	if _, err := personal.DecodeTaskInput(string(encoded)); err == nil {
		t.Fatal("unknown task input field was accepted")
	}
}
