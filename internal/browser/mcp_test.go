package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"kagari/internal/config"
)

func TestRequestNormalized(t *testing.T) {
	valid := []Request{
		{URL: " HTTPS://Example.org "},
		{Action: "TYPE", Ref: "e12"},
		{Action: "wait_for", Text: "ready"},
		{Action: "wait_for", Seconds: 30},
		{Action: "wait_for", Text: "ready", Seconds: 3},
		{Action: "click", Ref: "f2e12"},
		{Action: "press_key", Key: "Enter"},
		{Action: "back"},
		{Action: "snapshot"},
	}
	for _, request := range valid {
		if _, err := request.Normalized(); err != nil {
			t.Errorf("Normalized(%+v): %v", request, err)
		}
	}
	got, err := (Request{URL: " HTTPS://Example.org "}).Normalized()
	if err != nil || got.Action != "navigate" || got.URL != "https://example.org/" {
		t.Fatalf("normalized=%+v err=%v", got, err)
	}

	invalid := []Request{
		{Action: "navigate"},
		{URL: "file:///etc/passwd"},
		{Action: "navigate", URL: "https://example.org", Ref: "e1"},
		{Action: "evaluate", URL: "https://example.org"},
		{Action: "snapshot", URL: "https://example.org"},
		{Action: "click", Ref: "button.submit"},
		{Action: "click"},
		{Action: "type"},
		{Action: "type", Ref: "e1", Seconds: 1},
		{Action: "press_key"},
		{Action: "press_key", Key: "Enter", Text: "unexpected"},
		{Action: "wait_for"},
		{Action: "wait_for", Seconds: -1},
		{Action: "wait_for", Seconds: 31},
		{Action: "back", Key: "Enter"},
	}
	for _, request := range invalid {
		if _, err := request.Normalized(); err == nil {
			t.Errorf("Normalized(%+v) accepted invalid request", request)
		}
	}
}

func TestNormalizeURLPreservesHashRoutesAndAnchors(t *testing.T) {
	got, err := NormalizeURL(" HTTPS://Example.org/docs/#/guide?tab=2 ")
	if err != nil || got != "https://example.org/docs/#/guide?tab=2" {
		t.Fatalf("NormalizeURL=%q err=%v", got, err)
	}
	request, err := (Request{URL: "https://example.org/#/start"}).Normalized()
	if err != nil || request.URL != "https://example.org/#/start" {
		t.Fatalf("normalized browser request=%+v err=%v", request, err)
	}
}

func TestBrowsePreservesHashRouteAndAnchorLink(t *testing.T) {
	fixture := &browserFixture{url: "https://example.org/app#/home", title: "SPA", snapshot: "- link \"Detail\" [ref=e0]:\n  - /url: #/detail"}
	browser, close := newFixtureBrowser(t, fixture, config.Reader{Timeout: time.Second, MaxLinks: 5, MaxContentChars: 1000})
	defer close()
	source, err := browser(context.Background(), Request{URL: fixture.url})
	if err != nil || source.URL != fixture.url || len(source.Links) != 1 || source.Links[0].URL != "https://example.org/app#/detail" {
		t.Fatalf("source=%+v err=%v", source, err)
	}
}

