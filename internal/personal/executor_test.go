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
	return Run{Kind: RunTask, Input: string(raw)}
}

func analysisResponse(body string) fixtureResponse {
	raw, _ := json.Marshal(map[string]string{"kind": string(domain.ResultKindAnalysis), "body": body})
	return responseText(string(raw))
}

func TestReadingExecutorFramesTaskAndInjectsOnlyActiveBoundedMemory(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{analysisResponse("Stage report.")}}
	repo := newToolRepo()
	repo.memories["answer_style"] = Memory{Key: "answer_style", Value: "Concise answers are preferred.", Version: 2}
	repo.memories["forgotten_fact"] = Memory{Key: "forgotten_fact", Value: "must not be injected", Deleted: true, Version: 4}
	input := TaskInput{
		Goal:     "Compare the two official recovery guides.",
		Context:  "Use official sources and keep the comparison concise.",
		Step:     "Read the first guide and record its recovery steps.",
		Previous: "Earlier evidence: the second guide mentions checkpoints.",
	}
	result, err := ReadingExecutor(readingEngine(fixture, nil), repo)(context.Background(), executorRun(input), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "review" || result.Summary != "Stage report." {
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

func TestReadingExecutorReportsReadsAndClosesBrowser(t *testing.T) {
	t.Run("HTTP", func(t *testing.T) {
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("read_url", map[string]string{"url": "https://example.org/guide"}, "http-read"),
			analysisResponse("Read the guide."),
		}}
		engine := readingEngine(fixture, func(_ context.Context, rawURL string) (domain.Source, error) {
			return domain.Source{ID: "guide", URL: rawURL, Title: "Guide", Status: "ok", Content: "Step one."}, nil
		})
		repo := newToolRepo()
		var progress []string
		result, err := ReadingExecutor(engine, repo)(context.Background(), executorRun(TaskInput{Goal: "Read the guide."}), func(message string) error {
			progress = append(progress, message)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome != "review" || len(result.Sources) != 1 || result.Sources[0].Content != "Step one." {
			t.Fatalf("result=%+v", result)
		}
		if len(progress) != 1 || !strings.Contains(progress[0], "https://example.org/guide") {
			t.Fatalf("progress=%v", progress)
		}
	})

	t.Run("browser cleanup", func(t *testing.T) {
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("browse_url", map[string]string{"url": "https://example.org/rendered"}, "browser-read"),
			analysisResponse("The browser returned a restricted result."),
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
		result, err := ReadingExecutor(engine, newToolRepo())(context.Background(), executorRun(TaskInput{Goal: "Read the rendered page."}), func(message string) error {
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

func TestReadingExecutorCancellationKeepsPartialEvidence(t *testing.T) {
	fixture := &responsesFixture{t: t, responses: []fixtureResponse{
		responseTool("read_url", map[string]string{"url": "https://example.org/first"}, "first-read"),
		responseTool("read_url", map[string]string{"url": "https://example.org/second"}, "second-read"),
		analysisResponse("This response should not be reached."),
	}}
	engine := readingEngine(fixture, func(_ context.Context, rawURL string) (domain.Source, error) {
		return domain.Source{ID: "first", URL: rawURL, Status: "ok", Content: "Actual evidence from the first page."}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var progress []string
	result, err := ReadingExecutor(engine, newToolRepo())(ctx, executorRun(TaskInput{Goal: "Read both pages."}), func(message string) error {
		progress = append(progress, message)
		if strings.Contains(message, "/second") {
			cancel()
		}
		return nil
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context cancellation", err)
	}
	if result.Outcome != "failed" || len(result.Sources) == 0 {
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

func TestReadingExecutorRejectsBadTaskInputAndRetainsSourceEvidenceOnModelError(t *testing.T) {
	t.Run("bad task input", func(t *testing.T) {
		fixture := &responsesFixture{t: t}
		result, err := ReadingExecutor(readingEngine(fixture, nil), newToolRepo())(context.Background(), Run{Kind: RunTask, Input: `{"goal":"x"} {}`}, nil)
		if err == nil || result.Outcome != "failed" || !strings.Contains(err.Error(), "trailing JSON") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("model error", func(t *testing.T) {
		fixture := &responsesFixture{t: t, responses: []fixtureResponse{
			responseTool("read_url", map[string]string{"url": "https://example.org/evidence"}, "partial-read"),
			{status: "incomplete", output: responseText(`{"kind":"analysis","body":"incomplete"}`).output},
		}}
		engine := readingEngine(fixture, func(_ context.Context, rawURL string) (domain.Source, error) {
			return domain.Source{ID: "evidence", URL: rawURL, Status: "ok", Content: "Observed text only."}, nil
		})
		result, err := ReadingExecutor(engine, newToolRepo())(context.Background(), executorRun(TaskInput{Goal: "Read the evidence."}), nil)
		if err == nil || result.Outcome != "failed" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(result.Sources) != 1 || !strings.Contains(result.Summary, "Observed text only.") || strings.Contains(result.Summary, "successfully completed") {
			t.Fatalf("partial evidence was not preserved accurately: %+v", result)
		}
	})
}
