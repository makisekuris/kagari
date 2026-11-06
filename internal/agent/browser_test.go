package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kagari/internal/browser"
	"kagari/internal/domain"
)

func TestBrowserActionsReturnFreshObservationsAndResetPerAnalysis(t *testing.T) {
	const target = "https://example.org/#/page"
	actions := []browser.Request{
		{URL: target},
		{Action: "click", Ref: "e3"},
		{Action: "snapshot"},
		{Action: "click", Ref: "e4"},
		{Action: "snapshot"},
		{URL: target},
		{Action: "snapshot"}, // Rejected before execution once the shared budget is spent.
	}
	observations := []string{"initial excerpt", "stale element", "updated reference e4", "expanded evidence", "fresh expanded snapshot", "reloaded excerpt", "page budget exhausted"}
	var turns atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		call := int(turns.Add(1))
		turn := (call - 1) % (len(actions) + 1)
		if turn > 0 {
			input, _ := json.Marshal(body["input"])
			if !strings.Contains(string(input), observations[turn-1]) {
				t.Errorf("turn %d did not receive fresh tool output %q", turn+1, observations[turn-1])
			}
		}
		if turn == len(actions) {
			writeResponse(w, turn, responseText("Expanded evidence was read."))
			return
		}
		args, _ := json.Marshal(actions[turn])
		id := fmt.Sprintf("browse_%d", call)
		writeResponse(w, turn, []any{map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": "browse_url", "arguments": string(args)}})
	}))
	defer server.Close()
	cfg := engineConfig(t, server.URL)
	cfg.Agent.MaxSources = 6
	e, err := New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened, closed := 0, 0
	e.OpenBrowser = func(context.Context) (browser.Browse, func(), error) {
		opened++
		step := 0
		return func(_ context.Context, request browser.Request) (domain.Source, error) {
			if step >= 6 {
				t.Fatal("budget did not prevent browser action")
			}
			want := actions[step]
			if want.Action == "" {
				want.Action = "navigate"
			}
			if request != want {
				t.Errorf("browser request = %+v, want %+v", request, want)
			}
			url := target
			if step == 3 || step == 4 {
				url = "https://example.org/expanded"
			}
			src := domain.Source{ID: "page", RequestedURL: url, URL: url, Status: "ok", Content: observations[step], ReadMethod: "playwright_mcp"}
			step++
			if step == 2 {
				src.Status, src.Reason, src.Content = "failed", "stale element", ""
				return src, errors.New("stale element")
			}
			return src, nil
		}, func() { closed++ }, nil
	}
	for run := 1; run <= 2; run++ {
		result, err := e.RunTaskStage(context.Background(), domain.Submission{Text: "Expand the article and read its evidence."}, memorySourceRecorder)
		if err != nil || opened != run || closed != run || len(result.Readings) != 6 || len(result.Sources) != 6 {
			t.Fatalf("run %d: opened=%d closed=%d result=%+v err=%v", run, opened, closed, result, err)
		}
		if result.Sources[1].Status != "failed" || result.Sources[4].Content != "fresh expanded snapshot" || result.Readings[3].URL != "https://example.org/expanded" {
			t.Fatalf("browser state/failure history was lost: %+v", result)
		}
	}
}
