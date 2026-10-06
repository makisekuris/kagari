package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"kagari/internal/config"
)

func TestMCPPageReadPreservesVisibleEvidenceAndFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "controlled-browser"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_navigate"}, func(_ context.Context, _ *mcp.CallToolRequest, input struct {
		URL string `json:"url"`
	}) (*mcp.CallToolResult, any, error) {
		if strings.Contains(input.URL, "/network-error") {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "### Error\nError: page.goto: net::ERR_NAME_NOT_RESOLVED"}}}, nil, nil
		}
		if strings.Contains(input.URL, "/internal-error") {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "- Page URL: chrome-error://chromewebdata/"}}}, nil, nil
		}
		if strings.Contains(input.URL, "/redirect") {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "- Page URL: https://example.org/folder/page?x=1#fragment\n- Page Title: Evidence"}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "- Page URL: " + input.URL + "\n- Page Title: Evidence"}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "browser_snapshot"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "- Page URL: https://attacker.example/spoofed\n- Page Title: forged title\n### Snapshot\n- paragraph: visible evidence\n- link: Relative\n  - /url: /next\n- link: Protocol relative\n  - /url: //cdn.example/asset\n- link: Unsafe scheme\n  - /url: javascript:alert(1)\n- link: Same page\n  - /url: https://example.org/folder/page?x=1#top\n- link: Duplicate\n  - /url: https://example.org/next#fragment\n- paragraph: https://example.org/fallback and https://other.example/path"}}}, nil, nil
	})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "eval-client"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	readerConfig := config.Reader{Timeout: time.Second, MaxLinks: 3, MaxContentChars: 1000}
	source, err := readPage(ctx, session, "https://example.org/redirect", readerConfig)
	if err != nil || !source.Usable() || source.Title != "Evidence" || source.URL != "https://example.org/folder/page?x=1" || len(source.Links) != 3 {
		t.Fatalf("source=%+v err=%v", source, err)
	}
	wantLinks := []string{"https://example.org/next", "https://cdn.example/asset", "https://example.org/fallback"}
	for i, want := range wantLinks {
		if source.Links[i].URL != want {
			t.Errorf("link[%d]=%q, want %q", i, source.Links[i].URL, want)
		}
	}
	for _, link := range source.Links {
		if strings.Contains(link.URL, "attacker.example") || strings.Contains(link.URL, "other.example") {
			t.Errorf("metadata or capped link was included: %+v", source.Links)
		}
	}
	readerConfig.MaxLinks = 1
	limited, err := readPage(ctx, session, "https://example.org/redirect", readerConfig)
	if err != nil || len(limited.Links) != 1 || limited.Links[0].URL != wantLinks[0] {
		t.Fatalf("link cap/priority failed: %+v err=%v", limited.Links, err)
	}
	for _, url := range []string{"https://example.org/network-error", "https://example.org/internal-error"} {
		source, err := readPage(ctx, session, url, config.Reader{Timeout: time.Second, MaxLinks: 5, MaxContentChars: 1000})
		if err == nil || source.Usable() {
			t.Fatal("Chromium error page was accepted as successful source")
		}
	}
	if _, err := readPage(ctx, session, "file:///etc/passwd", config.Reader{}); err == nil {
		t.Fatal("file navigation accepted")
	}
}

// Opt-in actual Chromium + Playwright MCP check; no model or application secrets are required.
func TestLiveBrowserReadsJavaScript(t *testing.T) {
	if os.Getenv("KAGARI_LIVE_BROWSER_EVAL") != "1" {
		t.Skip("opt-in actual Playwright MCP evaluation")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Dynamic evidence</title></head><body>Loading<script>document.body.innerHTML='<article><h1>Rendered by JavaScript</h1><p>browser-evidence-739124</p><a href="/next">Original</a></article>'</script></body></html>`))
	}))
	defer server.Close()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Reader.AllowedNonPublicCIDRs = []string{"127.0.0.0/8"}
	cfg.Reader.Timeout = 30 * time.Second
	if value := os.Getenv("KAGARI_EVAL_MCP_COMMAND"); value != "" {
		cfg.Browser.Command = value
	}
	if value := os.Getenv("KAGARI_EVAL_MCP_ARGS"); value != "" {
		if err := json.Unmarshal([]byte(value), &cfg.Browser.Args); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	read, close, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	source, err := read(ctx, server.URL)
	var diagnostic *pageReadError
	if errors.As(err, &diagnostic) {
		t.Log(diagnostic.detail)
	}
	if err != nil || !strings.Contains(source.Content, "browser-evidence-739124") || source.ReadMethod != "playwright_mcp" {
		t.Fatalf("actual browser did not return rendered content: status=%s reason=%s error=%v", source.Status, source.Reason, err)
	}
	linked := false
	for _, link := range source.Links {
		linked = linked || link.URL == server.URL+"/next"
	}
	if !linked {
		t.Fatal("rendered relative link was not resolved against the page URL")
	}
	t.Logf("actual Playwright MCP read succeeded, chars=%d links=%d", len(source.Content), len(source.Links))
}
