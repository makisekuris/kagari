package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/reader"
)

// Browse performs bounded actions in one isolated browser session.
type Browse func(context.Context, Request) (domain.Source, error)

// Open creates one isolated browser per analysis. Only the bounded actions in
// Request are exposed; arbitrary evaluation, file access and tab management stay disabled.
func Open(ctx context.Context, cfg config.Config) (Browse, func(), error) {
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
	for _, name := range []string{"browser_navigate", "browser_snapshot", "browser_click", "browser_type", "browser_press_key", "browser_wait_for", "browser_navigate_back"} {
		if !available[name] {
			close()
			return nil, nil, errors.New("Playwright MCP is missing a required browser action")
		}
	}
	browser := &browserSession{session: session, cfg: cfg.Reader}
	return browser.Browse, close, nil
}