func TestBrowseActionsSharePageAndTakeFreshSnapshots(t *testing.T) {
	fixture := &browserFixture{
		url:      "https://example.org/folder/page?x=1#fragment",
		title:    "Landing",
		snapshot: landingSnapshot,
	}
	browser, close := newFixtureBrowser(t, fixture, config.Reader{Timeout: time.Second, MaxLinks: 10, MaxContentChars: 1000})
	defer close()

	source, err := browser(context.Background(), Request{URL: "https://example.org/start"})
	if err != nil || !source.Usable() || source.RequestedURL != "https://example.org/start" || source.URL != "https://example.org/folder/page?x=1#fragment" || source.Title != "Landing" {
		t.Fatalf("navigate source=%+v err=%v", source, err)
	}
	if !strings.Contains(source.Content, "visible evidence") || strings.Contains(source.Content, "### Page") {
		t.Fatalf("unexpected snapshot content: %q", source.Content)
	}
	if len(source.Links) != 4 || source.Links[0].URL != "https://example.org/next" || source.Links[1].URL != "https://cdn.example/asset" {
		t.Fatalf("links=%+v", source.Links)
	}

	_, err = browser(context.Background(), Request{Action: "snapshot"})
	if err != nil || fixture.count("browser_navigate") != 1 {
		t.Fatalf("snapshot navigated again: calls=%v err=%v", fixture.calls, err)
	}
	if err := fixture.expectCalls("browser_navigate", "browser_snapshot", "browser_snapshot"); err != nil {
		t.Fatal(err)
	}

	source, err = browser(context.Background(), Request{Action: "click", Ref: "e0"})
	if err != nil || source.URL != "https://example.org/form" || source.Title != "Form" || !strings.Contains(source.Content, "Search") {
		t.Fatalf("click source=%+v err=%v", source, err)
	}
	source, err = browser(context.Background(), Request{Action: "type", Ref: "e4", Text: "kagari"})
	if err != nil || !strings.Contains(source.Content, "kagari") || fixture.typed != "kagari" {
		t.Fatalf("type source=%+v typed=%q err=%v", source, fixture.typed, err)
	}
	source, err = browser(context.Background(), Request{Action: "press_key", Key: "Enter"})
	if err != nil || fixture.key != "Enter" || !strings.Contains(source.Content, "search results for kagari") {
		t.Fatalf("press_key source=%+v key=%q err=%v", source, fixture.key, err)
	}
	source, err = browser(context.Background(), Request{Action: "wait_for", Text: "search results for kagari"})
	if err != nil || fixture.waited != "search results for kagari" || !strings.Contains(source.Content, fixture.waited) {
		t.Fatalf("wait_for source=%+v waited=%q err=%v", source, fixture.waited, err)
	}
	source, err = browser(context.Background(), Request{Action: "back"})
	if err != nil || source.URL != "https://example.org/folder/page?x=1#fragment" || source.Title != "Landing" {
		t.Fatalf("back source=%+v err=%v", source, err)
	}
	callCount := len(fixture.calls)
	if _, err := browser(context.Background(), Request{Action: "click", Ref: "e4"}); err == nil || len(fixture.calls) != callCount {
		t.Fatalf("stale ref reached MCP: calls=%v err=%v", fixture.calls, err)
	}
	if fixture.count("browser_navigate") != 1 || fixture.count("browser_click") != 1 || fixture.count("browser_type") != 1 || fixture.count("browser_press_key") != 1 || fixture.count("browser_wait_for") != 1 || fixture.count("browser_navigate_back") != 1 {
		t.Fatalf("unexpected action calls: %v", fixture.calls)
	}
}

func TestBrowseRejectsInvalidRequestBeforeMCPCall(t *testing.T) {
	fixture := &browserFixture{}
	browser, close := newFixtureBrowser(t, fixture, config.Reader{Timeout: time.Second, MaxLinks: 5, MaxContentChars: 1000})
	defer close()
	if _, err := browser(context.Background(), Request{Action: "click", Ref: "button.submit"}); err == nil {
		t.Fatal("free selector accepted")
	}
	if len(fixture.calls) != 0 {
		t.Fatalf("invalid request had MCP side effects: %v", fixture.calls)
	}
}

func TestBrowsePreservesActionFailureAndAllowsSnapshotRecovery(t *testing.T) {
	fixture := &browserFixture{url: "https://example.org/start", title: "Home", snapshot: landingSnapshot}
	browser, close := newFixtureBrowser(t, fixture, config.Reader{Timeout: time.Second, MaxLinks: 10, MaxContentChars: 1000})
	defer close()
	if _, err := browser(context.Background(), Request{URL: "https://example.org/home"}); err != nil {
		t.Fatal(err)
	}
	fixture.failClick = true
	fixture.clickIsError = false
	callStart := len(fixture.calls)
	source, err := browser(context.Background(), Request{Action: "click", Ref: "e0"})
	if err == nil || source.Status != "failed" || source.URL != "https://example.org/home" || source.Usable() || strings.Contains(err.Error(), "private page detail") {
		t.Fatalf("failed action source=%+v err=%v", source, err)
	}
	if got := fixture.calls[callStart:]; len(got) != 2 || got[0] != "browser_click" || got[1] != "browser_snapshot" {
		t.Fatalf("failed action was retried or snapshot omitted: %v", got)
	}
	fixture.failClick = false
	source, err = browser(context.Background(), Request{Action: "snapshot"})
	if err != nil || !source.Usable() || !strings.Contains(source.Content, "visible evidence") {
		t.Fatalf("recovery snapshot source=%+v err=%v", source, err)
	}
	if fixture.count("browser_click") != 1 {
		t.Fatalf("mutating action retried: %v", fixture.calls)
	}
}

