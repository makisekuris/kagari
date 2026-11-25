package documentarchive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestS3PutOpenAndDownloadURL(t *testing.T) {
	type request struct {
		method        string
		path          string
		contentType   string
		contentLength int64
		authorization string
		body          string
		query         url.Values
		checksums     []string
	}
	var mu sync.Mutex
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s request: %v", r.Method, err)
		}
		var checksumHeaders []string
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-checksum-") {
				checksumHeaders = append(checksumHeaders, name)
			}
		}
		mu.Lock()
		requests = append(requests, request{
			method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"),
			contentLength: r.ContentLength, authorization: r.Header.Get("Authorization"),
			body: string(body), query: r.URL.Query(), checksums: checksumHeaders,
		})
		mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			w.Header().Set("ETag", `"test-etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Length", "12")
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = io.WriteString(w, "stored bytes")
		default:
			t.Errorf("unexpected HTTP method %q", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	store, err := NewS3(server.URL, "archive-test", "", "unit-test-access-key", "unit-test-secret-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "folder/document v1.pdf", strings.NewReader("archived"), int64(len("archived")), "application/pdf"); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	object, err := store.Open(context.Background(), "folder/document v1.pdf")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	content, err := io.ReadAll(object)
	if err != nil {
		t.Fatalf("read Open() body: %v", err)
	}
	if err := object.Close(); err != nil {
		t.Fatalf("close Open() body: %v", err)
	}
	if string(content) != "stored bytes" {
		t.Fatalf("Open() body = %q", content)
	}

	name := "../../保密 \"plan\".pdf\r\nextra"
	link, err := store.DownloadURL(context.Background(), "folder/document v1.pdf", name, 10*time.Minute)
	if err != nil {
		t.Fatalf("DownloadURL() error = %v", err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse DownloadURL() = %v", err)
	}
	if parsed.Path != "/archive-test/folder/document v1.pdf" {
		t.Fatalf("presigned URL path = %q; want path-style bucket and object", parsed.Path)
	}
	query := parsed.Query()
	if query.Get("X-Amz-Expires") != "600" {
		t.Fatalf("presigned expiry = %q, want 600 seconds", query.Get("X-Amz-Expires"))
	}
	disposition := query.Get("response-content-disposition")
	if !strings.Contains(disposition, "attachment") || !strings.Contains(disposition, "plan") || strings.Contains(disposition, "../") || strings.ContainsAny(disposition, "\r\n") {
		t.Fatalf("unsafe or missing presigned Content-Disposition: %q", disposition)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("S3 requests = %d, want 2", len(requests))
	}
	put := requests[0]
	if put.method != http.MethodPut || put.path != "/archive-test/folder/document v1.pdf" || put.contentType != "application/pdf" || put.contentLength != int64(len("archived")) || put.body != "archived" {
		t.Fatalf("PutObject request = %+v", put)
	}
	if !strings.Contains(put.authorization, "Credential=unit-test-access-key/") || !strings.Contains(put.authorization, "/us-east-1/s3/aws4_request") {
		t.Fatalf("request was not signed with static credentials and default region: %q", put.authorization)
	}
	if len(put.checksums) != 0 {
		t.Fatalf("optional checksum headers sent to S3-compatible service: %v", put.checksums)
	}
	if got := requests[1]; got.method != http.MethodGet || got.path != "/archive-test/folder/document v1.pdf" {
		t.Fatalf("GetObject request = %+v", got)
	}
}

func TestS3RejectsInvalidOptionsWithoutExposingCredentials(t *testing.T) {
	const secret = "unit-test-secret-placeholder"
	for _, endpoint := range []string{"", "ftp://rustfs.example", "http://user:pass@rustfs.example", "http://rustfs.example?token=hidden"} {
		_, err := NewS3(endpoint, "archive-test", "", "unit-test-access", secret)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("NewS3(%q) error = %v; want safe endpoint validation error", endpoint, err)
		}
	}
	if _, err := NewS3("http://rustfs.example", "", "", "unit-test-access", secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("NewS3 with empty bucket error = %v; want safe validation error", err)
	}

	store, err := NewS3("http://rustfs.example", "archive-test", "us-east-1", "unit-test-access", secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, ttl := range []time.Duration{0, time.Second - 1, maxPresignTTL + time.Second} {
		if _, err := store.DownloadURL(context.Background(), "object", "name.pdf", ttl); err == nil {
			t.Errorf("DownloadURL() accepted TTL %s", ttl)
		}
	}
}

func TestSafeDownloadFilename(t *testing.T) {
	for input, want := range map[string]string{
		"../folder/report.pdf": "report.pdf",
		`..\folder\report.pdf`: "report.pdf",
		"\r\n":                 "download",
		"/":                    "download",
	} {
		if got := safeDownloadFilename(input); got != want {
			t.Errorf("safeDownloadFilename(%q) = %q, want %q", input, got, want)
		}
	}
}
