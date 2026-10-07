package logging

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/openai/openai-go/v3"
	"go.uber.org/zap"
)

const noReadableContent = "no readable content; forward the discussion text or submit the original article URL"

type errorInfo struct {
	reason, kind, code string
	status             int
	offset             int64
}

func ErrorFields(err error) []zap.Field {
	if err == nil {
		return nil
	}
	info := describeError(err)
	fields := []zap.Field{zap.String("reason", info.reason), zap.String("error_type", info.kind)}
	if info.status > 0 {
		fields = append(fields, zap.Int("http_status", info.status))
	}
	if info.code != "" {
		fields = append(fields, zap.String("error_code", info.code))
	}
	if info.offset > 0 {
		fields = append(fields, zap.Int64("json_offset", info.offset))
	}
	return fields
}

func ErrorReason(err error) string {
	if err == nil {
		return ""
	}
	return describeError(err).reason
}

func describeError(err error) errorInfo {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		kind := safeIdentifier(apiErr.Type)
		if kind == "" {
			kind = "model_api_error"
		}
		return errorInfo{reason: "model request failed", kind: kind, code: safeIdentifier(apiErr.Code), status: apiErr.StatusCode}
	}
	if errors.Is(err, context.Canceled) {
		return errorInfo{reason: "operation canceled", kind: "context_canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errorInfo{reason: "operation deadline exceeded", kind: "deadline_exceeded"}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return errorInfo{reason: "model stream ended unexpectedly", kind: "unexpected_eof"}
	}
	if errors.Is(err, io.EOF) {
		return errorInfo{reason: "empty model response", kind: "empty_response"}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		kind := "dns_error"
		if dnsErr.IsNotFound {
			kind = "dns_not_found"
		} else if dnsErr.IsTimeout {
			kind = "dns_timeout"
		}
		return errorInfo{reason: "DNS lookup failed", kind: kind}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		reason := map[string]string{
			"connect": "network connection failed", "dial": "network connection failed",
			"read": "network read failed", "write": "network write failed",
			"accept": "network accept failed", "listen": "network listen failed",
		}[opErr.Op]
		if reason == "" {
			reason = "network operation failed"
		}
		return errorInfo{reason: reason, kind: "network_error"}
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return errorInfo{reason: "invalid JSON response", kind: "json_syntax_error", offset: syntaxErr.Offset}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return errorInfo{reason: "URL request failed", kind: "url_error"}
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		message := current.Error()
		switch message {
		case noReadableContent:
			return errorInfo{reason: "no readable content", kind: "read_error"}
		case "empty analysis output", "empty digest output":
			return errorInfo{reason: message, kind: "empty_response"}
		case "model response failed":
			return errorInfo{reason: message, kind: "model_response_error"}
		case "model response incomplete":
			return errorInfo{reason: message, kind: "model_response_incomplete"}
		case "model response refused":
			return errorInfo{reason: message, kind: "model_response_refused"}
		case "invalid analysis output":
			return errorInfo{reason: message, kind: "analysis_output_error"}
		case "model.base_url, model.name and model.api_key are required":
			return errorInfo{reason: "model configuration incomplete", kind: "configuration_error"}
		case "unsupported digest version", "digest snapshot does not match request":
			return errorInfo{reason: message, kind: "digest_snapshot_error"}
		case "page budget exhausted":
			return errorInfo{reason: message, kind: "reading_policy_error"}
		}
		switch {
		case strings.HasPrefix(message, "digest input exceeds weekly.max_input_chars"):
			return errorInfo{reason: "digest input exceeds weekly.max_input_chars", kind: "digest_input_limit"}
		case strings.HasPrefix(message, "received error event:"):
			return errorInfo{reason: "model stream reported an error", kind: "model_stream_error"}
		}
	}
	return errorInfo{reason: "operation failed", kind: "unknown_error"}
}

func safeIdentifier(value string) string {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return ""
	}
	for _, r := range value[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return ""
		}
	}
	return value
}