func TestBrowseChecksMCPErrorFlag(t *testing.T) {
	fixture := &browserFixture{url: "https://example.org/home", title: "Home", snapshot: landingSnapshot, failClick: true, clickIsError: true}
	browser, close := newFixtureBrowser(t, fixture, config.Reader{Timeout: time.Second, MaxLinks: 10, MaxContentChars: 1000})
	defer close()
	if _, err := browser(context.Background(), Request{URL: "https://example.org/home"}); err != nil {
		t.Fatal(err)
	}
	source, err := browser(context.Background(), Request{Action: "click", Ref: "e0"})
	if err == nil || source.Status != "failed" || strings.Contains(err.Error(), "private page detail") {
		t.Fatalf("MCP error flag ignored or leaked detail: source=%+v err=%v", source, err)
	}
}

func TestBrowseSnapshotErrorCanRecover(t *testing.T) {
	fixture := &browserFixture{url: "https://example.org/home", title: "Home", snapshot: landingSnapshot}
	browser, close := newFixtureBrowser(t, fixture, config.Reader{Timeout: time.Second, MaxLinks: 10, MaxContentChars: 1000})
	defer close()
	if _, err := browser(context.Background(), Request{URL: "https://example.org/home"}); err != nil {
		t.Fatal(err)
	}
	fixture.failSnapshot = true
	source, err := browser(context.Background(), Request{Action: "snapshot"})
	if err == nil || source.Status != "failed" || strings.Contains(err.Error(), "private snapshot detail") {
		t.Fatalf("snapshot error ignored or leaked detail: source=%+v err=%v", source, err)
	}
	fixture.failSnapshot = false
	source, err = browser(context.Background(), Request{Action: "snapshot"})
	if err != nil || !source.Usable() {
		t.Fatalf("snapshot recovery source=%+v err=%v", source, err)
	}
}

func TestBrowseRequiresTrustedCurrentPageMetadata(t *testing.T) {
	fixture := &browserFixture{url: "chrome-error://chromewebdata/", title: "Chrome error", snapshot: "- paragraph: error page"}
	browser, close := newFixtureBrowser(t, fixture, config.Reader{Timeout: time.Second, MaxLinks: 5, MaxContentChars: 1000})
	defer close()
	source, err := browser(context.Background(), Request{URL: "https://example.org/internal-error"})
	if err == nil || source.Status != "failed" || source.URL != "" || source.ID != "" {
		t.Fatalf("internal page accepted: source=%+v err=%v", source, err)
	}
	fixture.url = "https://example.org/recovered"
	source, err = browser(context.Background(), Request{Action: "snapshot"})
	if err != nil || !source.Usable() || source.URL != fixture.url {
		t.Fatalf("snapshot did not recover current identity: source=%+v err=%v", source, err)
	}

	fixture.omitMetadata = true
	source, err = browser(context.Background(), Request{Action: "snapshot"})
	if err == nil || source.Status != "failed" || source.URL != "" || source.ID != "" {
		t.Fatalf("missing metadata fell back to old identity: source=%+v err=%v", source, err)
	}
	fixture.omitMetadata, fixture.snapshot = false, ""
	source, err = browser(context.Background(), Request{Action: "snapshot"})
	if err == nil || source.Status != "failed" || source.Usable() {
		t.Fatalf("empty snapshot was accepted: source=%+v err=%v", source, err)
	}
}

