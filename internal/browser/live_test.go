package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"kagari/internal/config"
	"kagari/internal/domain"
)

// Opt-in actual Chrome + Playwright MCP check against a local site; no model is used.
func TestLiveBrowserInteractiveBrowsing(t *testing.T) {
	if os.Getenv("KAGARI_LIVE_BROWSER_EVAL") != "1" {
		t.Skip("opt-in actual Playwright MCP evaluation")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/next" {
			fmt.Fprint(w, `<html><title>Next article</title><body><h1>Next article</h1><p>linked-evidence-9812</p></body></html>`)
			return
		}
		fmt.Fprint(w, `<!doctype html><html><head><title>Interactive article</title></head><body>
<h1>Interactive article</h1>
<button id="expand">Expand article</button><p id="details" hidden>expanded-evidence-739124</p>
<form id="search"><label for="query">Search docs</label><input id="query"><button>Search</button></form>
<p id="result"></p><a href="/next">Next article</a>
<script>
document.querySelector('#expand').addEventListener('click', () => { document.querySelector('#details').hidden = false; });
document.querySelector('#search').addEventListener('submit', event => {
  event.preventDefault();
  setTimeout(() => { document.querySelector('#result').textContent = 'Search result: ' + document.querySelector('#query').value; }, 50);
});
</script></body></html>`)
	}))
	defer server.Close()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Reader.AllowedNonPublicCIDRs = []string{"127.0.0.0/8"}
	cfg.Reader.Timeout = 20 * time.Second
	cfg.Reader.MaxContentChars, cfg.Reader.MaxLinks = 20000, 40
	if value := os.Getenv("KAGARI_EVAL_MCP_COMMAND"); value != "" {
		cfg.Browser.Command = value
	}
	if value := os.Getenv("KAGARI_EVAL_MCP_ARGS"); value != "" {
		if err := json.Unmarshal([]byte(value), &cfg.Browser.Args); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	browse, closeBrowser, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBrowser()
	act := func(request Request) domain.Source {
		t.Helper()
		source, err := browse(ctx, request)
		if err != nil || !source.Usable() {
			var diagnostic *pageReadError
			if errors.As(err, &diagnostic) {
				t.Log(diagnostic.detail)
			}
			t.Fatalf("action=%s status=%s reason=%s error=%v", request.Action, source.Status, source.Reason, err)
		}
		return source
	}
	refPattern := regexp.MustCompile(`\[ref=((?:f\d+)?e\d+)\]`)
	refFor := func(source domain.Source, label string) string {
		t.Helper()
		for _, line := range strings.Split(source.Content, "\n") {
			if strings.Contains(line, label) {
				if match := refPattern.FindStringSubmatch(line); len(match) == 2 {
					return match[1]
				}
			}
		}
		t.Fatalf("no ref for %q in snapshot: %s", label, source.Content)
		return ""
	}
	page := act(Request{URL: server.URL + "/start"})
	if strings.Contains(page.Content, "expanded-evidence-739124") {
		t.Fatal("hidden article text was visible before expansion")
	}
	page = act(Request{Action: "click", Ref: refFor(page, `button "Expand article"`)})
	if !strings.Contains(page.Content, "expanded-evidence-739124") {
		t.Fatal("click did not return expanded article text")
	}
	act(Request{Action: "type", Ref: refFor(page, `textbox "Search docs"`), Text: "fresh query"})
	act(Request{Action: "press_key", Key: "Enter"})
	page = act(Request{Action: "wait_for", Text: "Search result: fresh query"})
	if !strings.Contains(page.Content, "Search result: fresh query") {
		t.Fatal("search result was not included in the fresh snapshot")
	}
	page = act(Request{Action: "click", Ref: refFor(page, `link "Next article"`)})
	if page.URL != server.URL+"/next" || page.Title != "Next article" || !strings.Contains(page.Content, "linked-evidence-9812") {
		t.Fatalf("link navigation retained old page identity: %+v", page)
	}
	page = act(Request{Action: "back"})
	if page.URL != server.URL+"/start" || page.Title != "Interactive article" {
		t.Fatalf("back retained wrong page identity: %+v", page)
	}
	page = act(Request{Action: "snapshot"})
	if page.URL != server.URL+"/start" || !strings.Contains(page.Content, "Search docs") {
		t.Fatalf("current-page snapshot failed: %+v", page)
	}
	t.Log("actual Chrome verified navigate, expand, type, Enter, wait, link navigation, back, and snapshot")
}
