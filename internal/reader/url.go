package reader

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/idna"

	"kagari/internal/domain"
)

var urlPattern = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"']+`)

// NormalizeURL canonicalizes URL syntax while preserving path and query semantics.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("URL scheme must be http or https")
	}
	if u.Opaque != "" || u.Host == "" || u.User != nil {
		return "", errors.New("URL must have a host and no credentials")
	}
	host := u.Hostname()
	if host == "" || strings.Contains(host, "%") {
		return "", errors.New("invalid URL host")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil || ascii == "" {
			return "", errors.New("invalid URL host")
		}
		host = strings.ToLower(ascii)
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid URL port")
		}
		port = strconv.Itoa(n)
		if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
			port = ""
		}
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}

// ExtractURLs returns normalized, deduplicated HTTP(S) links in input order.
func ExtractURLs(text string) []string {
	seen := make(map[string]struct{})
	var urls []string
	for _, match := range urlPattern.FindAllString(text, -1) {
		match = trimURLPunctuation(match)
		normalized, err := NormalizeURL(match)
		if err != nil {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		urls = append(urls, normalized)
	}
	return urls
}

func appendTextURLs(links []domain.Link, text, context string, limit int, excludeX bool, seen map[string]struct{}) ([]domain.Link, bool) {
	for _, target := range ExtractURLs(text) {
		if excludeX {
			u, _ := url.Parse(target)
			if isXOrTwitterHost(u.Hostname()) {
				continue
			}
		}
		var capped bool
		links, capped = appendLink(links, seen, target, target, context, limit)
		if capped {
			return links, true
		}
	}
	return links, false
}

func appendLink(links []domain.Link, seen map[string]struct{}, target, text, context string, limit int) ([]domain.Link, bool) {
	if _, ok := seen[target]; ok {
		return links, false
	}
	if len(links) >= limit {
		return links, true
	}
	seen[target] = struct{}{}
	return append(links, domain.Link{URL: target, Text: excerpt(text, 200), Context: excerpt(context, 400)}), false
}

func trimURLPunctuation(s string) string {
	s = strings.TrimRight(s, ".,;:!?'”’，。；：！？、")
	for _, pair := range [][2]byte{{')', '('}, {']', '['}, {'}', '{'}} {
		for strings.HasSuffix(s, string(pair[0])) && strings.Count(s, string(pair[0])) > strings.Count(s, string(pair[1])) {
			s = strings.TrimSuffix(s, string(pair[0]))
		}
	}
	return s
}

func resolveLink(base *url.URL, href string) (string, error) {
	reference, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	return NormalizeURL(base.ResolveReference(reference).String())
}
