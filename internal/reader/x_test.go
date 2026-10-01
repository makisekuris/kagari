package reader

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const xOEmbedFixture = `{"url":"https://x.com/jack/status/20","author_name":"Jack","html":"<blockquote class=\"twitter-tweet\"><p lang=\"en\" dir=\"ltr\">Just setting up my twttr <a href=\"https://t.co/abc\">here</a></p>&mdash; <a href=\"https://twitter.com/jack/status/20?ref_src=twsrc%5Etfw\">March 21, 2006</a></blockquote>"}`

func xFixtureReader(t *testing.T, status int, payload string) *Reader {
	t.Helper()
	return newReaderWithTransport(Options{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Host != "publish.x.com" || req.URL.Path != "/oembed" {
			t.Errorf("unexpected X request: %s %s", req.Method, req.URL)
		}
		query := req.URL.Query()
		if query.Get("url") != "https://twitter.com/jack/status/20" || query.Get("omit_script") != "true" || query.Get("hide_thread") != "true" {
			t.Errorf("unexpected oEmbed query: %v", query)
		}
		return response(req, status, "application/json; charset=utf-8", payload), nil
	}))
}

func TestReadXStatusFromVerifiedOEmbed(t *testing.T) {
	reader := xFixtureReader(t, http.StatusOK, xOEmbedFixture)
	source, err := reader.Read(context.Background(), "https://twitter.com/jack/status/20?s=20")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if source.Status != statusOK || source.Kind != "discussion" || source.ReadMethod != "x_oembed" || source.URL != "https://x.com/jack/status/20" {
		t.Fatalf("unexpected X source: %#v", source)
	}
	if source.Author != "Jack" || source.Content != "Just setting up my twttr here" || strings.Contains(source.Content, "March 21") {
		t.Fatalf("X post content/metadata = %#v", source)
	}
	if len(source.Links) != 1 || source.Links[0].URL != "https://t.co/abc" || source.Links[0].Text != "here" || source.Links[0].Context != source.Content {
		t.Fatalf("X post links/context = %#v", source.Links)
	}
	if !strings.Contains(source.Reason, "thread replies") {
		t.Fatalf("missing oEmbed scope limitation: %q", source.Reason)
	}
}

func TestReadXStatusRejectsMismatchedTargets(t *testing.T) {
	wrongURL := strings.Replace(xOEmbedFixture, `https://x.com/jack/status/20`, `https://x.com/jack/status/21`, 1)
	wrongPermalink := strings.Replace(xOEmbedFixture, `https://twitter.com/jack/status/20?ref_src=twsrc%5Etfw`, `https://twitter.com/jack/status/21?ref_src=twsrc%5Etfw`, 1)
	for name, fixture := range map[string]string{"returned URL": wrongURL, "HTML permalink": wrongPermalink} {
		t.Run(name, func(t *testing.T) {
			source, err := xFixtureReader(t, http.StatusOK, fixture).Read(context.Background(), "https://x.com/jack/status/20")
			if err == nil || source.Status != statusRestricted || source.Content != "" {
				t.Fatalf("mismatch accepted: source=%#v err=%v", source, err)
			}
		})
	}
}

func TestReadXStatusHandlesUnavailableAndEmptyPosts(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		source, err := xFixtureReader(t, code, "{}").Read(context.Background(), "https://x.com/jack/status/20")
		if err == nil || source.Status != statusRestricted {
			t.Errorf("HTTP %d: source status=%q err=%v", code, source.Status, err)
		}
	}
	emptyPost := `{"url":"https://x.com/jack/status/20","html":"<blockquote class=\"twitter-tweet\">&mdash; <a href=\"https://twitter.com/jack/status/20\">date</a></blockquote>"}`
	source, err := xFixtureReader(t, http.StatusOK, emptyPost).Read(context.Background(), "https://x.com/jack/status/20")
	if err == nil || source.Status != statusIncomplete {
		t.Fatalf("empty post: source status=%q err=%v", source.Status, err)
	}
}
