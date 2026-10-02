package reader

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(req *http.Request, status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestReadArticleMetadataLinksAndShortText(t *testing.T) {
	page := `<!doctype html><html><head><title>Readable page</title><meta name="author" content="Ada Example"><meta property="article:published_time" content="2024-02-03T04:05:06Z"></head><body><nav>Navigation only</nav><article><h1>Readable title</h1><p>A brief <a href="/reference?keep=yes#section">reference</a> gives the important detail.</p><p>Readers can follow this second note.</p><a href="/reference?keep=yes">duplicate target</a></article></body></html>`
	reader := newReaderWithTransport(Options{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(req, http.StatusOK, "text/html; charset=utf-8", page), nil
	}))
	source, err := reader.Read(context.Background(), "HTTPS://Example.Test:443/article?edition=2#top")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if source.Status != statusOK || source.URL != "https://example.test/article?edition=2" {
		t.Fatalf("unexpected source identity/status: %#v", source)
	}
	if source.Title == "" || !strings.Contains(source.Content, "important detail") || source.Author != "Ada Example" {
		t.Fatalf("article metadata/content missing: %#v", source)
	}
	if source.PublishedAt == nil || source.PublishedAt.Format("2006-01-02") != "2024-02-03" {
		t.Fatalf("published time = %v", source.PublishedAt)
	}
	if len(source.Links) != 1 || source.Links[0].URL != "https://example.test/reference?keep=yes" || source.Links[0].Text != "reference" || !strings.Contains(source.Links[0].Context, "important detail") {
		t.Fatalf("article links/context = %#v", source.Links)
	}
	if source.ID != sourceID(source.URL) {
		t.Fatalf("source ID %q is not derived from normalized final URL", source.ID)
	}
	again, err := reader.Read(context.Background(), "https://example.test/article?edition=2")
	if err != nil || again.ID != source.ID {
		t.Fatalf("source ID changed for same normalized URL: %q and %q, err=%v", source.ID, again.ID, err)
	}

	short := `<!doctype html><html><body><article><h1>Brief update</h1><p>The bridge reopened today after a short inspection.</p></article></body></html>`
	reader = newReaderWithTransport(Options{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(req, http.StatusOK, "application/xhtml+xml; charset=utf-8", short), nil
	}))
	source, err = reader.Read(context.Background(), "https://example.test/brief")
	if err != nil || source.Status != statusOK || !strings.Contains(source.Content, "bridge reopened today") {
		t.Fatalf("short XHTML article was not accepted: status=%q content=%q err=%v", source.Status, source.Content, err)
	}
}

func TestNormalizeAndExtractURLs(t *testing.T) {
	normalized, err := NormalizeURL(" HTTPS://BÜCHER.example:443/a?b=2&a=1#frag ")
	if err != nil || normalized != "https://xn--bcher-kva.example/a?b=2&a=1" {
		t.Fatalf("NormalizeURL() = %q, %v", normalized, err)
	}
	for _, invalid := range []string{"javascript:alert(1)", "https://user:pass@example.com/", "https://example.com:70000/", "//example.com/path"} {
		if _, err := NormalizeURL(invalid); err == nil {
			t.Errorf("NormalizeURL(%q) unexpectedly succeeded", invalid)
		}
	}
	got := ExtractURLs("Read (https://Example.com/a_(b)?x=1#here), then https://example.com/a_(b)?x=1.")
	want := []string{"https://example.com/a_(b)?x=1"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ExtractURLs() = %#v, want %#v", got, want)
	}
}

func TestReadRejectsPrivateTargetsAndXStatusWithoutFetching(t *testing.T) {
	calls := 0
	reader := newReaderWithTransport(Options{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return response(req, http.StatusOK, "text/html", `<html><body><article><p>Anything.</p></article></body></html>`), nil
	}))
	for _, raw := range []string{"http://127.0.0.1/admin", "https://[::1]/admin", "https://x.com/someone/status/not-a-number"} {
		source, err := reader.Read(context.Background(), raw)
		if err == nil || (source.Status != statusRestricted && source.Status != statusFailed) {
			t.Errorf("Read(%q) = status %q, err %v", raw, source.Status, err)
		}
	}
	if calls != 0 {
		t.Fatalf("blocked targets reached transport %d times", calls)
	}
}