func TestBrowseTruncationIsIncomplete(t *testing.T) {
	fixture := &browserFixture{url: "https://example.org/page", title: "Limited", snapshot: "- paragraph: " + strings.Repeat("x", 300) + "\n- link \"First\":\n  - /url: /first\n- link \"Second\":\n  - /url: /second"}
	browser, close := newFixtureBrowser(t, fixture, config.Reader{Timeout: time.Second, MaxLinks: 1, MaxContentChars: 256})
	defer close()
	source, err := browser(context.Background(), Request{URL: "https://example.org/page"})
	if err == nil || source.Status != "incomplete" || !source.Truncated || !source.Usable() || len([]rune(source.Content)) != 256 {
		t.Fatalf("source=%+v err=%v", source, err)
	}
	if !strings.Contains(source.Reason, "content limit") || !strings.Contains(source.Reason, "link list") {
		t.Fatalf("missing truncation reasons: %q", source.Reason)
	}
}

func TestSnapshotLinksKeepSafeLinksAndReportCap(t *testing.T) {
	content := `- link "Relative":
  - /url: /next
- link "Protocol relative":
  - /url: //cdn.example/asset
- link "Unsafe":
  - /url: javascript:alert(1)
- link "Same page":
  - /url: https://example.org/page?x=1#top
- paragraph: https://example.org/fallback and https://other.example/path`
	base, _ := url.Parse("https://example.org/page?x=1#top")
	links, truncated := snapshotLinks(content, base, "https://example.org/page?x=1#top", 3)
	if len(links) != 3 || !truncated || links[0].URL != "https://example.org/next" || links[1].URL != "https://cdn.example/asset" || links[2].URL != "https://example.org/fallback" {
		t.Fatalf("links=%+v truncated=%v", links, truncated)
	}
}

const landingSnapshot = `- button "Open" [ref=e0]
- textbox "Search" [ref=e1]
- link "Next" [ref=e2]:
  - /url: /next
- paragraph: visible evidence
- link "Protocol relative":
  - /url: //cdn.example/asset
- link "Duplicate":
  - /url: https://example.org/folder/page?x=1#fragment
- link "Unsafe":
  - /url: javascript:alert(1)
- paragraph: https://example.org/fallback and https://other.example/path`

type fixturePage struct{ url, title, snapshot string }

type browserFixture struct {
	url          string
	title        string
	snapshot     string
	calls        []string
	history      []fixturePage
	typed        string
	key          string
	waited       string
	failClick    bool
	clickIsError bool
	failSnapshot bool
	omitMetadata bool
}

func (f *browserFixture) record(name string) { f.calls = append(f.calls, name) }

func (f *browserFixture) count(name string) int {
	count := 0
	for _, call := range f.calls {
		if call == name {
			count++
		}
	}
	return count
}

func (f *browserFixture) expectCalls(names ...string) error {
	if strings.Join(f.calls, ",") != strings.Join(names, ",") {
		return fmt.Errorf("calls=%v want=%v", f.calls, names)
	}
	return nil
}

func (f *browserFixture) snapshotResult() *mcp.CallToolResult {
	if f.failSnapshot {
		return textResult("### Error\nprivate snapshot detail", false)
	}
	var out strings.Builder
	if !f.omitMetadata {
		fmt.Fprintf(&out, "### Page\n- Page URL: %s\n- Page Title: %s\n\n", f.url, f.title)
	}
	fmt.Fprintf(&out, "### Snapshot\n```yaml\n%s\n```", f.snapshot)
	return textResult(out.String(), false)
}

func textResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isError}
}

