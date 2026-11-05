package personal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/domain"
)

func readingEngine(fixture *responsesFixture, read func(context.Context, string) (domain.Source, error)) *agent.Engine {
	cfg := config.Config{}
	cfg.Agent.MaxSources = 8
	cfg.Agent.MaxIterations = 6
	cfg.Agent.Timeout = 4 * time.Second
	return &agent.Engine{Model: fixture.model(fixture.t), Config: cfg, Profile: "PROFILE_READING", Read: read}
}

func executorRun(input TaskInput) Run {
	raw, _ := json.Marshal(input)
	return Run{ID: 190, TaskID: 90, Revision: 1, Kind: RunTask, Status: RunRunning, Input: string(raw)}
}

func runTaskExecutor(t *testing.T, ctx context.Context, engine *agent.Engine, repo *toolRepo, run Run, progress func(string) error) (StageResult, error) {
	t.Helper()
	repo.tasks[run.TaskID] = Task{ID: run.TaskID, Revision: run.Revision, Status: TaskRunning}
	repo.runs[run.ID] = run
	return TaskExecutor(engine, repo, t.TempDir())(ctx, run, progress)
}

func createRunningTask(t *testing.T, store *Store, key string, input TaskInput) Run {
	t.Helper()
	ctx := context.Background()
	event := openUserEvent(t, store, 1, 1, "Run task stage")
	_, _, err := store.CreateTask(ctx, event.ID, key, key, input)
	if err != nil {
		t.Fatal(err)
	}
	finishEvent(t, store, event, TurnOutput{}, nil)
	run, err := store.ClaimRun(ctx, RunTask)
	if err != nil || run == nil {
		t.Fatalf("ClaimRun() = (%+v, %v)", run, err)
	}
	return *run
}

func TestTaskExecutorFramesTaskAndInjectsOnlyActiveBoundedMemory(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{responseText("Stage report.")}}
	repo := newToolRepo()
	repo.memories["answer_style"] = Memory{Key: "answer_style", Value: "Concise answers are preferred.", Version: 2}
	repo.memories["forgotten_fact"] = Memory{Key: "forgotten_fact", Value: "must not be injected", Deleted: true, Version: 4}
	input := TaskInput{
		Goal:     "Compare the two official recovery guides.",
		Context:  "Use official sources and keep the comparison concise.",
		Step:     "Read the first guide and record its recovery steps.",
		Previous: "Earlier evidence: the second guide mentions checkpoints.",
	}
	result, err := runTaskExecutor(t, context.Background(), readingEngine(fixture, nil), repo, executorRun(input), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "Stage report." {
		t.Fatalf("stage result=%+v", result)
	}
	if len(fixture.requests) != 1 {
		t.Fatalf("Responses requests=%d, want 1", len(fixture.requests))
	}
	request := requestInput(fixture.requests[0])
	for _, want := range []string{input.Goal, input.Context, input.Step, input.Previous, "answer_style", "Concise answers are preferred.", "PROFILE_READING", "background data"} {
		if !strings.Contains(request, want) {
			t.Fatalf("task frame did not include %q: %s", want, request)
		}
	}
	if strings.Contains(request, "must not be injected") || strings.Contains(request, "forgotten_fact") {
		t.Fatalf("deleted memory was injected: %s", request)
	}
}

