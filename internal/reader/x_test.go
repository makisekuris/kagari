package reader

import (
	"context"
	"errors"
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

func TestReadXAttachedShortLink(t *testing.T) {
	for _, target := range []string{"https://example.test/article", "http://127.0.0.1/admin"} {
		t.Run(target, func(t *testing.T) {
			var requests []string
			r := newReaderWithTransport(Options{}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests = append(requests, req.URL.String())
				switch req.URL.Host {
				case "publish.x.com":
					return response(req, http.StatusOK, "application/json", xOEmbedFixture), nil
				case "t.co":
					res := response(req, http.StatusFound, "text/html", "")
					res.Header.Set("Location", target)
					return res, nil
				case "example.test":
					return response(req, http.StatusOK, "text/html", `<html><head><title>Linked report</title></head><body><article><p>The attached report confirms that the bridge reopened after inspection.</p></article></body></html>`), nil
				default:
					t.Fatalf("unexpected request: %s", req.URL)
					return nil, errors.New("unexpected request")
				}
			}))
			post, err := r.Read(context.Background(), "https://x.com/jack/status/20")
			if err != nil || len(post.Links) != 1 {
				t.Fatalf("post links = %+v, err = %v", post.Links, err)
			}
			linked, err := r.Read(context.Background(), post.Links[0].URL)
			if target == "http://127.0.0.1/admin" {
				if !errors.Is(err, errUnsafeTarget) || linked.Status != statusRestricted || len(requests) != 2 {
					t.Fatalf("unsafe attached redirect: source=%+v err=%v requests=%v", linked, err, requests)
				}
				return
			}
			if err != nil || linked.Status != statusOK || linked.RequestedURL != "https://t.co/abc" || linked.URL != target || linked.ID != sourceID(target) || !strings.Contains(linked.Content, "bridge reopened") || len(requests) != 3 {
				t.Fatalf("attached article: source=%+v err=%v requests=%v", linked, err, requests)
			}
		})
	}
}

func TestReadXFindsTextURLsOnlyInVerifiedParagraph(t *testing.T) {
	fixture := strings.Replace(xOEmbedFixture, `here</a></p>`, `https://display.example/item</a>, then https://article.example/report. Also https://twitter.com/jack/status/21 <script>https://script.example/skip</script><style>https://style.example/skip</style></p>`, 1)
	source, err := xFixtureReader(t, http.StatusOK, fixture).Read(context.Background(), "https://x.com/jack/status/20")
	if err != nil || source.Status != statusOK || len(source.Links) != 2 {
		t.Fatalf("X text links: source=%#v err=%v", source, err)
	}
	if source.Links[0].URL != "https://t.co/abc" || source.Links[1].URL != "https://article.example/report" || source.Links[1].Context != source.Content {
		t.Fatalf("X text links/context = %#v", source.Links)
	}
}

func TestReadXTextLinksUseAnchorLinkLimit(t *testing.T) {
	fixture := strings.Replace(xOEmbedFixture, `here</a></p>`, `here</a> https://article.example/report</p>`, 1)
	r := newReaderWithTransport(Options{MaxLinks: 1}, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(req, http.StatusOK, "application/json", fixture), nil
	}))
	source, err := r.Read(context.Background(), "https://x.com/jack/status/20")
	if err == nil || source.Status != statusIncomplete || !source.Truncated || len(source.Links) != 1 || source.Links[0].URL != "https://t.co/abc" {
		t.Fatalf("X text URL cap: source=%#v err=%v", source, err)
	}
}