func newFixtureBrowser(t *testing.T, fixture *browserFixture, cfg config.Reader) (Browse, func()) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "controlled-browser"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_navigate"}, func(_ context.Context, _ *mcp.CallToolRequest, input struct {
		URL string `json:"url"`
	}) (*mcp.CallToolResult, any, error) {
		fixture.record("browser_navigate")
		fixture.url = input.URL
		if input.URL == "https://example.org/start" {
			fixture.url, fixture.title, fixture.snapshot = "https://example.org/folder/page?x=1#fragment", "Landing", landingSnapshot
		}
		if strings.Contains(input.URL, "/internal-error") {
			fixture.url, fixture.title = "chrome-error://chromewebdata/", "Chrome error"
		}
		return textResult("navigation complete", false), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "browser_snapshot"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		fixture.record("browser_snapshot")
		return fixture.snapshotResult(), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "browser_click"}, func(_ context.Context, _ *mcp.CallToolRequest, input struct {
		Target string `json:"target"`
	}) (*mcp.CallToolResult, any, error) {
		fixture.record("browser_click")
		if fixture.failClick {
			return textResult("### Error\nprivate page detail", fixture.clickIsError), nil, nil
		}
		if input.Target == "e0" {
			fixture.history = append(fixture.history, fixturePage{fixture.url, fixture.title, fixture.snapshot})
			fixture.url, fixture.title = "https://example.org/form", "Form"
			fixture.snapshot = `- textbox "Search" [ref=e4]
- link "Next" [ref=e5]:
  - /url: /next`
		}
		return textResult("clicked", false), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "browser_type"}, func(_ context.Context, _ *mcp.CallToolRequest, input struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}) (*mcp.CallToolResult, any, error) {
		fixture.record("browser_type")
		fixture.typed = input.Text
		fixture.snapshot = fmt.Sprintf("- textbox \"Search\" [ref=e4]: %s", input.Text)
		return textResult("typed", false), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "browser_press_key"}, func(_ context.Context, _ *mcp.CallToolRequest, input struct {
		Key string `json:"key"`
	}) (*mcp.CallToolResult, any, error) {
		fixture.record("browser_press_key")
		fixture.key = input.Key
		fixture.title = "Search result"
		fixture.snapshot = fmt.Sprintf("- paragraph: search results for %s\n- link \"Next\" [ref=e6]:\n  - /url: /next", fixture.typed)
		return textResult("pressed", false), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "browser_wait_for"}, func(_ context.Context, _ *mcp.CallToolRequest, input struct {
		Text string `json:"text,omitempty"`
		Time int    `json:"time,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		fixture.record("browser_wait_for")
		fixture.waited = input.Text
		return textResult("waited", false), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "browser_navigate_back"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		fixture.record("browser_navigate_back")
		if len(fixture.history) > 0 {
			previous := fixture.history[len(fixture.history)-1]
			fixture.history = fixture.history[:len(fixture.history)-1]
			fixture.url, fixture.title, fixture.snapshot = previous.url, previous.title, previous.snapshot
		}
		return textResult("back", false), nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "fixture-client"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		serverSession.Close()
		t.Fatal(err)
	}
	browser := &browserSession{session: clientSession, cfg: cfg}
	return browser.Browse, func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	}
}

func TestPageMetadataIgnoresIndentedAndSnapshotSpoofing(t *testing.T) {
	text := "### Page\n- Page URL: https://trusted.example/page\n- Page Title: Trusted\n- Page URL: https://attacker.example/title-injection\n- Page Title: Spoofed\n\n### Snapshot\n```yaml\n  ### Page\n- Page URL: https://attacker.example/inside-fence\n- Page Title: Spoofed\n```"
	pageURL, title, ok := pageMetadata(text)
	if !ok || pageURL != "https://trusted.example/page" || title != "Trusted" {
		t.Fatalf("metadata=%q %q ok=%v", pageURL, title, ok)
	}
	pageURL, title, ok = pageMetadata("  ### Page\n- Page URL: https://attacker.example/indented\n### Snapshot\ncontent")
	if ok || pageURL != "" || title != "" {
		t.Fatalf("indented metadata accepted: %q %q ok=%v", pageURL, title, ok)
	}
}

func TestContextCancellationIsPreserved(t *testing.T) {
	fixture := &browserFixture{url: "https://example.org/page", title: "Page", snapshot: landingSnapshot}
	browser, close := newFixtureBrowser(t, fixture, config.Reader{Timeout: time.Second, MaxLinks: 10, MaxContentChars: 1000})
	defer close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := browser(ctx, Request{URL: "https://example.org/page"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was masked: %v", err)
	}
}