func TestTaskExecutorReportsReadsAndClosesBrowser(t *testing.T) {
	t.Run("HTTP", func(t *testing.T) {
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("read_url", map[string]string{"url": "https://example.org/guide"}, "http-read"),
			responseText("Read the guide."),
		}}
		engine := readingEngine(fixture, func(_ context.Context, rawURL string) (domain.Source, error) {
			return domain.Source{ID: "guide", URL: rawURL, Title: "Guide", Status: "ok", Content: "Step one."}, nil
		})
		repo := newToolRepo()
		var progress []string
		result, err := runTaskExecutor(t, context.Background(), engine, repo, executorRun(TaskInput{Goal: "Read the guide."}), func(message string) error {
			progress = append(progress, message)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Sources) != 1 || result.Sources[0].Content != "Step one." {
			t.Fatalf("result=%+v", result)
		}
		if len(progress) != 1 || !strings.Contains(progress[0], "https://example.org/guide") {
			t.Fatalf("progress=%v", progress)
		}
	})

	t.Run("browser cleanup", func(t *testing.T) {
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("browse_url", map[string]string{"url": "https://example.org/rendered"}, "browser-read"),
			responseText("The browser returned a restricted result."),
		}}
		engine := readingEngine(fixture, nil)
		closed := 0
		readErr := errors.New("browser returned partial content")
		engine.OpenBrowser = func(context.Context) (func(context.Context, string) (domain.Source, error), func(), error) {
			return func(_ context.Context, rawURL string) (domain.Source, error) {
				return domain.Source{ID: "partial", URL: rawURL, Title: "Rendered page", Status: "restricted", Content: "visible heading"}, readErr
			}, func() { closed++ }, nil
		}
		var progress []string
		repo := newToolRepo()
		result, err := runTaskExecutor(t, context.Background(), engine, repo, executorRun(TaskInput{Goal: "Read the rendered page."}), func(message string) error {
			progress = append(progress, message)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if closed != 1 {
			t.Fatalf("browser closed %d times", closed)
		}
		if len(progress) != 1 || !strings.Contains(progress[0], "https://example.org/rendered") {
			t.Fatalf("browser progress=%v", progress)
		}
		if len(result.Sources) != 1 || result.Sources[0].Status != "restricted" || result.Sources[0].Content != "visible heading" || result.Sources[0].Reason != "operation failed" {
			t.Fatalf("partial browser evidence lost: %+v", result.Sources)
		}
	})
}

func TestTaskExecutorCancellationKeepsPartialEvidence(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("read_url", map[string]string{"url": "https://example.org/first"}, "first-read"),
		responseTool("read_url", map[string]string{"url": "https://example.org/second"}, "second-read"),
		responseText("This response should not be reached."),
	}}
	engine := readingEngine(fixture, func(_ context.Context, rawURL string) (domain.Source, error) {
		return domain.Source{ID: "first", URL: rawURL, Status: "ok", Content: "Actual evidence from the first page."}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var progress []string
	repo := newToolRepo()
	result, err := runTaskExecutor(t, ctx, engine, repo, executorRun(TaskInput{Goal: "Read both pages."}), func(message string) error {
		progress = append(progress, message)
		if strings.Contains(message, "/second") {
			cancel()
		}
		return nil
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context cancellation", err)
	}
	if len(result.Sources) == 0 {
		t.Fatalf("partial result=%+v", result)
	}
	joinedSources := ""
	for _, source := range result.Sources {
		joinedSources += source.Content + " " + source.Reason
	}
	if !strings.Contains(joinedSources, "Actual evidence from the first page.") {
		t.Fatalf("first page evidence was lost: %+v", result.Sources)
	}
	if !strings.Contains(result.Summary, "Actual evidence from the first page.") || !strings.Contains(result.Summary, "did not complete") {
		t.Fatalf("partial result summary=%q", result.Summary)
	}
	if len(progress) != 2 {
		t.Fatalf("progress calls=%v", progress)
	}
}

func TestTaskExecutorRejectsBadTaskInputAndRetainsSourceEvidenceOnModelError(t *testing.T) {
	t.Run("bad task input", func(t *testing.T) {
		fixture := &responsesFixture{t: t}
		repo := newToolRepo()
		run := Run{ID: 190, TaskID: 90, Revision: 1, Kind: RunTask, Status: RunRunning, Input: `{"goal":"x"} {}`}
		result, err := runTaskExecutor(t, context.Background(), readingEngine(fixture, nil), repo, run, nil)
		if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("model error", func(t *testing.T) {
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("read_url", map[string]string{"url": "https://example.org/evidence"}, "partial-read"),
			{status: "incomplete", output: responseText("incomplete task-stage output").output},
		}}
		engine := readingEngine(fixture, func(_ context.Context, rawURL string) (domain.Source, error) {
			return domain.Source{ID: "evidence", URL: rawURL, Status: "ok", Content: "Observed text only."}, nil
		})
		repo := newToolRepo()
		result, err := runTaskExecutor(t, context.Background(), engine, repo, executorRun(TaskInput{Goal: "Read the evidence."}), nil)
		if err == nil {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(result.Sources) != 1 || !strings.Contains(result.Summary, "Observed text only.") || strings.Contains(result.Summary, "successfully completed") {
			t.Fatalf("partial evidence was not preserved accurately: %+v", result)
		}
	})
}
