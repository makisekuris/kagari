package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"kagari/internal/domain"
)

type xStatus struct {
	user         string
	id           string
	canonicalURL string
}

type xOEmbed struct {
	URL        string `json:"url"`
	AuthorName string `json:"author_name"`
	HTML       string `json:"html"`
}

func parseXStatusURL(u *url.URL) (xStatus, bool) {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return xStatus{}, false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host != "x.com" && host != "twitter.com" {
		return xStatus{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[1] != "status" || !validXUser(parts[0]) || !digitsOnly(parts[2]) {
		return xStatus{}, false
	}
	user := strings.ToLower(parts[0])
	canonical := "https://x.com/" + url.PathEscape(user) + "/status/" + parts[2]
	return xStatus{user: user, id: parts[2], canonicalURL: canonical}, true
}

func isXStatusURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host != "x.com" && host != "twitter.com" {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 1; i+1 < len(parts); i++ {
		if parts[i] == "status" && parts[i+1] != "" {
			return true
		}
	}
	return false
}

func validXUser(user string) bool {
	if len(user) < 1 || len(user) > 15 {
		return false
	}
	for _, r := range user {
		if !('a' <= r && r <= 'z') && !('A' <= r && r <= 'Z') && !('0' <= r && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// readXPost 只接受官方 oEmbed 中 URL 与目标 status ID 一致、且正文所在 blockquote 自带目标链接的单帖。
// 返回内容不覆盖线程回复或链接卡片，调用方会在 Source.Reason 中保留这一限制。
func (r *Reader) readXPost(ctx context.Context, source domain.Source, target xStatus) (domain.Source, error) {
	params := url.Values{
		"url":         {"https://twitter.com/" + url.PathEscape(target.user) + "/status/" + target.id},
		"omit_script": {"true"},
		"hide_thread": {"true"},
	}
	endpoint := "https://publish.x.com/oembed?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return finishError(source, statusFailed, "could not create X oEmbed request", err)
	}
	req.Header.Set("User-Agent", "KagariReader/1.0")
	req.Header.Set("Accept", "application/json")
	client := *r.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		status := statusFailed
		if errors.Is(err, errUnsafeTarget) {
			status = statusRestricted
		}
		return finishError(source, status, safeErrorReason(err), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("X oEmbed returned HTTP status %d", resp.StatusCode)
		status := statusFailed
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusTooManyRequests {
			status = statusRestricted
		}
		return finishError(source, status, err.Error(), err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, r.opts.MaxBytes+1))
	if err != nil {
		return finishError(source, statusFailed, "could not read X oEmbed response", err)
	}
	if int64(len(body)) > r.opts.MaxBytes {
		err := errors.New("X oEmbed response exceeded the byte limit")
		source.Truncated = true
		return finishError(source, statusIncomplete, err.Error(), err)
	}
	var embed xOEmbed
	if err := json.Unmarshal(body, &embed); err != nil {
		return finishError(source, statusIncomplete, "X oEmbed response was not usable JSON", err)
	}
	returnedURL, err := url.Parse(embed.URL)
	returned, ok := parseXStatusURL(returnedURL)
	if err != nil || !ok || returned.id != target.id {
		err := errors.New("X oEmbed response did not match the requested post")
		return finishError(source, statusRestricted, err.Error(), err)
	}
	doc, err := html.Parse(strings.NewReader(embed.HTML))
	if err != nil {
		return finishError(source, statusIncomplete, "X oEmbed HTML was not usable", err)
	}
	paragraph, verified := verifiedPostParagraph(doc, target)
	if !verified {
		err := errors.New("X oEmbed HTML did not contain the requested post permalink")
		return finishError(source, statusRestricted, err.Error(), err)
	}
	if paragraph == nil {
		err := errors.New("X oEmbed response contained no post body paragraph")
		return finishError(source, statusIncomplete, err.Error(), err)
	}
	source.Content = nodeText(paragraph)
	if source.Content == "" {
		err := errors.New("X oEmbed response contained no post text")
		return finishError(source, statusIncomplete, err.Error(), err)
	}
	source.Author = strings.TrimSpace(embed.AuthorName)
	source.Title = source.Author
	base, _ := url.Parse(source.URL)
	source.Links, source.Truncated = extractXLinks(paragraph, base, r.opts.MaxLinks)
	var partial []error
	var reasons []string
	if chars := []rune(source.Content); len(chars) > r.opts.MaxContentChars {
		source.Content = string(chars[:r.opts.MaxContentChars])
		partial = append(partial, errors.New("X post text exceeded the character limit"))
		reasons = append(reasons, "post text exceeded the character limit")
	}
	if source.Truncated {
		partial = append(partial, errors.New("X post links exceeded the link limit"))
		reasons = append(reasons, "post links exceeded the link limit")
	}
	if len(partial) > 0 {
		source.Status = statusIncomplete
		source.Reason = strings.Join(reasons, "; ")
		source.Truncated = true
		return source, errors.Join(partial...)
	}
	source.Status = statusOK
	source.Reason = "Only the public post text is available; thread replies and link cards are not included."
	return source, nil
}

func verifiedPostParagraph(root *html.Node, target xStatus) (*html.Node, bool) {
	var find func(*html.Node) (*html.Node, bool)
	find = func(n *html.Node) (*html.Node, bool) {
		if n == nil {
			return nil, false
		}
		if n.Type == html.ElementNode && n.Data == "blockquote" && hasClass(n, "twitter-tweet") && hasTargetPermalink(n, target) {
			return firstPostParagraph(n), true
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if p, ok := find(child); ok {
				return p, true
			}
		}
		return nil, false
	}
	return find(root)
}

func hasClass(n *html.Node, class string) bool {
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, "class") {
			for _, value := range strings.Fields(attr.Val) {
				if value == class {
					return true
				}
			}
		}
	}
	return false
}

func hasTargetPermalink(root *html.Node, target xStatus) bool {
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n != root && n.Type == html.ElementNode && n.Data == "blockquote" {
			return false
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if strings.EqualFold(attr.Key, "href") {
					u, err := url.Parse(attr.Val)
					ref, ok := parseXStatusURL(u)
					if err == nil && ok && ref.id == target.id {
						return true
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if walk(child) {
				return true
			}
		}
		return false
	}
	return walk(root)
}

func firstPostParagraph(root *html.Node) *html.Node {
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == "blockquote" {
			continue
		}
		if child.Type == html.ElementNode && child.Data == "p" {
			return child
		}
		if paragraph := firstPostParagraph(child); paragraph != nil {
			return paragraph
		}
	}
	return nil
}

func extractXLinks(paragraph *html.Node, base *url.URL, limit int) ([]domain.Link, bool) {
	content := nodeText(paragraph)
	var links []domain.Link
	seen := make(map[string]struct{})
	truncated := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil || truncated {
			return
		}
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if !strings.EqualFold(attr.Key, "href") || strings.TrimSpace(attr.Val) == "" {
					continue
				}
				target, err := resolveLink(base, attr.Val)
				if err != nil {
					break
				}
				u, _ := url.Parse(target)
				if isXOrTwitterHost(u.Hostname()) {
					break
				}
				links, truncated = appendLink(links, seen, target, nodeText(n), content, limit)
				break
			}
			return
		}
		if n.Type == html.TextNode {
			links, truncated = appendTextURLs(links, n.Data, content, limit, true, seen)
			if truncated {
				return
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(paragraph)
	return links, truncated
}

func isXOrTwitterHost(host string) bool {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	return host == "x.com" || host == "twitter.com"
}
