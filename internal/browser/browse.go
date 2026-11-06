package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"kagari/internal/config"
	"kagari/internal/domain"
)

type browserSession struct {
	session    *mcp.ClientSession
	cfg        config.Reader
	navigated  bool
	currentURL string
	refs       map[string]bool
}

func (b *browserSession) Browse(ctx context.Context, request Request) (domain.Source, error) {
	request, err := request.Normalized()
	if err != nil {
		return failedSource("", err.Error()), err
	}

	requestedURL := request.URL
	if request.Action != "navigate" {
		requestedURL = b.currentURL
		if !b.navigated {
			return failedSource(requestedURL, "browser must navigate before this action"), errors.New("browser must navigate before this action")
		}
		if (request.Action == "click" || request.Action == "type") && !b.refs[request.Ref] {
			return failedSource(requestedURL, "Ref must come from the latest browser snapshot"), errors.New("Ref must come from the latest browser snapshot")
		}
	}
	source := failedSource(requestedURL, "browser could not read the page")
	callCtx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	var actionErr, snapshotErr error
	var snapshot *mcp.CallToolResult
	if request.Action == "snapshot" {
		snapshot, snapshotErr = b.call(callCtx, "browser_snapshot", map[string]any{})
		actionErr = snapshotErr
	} else {
		name, args := actionCall(request)
		if request.Action == "navigate" {
			b.currentURL = ""
			b.refs = nil
		}
		_, actionErr = b.call(callCtx, name, args)
		if request.Action == "navigate" && actionErr == nil {
			b.navigated = true
		}
		snapshot, snapshotErr = b.call(callCtx, "browser_snapshot", map[string]any{})
	}

	if snapshot == nil || snapshotErr != nil || snapshot.IsError || hasMCPSection(resultText(snapshot), "Error") {
		b.currentURL, b.refs = "", nil
		source.ID = ""
		if actionErr == nil {
			actionErr = snapshotErr
		}
		if actionErr == nil {
			actionErr = errors.New("browser snapshot failed")
		}
		source.Reason = actionErr.Error()
		return source, actionErr
	}

	snapshotText := resultText(snapshot)
	pageURL, title, hasURL := pageMetadata(snapshotText)
	finalURL, urlErr := NormalizeURL(pageURL)
	if !hasURL || urlErr != nil {
		b.currentURL, b.refs = "", nil
		source.ID = ""
		if actionErr != nil {
			source.Reason = "browser action failed"
			return source, actionErr
		}
		if hasURL {
			source.Reason = "browser returned an invalid page URL"
		} else {
			source.Reason = "browser returned no page metadata"
		}
		return source, errors.New(source.Reason)
	}
	b.currentURL = finalURL
	b.refs = snapshotRefs(snapshotText)
	b.navigated = true
	source.URL, source.Title = finalURL, title
	source.ID = sourceID(finalURL)
	if actionErr != nil {
		source.Reason = "browser action failed"
		return source, actionErr
	}
	content, hasSnapshot := mcpSection(snapshotText, "Snapshot")
	source.Content = stripCodeFence(content)
	if !hasSnapshot || source.Content == "" {
		source.Reason = "browser returned no visible page content"
		return source, errors.New(source.Reason)
	}
	base, _ := url.Parse(finalURL)
	var linksTruncated, contentTruncated bool
	source.Links, linksTruncated = snapshotLinks(source.Content, base, finalURL, b.cfg.MaxLinks)
	var reasons []string
	runes := []rune(source.Content)
	if len(runes) > b.cfg.MaxContentChars {
		source.Content = string(runes[:b.cfg.MaxContentChars])
		contentTruncated = true
		reasons = append(reasons, "browser snapshot exceeded the configured content limit")
	}
	if linksTruncated {
		reasons = append(reasons, "browser link list exceeded the configured limit")
	}
	if linksTruncated || contentTruncated {
		source.Truncated = true
		source.Status = "incomplete"
		source.Reason = strings.Join(reasons, "; ")
		return source, errors.New(source.Reason)
	}
	source.Status = "ok"
	return source, nil
}

func actionCall(request Request) (string, map[string]any) {
	switch request.Action {
	case "navigate":
		return "browser_navigate", map[string]any{"url": request.URL}
	case "click":
		return "browser_click", map[string]any{"target": request.Ref}
	case "type":
		return "browser_type", map[string]any{"target": request.Ref, "text": request.Text}
	case "press_key":
		return "browser_press_key", map[string]any{"key": request.Key}
	case "wait_for":
		args := map[string]any{}
		if strings.TrimSpace(request.Text) != "" {
			args["text"] = request.Text
		}
		if request.Seconds > 0 {
			args["time"] = request.Seconds
		}
		return "browser_wait_for", args
	case "back":
		return "browser_navigate_back", map[string]any{}
	default:
		panic("Request.Normalized must validate action before dispatch")
	}
}

func (b *browserSession) call(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	result, err := b.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	reason := "browser action failed"
	if name == "browser_snapshot" {
		reason = "browser snapshot failed"
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		return result, &pageReadError{reason: reason, detail: err.Error()}
	}
	if result == nil || result.IsError || hasMCPSection(resultText(result), "Error") {
		return result, &pageReadError{reason: reason, detail: resultText(result)}
	}
	return result, nil
}

// Error excludes MCP diagnostics, which may contain page content or request URLs.
type pageReadError struct{ reason, detail string }

func (e *pageReadError) Error() string { return e.reason }

func failedSource(requestedURL, reason string) domain.Source {
	return domain.Source{ID: sourceID(requestedURL), RequestedURL: requestedURL, Kind: "article", ReadMethod: "playwright_mcp", FetchedAt: time.Now().UTC(), Status: "failed", Reason: reason}
}

func sourceID(pageURL string) string {
	sum := sha256.Sum256([]byte(pageURL + "\nbrowser"))
	return "s_" + hex.EncodeToString(sum[:])[:16]
}
