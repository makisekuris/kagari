package browser

import (
	"fmt"
	neturl "net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"kagari/internal/domain"
	"kagari/internal/reader"
)

func pageMetadata(text string) (pageURL, title string, hasURL bool) {
	var page strings.Builder
	inPage, inFence, hasTitle := false, false, false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		if !inFence && line == "### Snapshot" {
			break
		}
		if !inFence && strings.HasPrefix(line, "### ") {
			if inPage {
				break
			}
			inPage = line == "### Page"
			continue
		}
		if inPage {
			page.WriteString(line)
			page.WriteByte('\n')
		}
	}
	if !inPage && page.Len() == 0 {
		return "", "", false
	}
	for _, line := range strings.Split(page.String(), "\n") {
		if !hasURL && strings.HasPrefix(line, "- Page URL: ") {
			pageURL, hasURL = strings.TrimSpace(strings.TrimPrefix(line, "- Page URL: ")), true
		}
		if !hasTitle && strings.HasPrefix(line, "- Page Title: ") {
			title = strings.TrimSpace(strings.TrimPrefix(line, "- Page Title: "))
			hasTitle = true
		}
	}
	return pageURL, title, hasURL
}

func mcpSection(text, name string) (string, bool) {
	lines := strings.Split(text, "\n")
	var body []string
	started, inFence := false, false
	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "### ") {
			if started {
				return strings.Join(body, "\n"), true
			}
			started = line == "### "+name
			continue
		}
		if started {
			body = append(body, line)
		}
	}
	return strings.Join(body, "\n"), started
}

func hasMCPSection(text, name string) bool {
	_, ok := mcpSection(text, name)
	return ok
}

func stripCodeFence(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "```") {
		return text
	}
	if newline := strings.IndexByte(text, '\n'); newline >= 0 {
		text = text[newline+1:]
	}
	text = strings.TrimSuffix(strings.TrimSpace(text), "```")
	return strings.TrimSpace(text)
}

func snapshotRefs(text string) map[string]bool {
	content, _ := mcpSection(text, "Snapshot")
	refs := make(map[string]bool)
	for _, match := range snapshotRefCapture.FindAllStringSubmatch(content, -1) {
		refs[match[1]] = true
	}
	return refs
}

func snapshotLinks(content string, base *neturl.URL, pageURL string, limit int) ([]domain.Link, bool) {
	if base == nil || limit <= 0 {
		return nil, false
	}
	var attributes []string
	var body strings.Builder
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- /url: ") {
			attributes = append(attributes, strings.TrimSpace(strings.TrimPrefix(trimmed, "- /url: ")))
		} else if !strings.HasPrefix(trimmed, "- Page URL: ") && !strings.HasPrefix(trimmed, "- Page Title: ") {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}

	links := make([]domain.Link, 0, limit)
	seen := map[string]bool{pageURL: true}
	truncated := false
	add := func(raw string, relative bool) {
		target := raw
		if relative {
			reference, err := neturl.Parse(raw)
			if err != nil {
				return
			}
			target = base.ResolveReference(reference).String()
		}
		normalized, err := NormalizeURL(target)
		if err != nil || seen[normalized] {
			return
		}
		seen[normalized] = true
		if len(links) >= limit {
			truncated = true
			return
		}
		links = append(links, domain.Link{URL: normalized, Text: "浏览器页面链接"})
	}
	for _, href := range attributes {
		add(href, true)
	}
	for _, target := range reader.ExtractURLs(body.String()) {
		add(target, false)
	}
	return links, truncated
}

func resultText(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var out strings.Builder
	for _, content := range result.Content {
		if value, ok := content.(*mcp.TextContent); ok {
			fmt.Fprintln(&out, value.Text)
		}
	}
	return out.String()
}
