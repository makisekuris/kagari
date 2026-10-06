package logging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

func TestErrorFieldsAreUsefulAndDoNotExposeRawErrors(t *testing.T) {
	apiErr := &openai.Error{
		StatusCode: 403,
		Request:    &http.Request{Method: "POST", URL: &url.URL{Scheme: "https", Host: "api.example", Path: "/v1/responses", RawQuery: "key=fixture_secret_query"}},
		Response:   &http.Response{StatusCode: 403},
	}
	if err := apiErr.UnmarshalJSON([]byte(`{"message":"fixture_secret_body","type":"invalid_request_error","code":"invalid_api_key"}`)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"api", fmt.Errorf("wrapped: %w", apiErr), "model request failed"},
		{"canceled", fmt.Errorf("wrapped: %w", context.Canceled), "operation canceled"},
		{"deadline", context.DeadlineExceeded, "operation deadline exceeded"},
		{"dns", &net.DNSError{Err: "fixture_secret_body", Name: "secret.example", IsNotFound: true}, "DNS lookup failed"},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, Err: errors.New("fixture_secret_body")}, "network connection failed"},
		{"json", &json.SyntaxError{Offset: 12}, "invalid JSON response"},
		{"url", &url.Error{Op: "Get", URL: "https://user:pass@example.org/path?token=secret", Err: errors.New("fixture_secret_body")}, "URL request failed"},
		{"unknown", errors.New("SSE error contains fixture_secret_body"), "operation failed"},
		{"unreadable", errors.New(noReadableContent), "no readable content"},
		{"incomplete", errors.New("model response incomplete"), "model response incomplete"},
		{"empty", io.EOF, "empty model response"},
		{"empty_analysis", errors.New("empty analysis output"), "empty analysis output"},
		{"page_budget", errors.New("page budget exhausted"), "page budget exhausted"},
		{"unexpected_eof", fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF), "model stream ended unexpectedly"},
		{"network_unexpected_eof", &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}, "model stream ended unexpectedly"},
		{"response_failed", errors.New("model response failed"), "model response failed"},
		{"sse", fmt.Errorf("wrapped: %w", errors.New("received error event: code=fixture_secret_body message=secret")), "model stream reported an error"},
		{"digest_limit", errors.New("digest input exceeds weekly.max_input_chars (120000)"), "digest input exceeds weekly.max_input_chars"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := ErrorFields(tc.err)
			values := map[string]string{}
			for _, field := range fields {
				values[field.Key] = field.String
				if strings.Contains(field.String, "fixture_secret_body") || strings.Contains(field.String, "secret") || strings.Contains(field.String, "user:pass") {
					t.Fatalf("unsafe field %q = %q", field.Key, field.String)
				}
			}
			if values["reason"] != tc.want || ErrorReason(tc.err) != tc.want {
				t.Fatalf("reason fields = %v, ErrorReason = %q, want %q", values, ErrorReason(tc.err), tc.want)
			}
			if tc.name == "api" && (values["error_type"] != "invalid_request_error" || values["error_code"] != "invalid_api_key" || fields[2].Integer != 403) {
				t.Fatalf("API diagnostics missing safe structured data: %+v", fields)
			}
			if tc.name == "json" && values["error_type"] != "json_syntax_error" {
				t.Fatalf("JSON error type missing: %+v", fields)
			}
			if tc.name == "incomplete" && values["error_type"] != "model_response_incomplete" {
				t.Fatalf("incomplete response type missing: %+v", fields)
			}
			if (tc.name == "unexpected_eof" || tc.name == "network_unexpected_eof") && values["error_type"] != "unexpected_eof" {
				t.Fatalf("unexpected EOF type missing: %+v", fields)
			}
		})
	}
}

func TestErrorFieldsNil(t *testing.T) {
	if fields := ErrorFields(nil); fields != nil || ErrorReason(nil) != "" {
		t.Fatalf("nil error diagnostics = %#v, reason %q", fields, ErrorReason(nil))
	}
}
