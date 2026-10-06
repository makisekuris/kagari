package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/reader"
)

// Open creates one isolated browser per analysis. Only navigation and snapshots
// are used; arbitrary evaluation and form-submission tools are not exposed.
func Open(ctx context.Context, cfg config.Config) (func(context.Context, string) (domain.Source, error), func(), error) {
	dir, err := os.MkdirTemp("", "kagari-browser-")
	if err != nil {
		return nil, nil, err
	}
	proxy, closeProxy, err := reader.StartBrowserProxy(ctx, reader.Options{Timeout: cfg.Reader.Timeout, AllowedNonPublicCIDRs: cfg.Reader.AllowedNonPublicCIDRs})
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	cleanup := func() { closeProxy(); _ = os.RemoveAll(dir) }
	settings := map[string]any{"browser": map[string]any{"browserName": "chromium", "isolated": true, "launchOptions": map[string]any{"headless": true, "channel": "chrome", "proxy": map[string]any{"server": proxy}, "args": []string{"--proxy-bypass-list=<-loopback>", "--disable-quic", "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"}}, "contextOptions": map[string]any{"serviceWorkers": "block", "acceptDownloads": false}}, "codegen": "none", "outputDir": dir}
	raw, _ := json.Marshal(settings)
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		cleanup()
		return nil, nil, err
	}
	args := append(append([]string{}, cfg.Browser.Args...), "--config", path)
	command := exec.CommandContext(ctx, cfg.Browser.Command, args...)
	command.Dir = dir
	// The browser subprocess does not need model, Telegram or other application credentials.
	for _, name := range []string{"PATH", "HOME", "USER", "TMPDIR", "DISPLAY", "LANG", "PLAYWRIGHT_BROWSERS_PATH"} {
		if value, ok := os.LookupEnv(name); ok {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "kagari-reader", Version: "1"}, nil)
	connectCtx, cancel := context.WithTimeout(ctx, cfg.Reader.Timeout)
	defer cancel()
	session, err := client.Connect(connectCtx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		cleanup()
		return nil, nil, errors.New("could not connect to Playwright MCP")
	}
	close := func() { _ = session.Close(); cleanup() }
	tools, err := session.ListTools(connectCtx, nil)
	if err != nil {
		close()
		return nil, nil, errors.New("could not list Playwright MCP tools")
	}
	available := map[string]bool{}
	for _, tool := range tools.Tools {
		available[tool.Name] = true
	}
	if !available["browser_navigate"] || !available["browser_snapshot"] {
		close()
		return nil, nil, errors.New("Playwright MCP must provide browser_navigate and browser_snapshot")
	}
	read := func(ctx context.Context, url string) (domain.Source, error) {
		return readPage(ctx, session, url, cfg.Reader)
	}
	return read, close, nil
}

func readPage(ctx context.Context, session *mcp.ClientSession, rawURL string, cfg config.Reader) (domain.Source, error) {
	url, err := reader.NormalizeURL(rawURL)
	if err != nil {
		return domain.Source{}, err
	}
	sum := sha256.Sum256([]byte(url + "\nbrowser"))
	source := domain.Source{ID: "s_" + hex.EncodeToString(sum[:])[:16], RequestedURL: url, URL: url, Kind: "article", ReadMethod: "playwright_mcp", FetchedAt: time.Now().UTC(), Status: "failed"}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	for _, call := range []struct {
		name string
		args map[string]any
	}{{"browser_navigate", map[string]any{"url": url}}, {"browser_snapshot", map[string]any{}}} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil {
			source.Reason = "browser tool unavailable"
			return source, err
		}
		text := resultText(result)
		if result.IsError {
			source.Reason = "browser could not read the page"
			return source, &pageReadError{detail: text}
		}
		if call.name == "browser_snapshot" {
			source.Content = text
		}
		if call.name == "browser_navigate" {
			metadata := strings.SplitN(text, "### Snapshot", 2)[0]
			if strings.Contains(metadata, "### Error") || strings.Contains(metadata, "Error: page.goto:") {
				source.Reason = "browser navigation failed"
				return source, &pageReadError{detail: metadata}
			}
			for _, line := range strings.Split(metadata, "\n") {
				if strings.HasPrefix(line, "- Page URL: ") {
					source.URL = strings.TrimSpace(strings.TrimPrefix(line, "- Page URL: "))
				}
				if strings.HasPrefix(line, "- Page Title: ") {
					source.Title = strings.TrimSpace(strings.TrimPrefix(line, "- Page Title: "))
				}
			}
		}

	}
	finalURL, err := reader.NormalizeURL(source.URL)
	if err != nil {
		source.Reason = "browser returned an invalid page URL"
		source.Content = ""
		return source, errors.New(source.Reason)
	}
	source.URL = finalURL
	if strings.TrimSpace(source.Content) == "" {
		source.Reason = "browser returned no visible page content"
		return source, errors.New(source.Reason)
	}
	base, _ := neturl.Parse(source.URL)
	source.Links = snapshotLinks(source.Content, base, source.URL, cfg.MaxLinks)
	runes := []rune(source.Content)
	if len(runes) > cfg.MaxContentChars {
		source.Content = string(runes[:cfg.MaxContentChars])
		source.Truncated = true
	}
	source.Status = "ok"
	return source, nil
}

func snapshotLinks(content string, base *neturl.URL, pageURL string, limit int) []domain.Link {
	if base == nil || limit <= 0 {
		return nil
	}
	var attributes []string
	var body strings.Builder
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "- /url: "):
			attributes = append(attributes, strings.TrimSpace(strings.TrimPrefix(trimmed, "- /url: ")))
		case strings.HasPrefix(trimmed, "- Page URL: "), strings.HasPrefix(trimmed, "- Page Title: "):
			// MCP metadata is not a link in the page body.
		default:
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}

	links := make([]domain.Link, 0, limit)
	seen := map[string]bool{pageURL: true}
	add := func(raw string, relative bool) {
		if len(links) >= limit {
			return
		}
		target := raw
		if relative {
			reference, err := neturl.Parse(raw)
			if err != nil {
				return
			}
			target = base.ResolveReference(reference).String()
		}
		normalized, err := reader.NormalizeURL(target)
		if err != nil || seen[normalized] {
			return
		}
		seen[normalized] = true
		links = append(links, domain.Link{URL: normalized, Text: "浏览器页面链接"})
	}
	for _, href := range attributes {
		add(href, true)
	}
	for _, target := range reader.ExtractURLs(body.String()) {
		add(target, false)
	}
	return links
}

func resultText(result *mcp.CallToolResult) string {
	var out strings.Builder
	for _, content := range result.Content {
		if value, ok := content.(*mcp.TextContent); ok {
			fmt.Fprintln(&out, value.Text)
		}
	}
	return out.String()
}

// Error() excludes browser diagnostics, which can contain page content or request URLs.
type pageReadError struct{ detail string }

func (*pageReadError) Error() string { return "browser could not read the page" }