func TestDialPublicRejectsMixedPrivateDNSAnswers(t *testing.T) {
	dials := 0
	_, err := dialPublic(context.Background(), "tcp", "example.test:443", nil, func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("100.64.0.12")}}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("unexpected dial")
	})
	if !errors.Is(err, errUnsafeTarget) || dials != 0 {
		t.Fatalf("mixed public/private DNS answer: err=%v dials=%d", err, dials)
	}
}

func TestDialAllowedNonPublicCIDRUsesPinnedIPAndRejectsMixedAnswers(t *testing.T) {
	allowed := parseAllowedNonPublicCIDRs([]string{"198.18.0.0/16"})
	var target string
	conn, err := dialPublic(context.Background(), "tcp", "example.test:443", allowed, func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("198.18.0.1")}}, nil
	}, func(_ context.Context, _, address string) (net.Conn, error) {
		target = address
		local, remote := net.Pipe()
		_ = remote.Close()
		return local, nil
	})
	if err != nil {
		t.Fatalf("configured fake-IP dial failed: %v", err)
	}
	_ = conn.Close()
	if target != "198.18.0.1:443" {
		t.Fatalf("dial target = %q, want the validated IP", target)
	}

	dials := 0
	_, err = dialPublic(context.Background(), "tcp", "example.test:443", allowed, func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("198.18.0.1")}, {IP: net.ParseIP("100.64.0.12")}}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("unexpected dial")
	})
	if !errors.Is(err, errUnsafeTarget) || dials != 0 {
		t.Fatalf("mixed DNS answer with an unapproved address: err=%v dials=%d", err, dials)
	}
}

func TestReadAllowsOnlyConfiguredNonPublicCIDRs(t *testing.T) {
	page := `<!doctype html><html><body><article><p>A useful fake-IP response.</p></article></body></html>`
	calls := 0
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return response(req, http.StatusOK, "text/html", page), nil
	})
	reader := newReaderWithTransport(Options{}, transport)
	blocked, err := reader.Read(context.Background(), "http://198.18.0.1/article")
	if !errors.Is(err, errUnsafeTarget) || blocked.Status != statusRestricted || calls != 0 {
		t.Fatalf("default fake-IP policy: source=%#v err=%v calls=%d", blocked, err, calls)
	}

	reader = newReaderWithTransport(Options{AllowedNonPublicCIDRs: []string{"198.18.0.0/16"}}, transport)
	allowed, err := reader.Read(context.Background(), "http://198.18.0.1/article")
	if err != nil || allowed.Status != statusOK || calls != 1 {
		t.Fatalf("configured fake-IP policy: source=%#v err=%v calls=%d", allowed, err, calls)
	}

	reader = newReaderWithTransport(Options{AllowedNonPublicCIDRs: []string{"bad-cidr", "::ffff:198.18.0.0/112"}}, transport)
	blocked, err = reader.Read(context.Background(), "http://198.18.0.1/article")
	if !errors.Is(err, errUnsafeTarget) || blocked.Status != statusRestricted || calls != 1 {
		t.Fatalf("invalid options broadened fake-IP access: source=%#v err=%v calls=%d", blocked, err, calls)
	}
}

func TestReadRedirectUsesConfiguredNonPublicCIDRs(t *testing.T) {
	page := `<!doctype html><html><body><article><p>A useful redirected response.</p></article></body></html>`
	calls := 0
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host == "example.test" {
			res := response(req, http.StatusFound, "text/html", "")
			res.Header.Set("Location", "http://198.18.0.1/final")
			return res, nil
		}
		return response(req, http.StatusOK, "text/html", page), nil
	})
	reader := newReaderWithTransport(Options{}, transport)
	blocked, err := reader.Read(context.Background(), "https://example.test/start")
	if !errors.Is(err, errUnsafeTarget) || blocked.Status != statusRestricted || calls != 1 {
		t.Fatalf("default fake-IP redirect: source=%#v err=%v calls=%d", blocked, err, calls)
	}

	reader = newReaderWithTransport(Options{AllowedNonPublicCIDRs: []string{"198.18.0.0/16"}}, transport)
	allowed, err := reader.Read(context.Background(), "https://example.test/start")
	if err != nil || allowed.Status != statusOK || calls != 3 {
		t.Fatalf("configured fake-IP redirect: source=%#v err=%v calls=%d", allowed, err, calls)
	}
}

