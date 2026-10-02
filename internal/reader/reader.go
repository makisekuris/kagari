package reader

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"kagari/internal/domain"
)

type Options struct {
	Timeout               time.Duration
	MaxBytes              int64
	MaxContentChars       int
	MaxLinks              int
	AllowedNonPublicCIDRs []string
}

type Reader struct {
	opts                  Options
	allowedNonPublicCIDRs []netip.Prefix
	client                *http.Client
}

func New(opts Options) *Reader {
	opts = defaults(opts)
	return newReaderWithTransport(opts, safeTransport(opts))
}

func defaults(opts Options) Options {
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 4 << 20
	}
	if opts.MaxContentChars <= 0 {
		opts.MaxContentChars = 100_000
	}
	if opts.MaxLinks <= 0 {
		opts.MaxLinks = 100
	}
	return opts
}

// Read 即使遇到错误也返回 Source：incomplete 可能带有上限裁剪后的可用片段，上层须结合状态与原因判断是否使用；
// restricted 表示访问受限或目标未能验证，failed 表示输入、请求等技术错误阻止了解析。
func (r *Reader) Read(ctx context.Context, rawURL string) (domain.Source, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized, err := NormalizeURL(rawURL)
	if err != nil {
		return failedSource(rawURL, strings.TrimSpace(rawURL), statusFailed, err.Error(), err)
	}
	source := newSource(normalized, normalized)
	requestURL, _ := url.Parse(normalized)
	if isXStatusURL(requestURL) {
		source.Kind, source.ReadMethod = "discussion", "x_oembed"
		target, ok := parseXStatusURL(requestURL)
		if !ok {
			err := errors.New("X status URL must identify a public post with a numeric status ID")
			return finishError(source, statusRestricted, err.Error(), err)
		}
		source.URL = target.canonicalURL
		source.ID = sourceID(source.URL)
		return r.readXPost(ctx, source, target)
	}
	if ip := net.ParseIP(requestURL.Hostname()); ip != nil && !isAllowedTargetIP(ip, "", r.allowedNonPublicCIDRs) {
		return finishError(source, statusRestricted, errUnsafeTarget.Error(), errUnsafeTarget)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalized, nil)
	if err != nil {
		return finishError(source, statusFailed, "could not create request", err)
	}
	req.Header.Set("User-Agent", "KagariReader/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.1")
	resp, err := r.client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		status := statusFailed
		if errors.Is(err, errUnsafeTarget) || errors.Is(err, errUnverifiedXStatus) {
			status = statusRestricted
		}
		return finishError(source, status, safeErrorReason(err), err)
	}
	defer resp.Body.Close()
	if resp.Request != nil && resp.Request.URL != nil {
		if finalURL, normErr := NormalizeURL(resp.Request.URL.String()); normErr == nil {
			source.URL = finalURL
		}
	}
	source.ID = sourceID(source.URL)
	finalURL, _ := url.Parse(source.URL)
	if isXStatusURL(finalURL) {
		err := errors.New("X status pages do not provide verified post text through ordinary HTML extraction")
		return finishError(source, statusRestricted, err.Error(), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("HTTP status %d", resp.StatusCode)
		status := statusFailed
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnavailableForLegalReasons {
			status = statusRestricted
		}
		return finishError(source, status, err.Error(), err)
	}
	return r.readArticle(source, resp, finalURL)
}
