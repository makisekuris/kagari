package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"

	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/inputbudget"
	"kagari/internal/personal"
	"kagari/internal/personal/internal/testkit"
)

type acceptanceManifestRepository struct {
	Repository
	saveErr   error
	saveCalls int
}

func TestInitialContextManifestIsSavedBeforeModelAndSaveFailureStopsCall(t *testing.T) {
	t.Run("persisted before first model request", func(t *testing.T) {
		ctx := context.Background()
		store := testkit.Store(t, ":memory:", time.Minute)
		run := testkit.CreateRunningTask(t, store, "manifest", personal.TaskInput{Goal: "Check manifest ordering"})
		var mu sync.Mutex
		calls := 0
		manifestAtCall := ""
		modelErr := error(nil)
		engine := acceptanceManifestEngine(t, func() {
			mu.Lock()
			defer mu.Unlock()
			calls++
			stored, err := store.Run(ctx, run.ID)
			if err != nil {
				modelErr = err
				return
			}
			manifestAtCall = stored.ContextManifest
		})
		repo := &acceptanceManifestRepository{Repository: store}
		if _, err := readingExecutor(engine, repo)(ctx, run, nil); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		var manifest personal.ContextManifest
		if calls != 1 || modelErr != nil || manifestAtCall == "" || json.Unmarshal([]byte(manifestAtCall), &manifest) != nil || manifest.FormatVersion != 1 || manifest.Revision != run.Revision {
			t.Fatalf("model calls=%d read error=%v manifest at request=%q parsed=%+v", calls, modelErr, manifestAtCall, manifest)
		}
	})

	t.Run("save failure prevents model request", func(t *testing.T) {
		ctx := context.Background()
		store := testkit.Store(t, ":memory:", time.Minute)
		run := testkit.CreateRunningTask(t, store, "manifest-fail", personal.TaskInput{Goal: "Do not call model without a manifest"})
		var mu sync.Mutex
		modelCalls := 0
		engine := acceptanceManifestEngine(t, func() {
			mu.Lock()
			modelCalls++
			mu.Unlock()
		})
		saveErr := errors.New("manifest storage failed")
		repo := &acceptanceManifestRepository{Repository: store, saveErr: saveErr}
		if _, err := readingExecutor(engine, repo)(ctx, run, nil); err == nil || !strings.Contains(err.Error(), saveErr.Error()) {
			t.Fatalf("execution error=%v, want %q", err, saveErr)
		}
		mu.Lock()
		defer mu.Unlock()
		if modelCalls != 0 || repo.saveCalls != 1 {
			t.Fatalf("model calls=%d manifest saves=%d", modelCalls, repo.saveCalls)
		}
	})
}

func (r *acceptanceManifestRepository) SaveContextManifest(ctx context.Context, runID int64, manifest personal.ContextManifest) error {
	r.saveCalls++
	if r.saveErr != nil {
		return r.saveErr
	}
	return r.Repository.SaveContextManifest(ctx, runID, manifest)
}

func acceptanceManifestEngine(t *testing.T, onRequest func()) *agent.Engine {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if onRequest != nil {
			onRequest()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "manifest-response", "object": "response", "status": "completed", "model": "fixture",
			"output": []any{map[string]any{
				"type": "message", "id": "manifest-message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "Stage report."}},
			}},
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
		})
	}))
	t.Cleanup(server.Close)
	timeout := 4 * time.Second
	maxRetries, store, parallelCalls := 0, false, false
	rawModel, err := agenticopenai.NewResponsesModel(context.Background(), &agenticopenai.ResponsesConfig{
		BaseURL: server.URL + "/v1", APIKey: "test-key", Model: "manifest-fixture",
		Timeout: &timeout, MaxRetries: &maxRetries, Store: &store, ParallelToolCalls: &parallelCalls,
	})
	if err != nil {
		t.Fatal(err)
	}
	limits, err := inputbudget.NewLimits(262144, 5000)
	if err != nil {
		t.Fatal(err)
	}
	model, err := inputbudget.NewAgenticModel(rawModel, inputbudget.ModelConfig{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Agent.MaxSources = 8
	cfg.Agent.MaxIterations = 3
	cfg.Agent.Timeout = 4 * time.Second
	return &agent.Engine{Model: model, Config: cfg, Profile: "acceptance-test"}
}
