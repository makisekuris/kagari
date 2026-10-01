package reader

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"codeberg.org/readeck/go-readability/v2"
	"golang.org/x/net/html"
	"kagari/internal/domain"
)

func (r *Reader) readArticle(source domain.Source, resp *http.Response, finalURL *url.URL) (domain.Source, error) {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, r.opts.MaxBytes+1))
	if readErr != nil {
		return finishError(source, statusFailed, "could not read page body", readErr)
	}
	byteTruncated := int64(len(body)) > r.opts.MaxBytes
	if byteTruncated {
		body = body[:r.opts.MaxBytes]
	}
	if !isHTML(resp.Header.Get("Content-Type"), body) {
		err := errors.New("response is not an HTML or XHTML document")
		return finishError(source, statusIncomplete, err.Error(), err)
	}
	if restrictedPage(body) {
		err := errors.New("page requires access or a browser challenge")
		return finishError(source, statusRestricted, err.Error(), err)
	}
	article, err := readability.FromReader(bytes.NewReader(body), finalURL)
	if err != nil {
		return finishError(source, statusIncomplete, "could not extract readable article text", err)
	}
	var content bytes.Buffer
	if article.Node != nil {
		if err := article.RenderText(&content); err != nil {
			return finishError(source, statusIncomplete, "could not render readable article text", err)
		}
	}
	source.Title = strings.TrimSpace(article.Title())
	source.Author = strings.TrimSpace(article.Byline())
	if published, err := article.PublishedTime(); err == nil {
		published = published.UTC()
		source.PublishedAt = &published
	}
	source.Content = strings.Join(strings.Fields(content.String()), " ")
	if source.Content == "" {
		err := errors.New("page contained no readable article text")
		return finishError(source, statusIncomplete, err.Error(), err)
	}
	var partial []error
	var reasons []string
	if byteTruncated {
		partial = append(partial, errors.New("page exceeded the byte limit"))
		reasons = append(reasons, "page exceeded the byte limit")
	}
	if chars := []rune(source.Content); len(chars) > r.opts.MaxContentChars {
		source.Content = string(chars[:r.opts.MaxContentChars])
		partial = append(partial, errors.New("article content exceeded the character limit"))
		reasons = append(reasons, "article content exceeded the character limit")
	}
	var linkCap bool
	source.Links, linkCap = extractLinks(article.Node, finalURL, r.opts.MaxLinks)
	if linkCap {
		partial = append(partial, errors.New("article links exceeded the link limit"))
		reasons = append(reasons, "article links exceeded the link limit")
	}
	if len(partial) > 0 {
		source.Status = statusIncomplete
		source.Reason = strings.Join(reasons, "; ")
		source.Truncated = true
		return source, errors.Join(partial...)
	}
	source.Status = statusOK
	return source, nil
}

func isHTML(contentType string, body []byte) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil {
		switch strings.ToLower(mediaType) {
		case "text/html", "application/xhtml+xml":
			return true
		case "application/xml", "text/xml":
			return bytes.Contains(bytes.ToLower(body), []byte("<html"))
		}
	}
	return strings.EqualFold(http.DetectContentType(body), "text/html; charset=utf-8")
}

func restrictedPage(body []byte) bool {
	// A normal article can mention CAPTCHAs or contain a challenge script for
	// its comments. Treat markers as a gate only on a short visible page.
	if doc, err := html.Parse(bytes.NewReader(body)); err == nil && len([]rune(nodeText(doc))) > 2000 {
		return false
	}
	page := strings.ToLower(string(body))
	for _, marker := range []string{
		"cf-challenge", "cf-turnstile", "challenge-platform", "checking your browser",
		"just a moment...", "verify you are human", "are you a robot",
		"enable javascript to run this app", "sign in to continue", "log in to continue",
		"sign in to view this page", "subscribe to continue reading", "subscription required",
	} {
		if strings.Contains(page, marker) {
			return true
		}
	}
	return false
}

func extractLinks(root *html.Node, base *url.URL, limit int) ([]domain.Link, bool) {
	var links []domain.Link
	seen := make(map[string]struct{})
	truncated := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil || truncated {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			href := ""
			for _, attr := range n.Attr {
				if strings.EqualFold(attr.Key, "href") {
					href = strings.TrimSpace(attr.Val)
					break
				}
			}
			if href != "" {
				if target, err := resolveLink(base, href); err == nil {
					if _, ok := seen[target]; !ok {
						if len(links) >= limit {
							truncated = true
							return
						}
						seen[target] = struct{}{}
						links = append(links, domain.Link{URL: target, Text: excerpt(nodeText(n), 200), Context: excerpt(paragraphContext(n), 400)})
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return links, truncated
}

func paragraphContext(n *html.Node) string {
	for parent := n.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode && parent.Data == "p" {
			return nodeText(parent)
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current == nil {
			return
		}
		if current.Type == html.TextNode {
			b.WriteString(current.Data)
			b.WriteByte(' ')
			return
		}
		if current.Type == html.ElementNode && (current.Data == "script" || current.Data == "style") {
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// excerpt 裁剪链接锚文本和附近段落的上下文，避免一条长段落让多个链接重复放大模型输入。
func excerpt(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}