func TestAllowedNonPublicCIDRIPBoundaries(t *testing.T) {
	allowed := parseAllowedNonPublicCIDRs([]string{"198.18.0.0/16", "fe80::/10", "bad-cidr"})
	if !isAllowedTargetIP(net.ParseIP("::ffff:198.18.0.1"), "", allowed) {
		t.Fatal("IPv4-mapped address did not match its IPv4 exception")
	}
	if isAllowedTargetIP(net.ParseIP("fe80::1"), "eth0", allowed) {
		t.Fatal("zoned IPv6 address was allowed")
	}
	if isAllowedTargetIP(nil, "", allowed) {
		t.Fatal("invalid IP was allowed")
	}
}

func TestIsPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"1.1.1.1":              true,
		"10.1.2.3":             false,
		"100.64.0.1":           false,
		"169.254.169.254":      false,
		"::ffff:127.0.0.1":     false,
		"fd00::1":              false,
		"2001:4860:4860::8888": true,
		"2001:db8::1":          false,
		"240.0.0.1":            false,
	} {
		if got := isPublicIP(net.ParseIP(ip)); got != want {
			t.Errorf("isPublicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestReadRejectsPrivateRedirect(t *testing.T) {
	calls := 0
	reader := newReaderWithTransport(Options{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		res := response(req, http.StatusFound, "text/html", "")
		res.Header.Set("Location", "http://127.0.0.1/internal")
		return res, nil
	}))
	source, err := reader.Read(context.Background(), "https://example.test/start")
	if !errors.Is(err, errUnsafeTarget) || source.Status != statusRestricted || calls != 1 {
		t.Fatalf("private redirect: source=%#v err=%v calls=%d", source, err, calls)
	}
}

func TestReadReturnsIncompleteOnByteLimitAndChallenges(t *testing.T) {
	long := `<!doctype html><html><body><article><h1>Report</h1><p>` + strings.Repeat("A useful report explains what changed. ", 30) + `</p></article></body></html>`
	reader := newReaderWithTransport(Options{MaxBytes: 700}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(req, http.StatusOK, "text/html", long), nil
	}))
	source, err := reader.Read(context.Background(), "https://example.test/large")
	if err == nil || source.Status != statusIncomplete || !source.Truncated {
		t.Fatalf("byte limit: source status=%q truncated=%v err=%v", source.Status, source.Truncated, err)
	}

	reader = newReaderWithTransport(Options{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(req, http.StatusOK, "text/html", `<html><body><div class="cf-challenge">Checking your browser</div></body></html>`), nil
	}))
	source, err = reader.Read(context.Background(), "https://example.test/challenge")
	if err == nil || source.Status != statusRestricted {
		t.Fatalf("challenge: source status=%q err=%v", source.Status, err)
	}
}

func TestReadByteLimitCountsDecompressedBody(t *testing.T) {
	page := `<!doctype html><html><body><article><h1>Report</h1><p>` + strings.Repeat("Useful details matter. ", 100) + `</p></article></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		zipper := gzip.NewWriter(w)
		_, _ = io.WriteString(zipper, page)
		_ = zipper.Close()
	}))
	defer server.Close()
	dialer := &net.Dialer{}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	reader := newReaderWithTransport(Options{MaxBytes: 512}, transport)
	source, err := reader.Read(context.Background(), "http://public.example/article")
	if err == nil || source.Status != statusIncomplete || !source.Truncated {
		t.Fatalf("decompressed size limit: status=%q truncated=%v err=%v", source.Status, source.Truncated, err)
	}
}

func TestReadMarksContentAndLinkCapsIncomplete(t *testing.T) {
	page := `<!doctype html><html><body><article><p>` + strings.Repeat("A useful report explains what changed. ", 8) + `</p><p><a href="/one">one</a> <a href="/two">two</a></p></article></body></html>`
	reader := newReaderWithTransport(Options{MaxContentChars: 20, MaxLinks: 1}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(req, http.StatusOK, "text/html", page), nil
	}))
	source, err := reader.Read(context.Background(), "https://example.test/capped")
	if err == nil || source.Status != statusIncomplete || !source.Truncated || len([]rune(source.Content)) != 20 || len(source.Links) != 1 {
		t.Fatalf("caps: source=%#v err=%v", source, err)
	}
}
