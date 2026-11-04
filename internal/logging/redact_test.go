package logging

import (
	"encoding/json"
	"strings"
	"testing"
)

// This protects exact-key masking, recursive secret replacement, and lossless parsing of tool JSON.
func TestRedactValueCoversSensitiveKeysSecretsAndEmbeddedJSON(t *testing.T) {
	input := map[string]any{
		"Authorization":   "bearer auth-value",
		"set-cookie":      "cookie-value",
		"api_key":         "key-value",
		"access-token":    "access-value",
		"refresh_token":   "refresh-value",
		"Token":           "token-value",
		"password":        "password-value",
		"secret":          "secret-value",
		"input_tokens":    json.Number("9007199254740993"),
		"tool memory key": "ordinary metadata",
		"nested": []any{
			map[string]any{"message": "prefix inline-secret suffix", "private-name": "known key", "[REDACTED]": "existing safe key"},
			`{"token":"inline-secret","id":9007199254740993,"input_tokens":64}`,
		},
	}
	cleaned := redactValue(input, []string{"", "inline-secret", "private-name"}).(map[string]any)
	for _, key := range []string{"Authorization", "set-cookie", "api_key", "access-token", "refresh_token", "Token", "password", "secret"} {
		if cleaned[key] != redactedValue {
			t.Errorf("%s = %#v, want redacted", key, cleaned[key])
		}
	}
	if cleaned["input_tokens"] != json.Number("9007199254740993") || cleaned["tool memory key"] != "ordinary metadata" {
		t.Fatalf("safe metadata changed: %#v", cleaned)
	}
	nested := cleaned["nested"].([]any)
	message := nested[0].(map[string]any)["message"].(string)
	if message != "prefix [REDACTED] suffix" {
		t.Fatalf("configured secret in prose = %q", message)
	}
	keyed := nested[0].(map[string]any)
	if len(keyed) != 3 || keyed["[REDACTED]"] == nil {
		t.Fatalf("configured secret in object key was not removed: %#v", nested[0])
	}
	if keyed["[REDACTED]"] != "known key" && keyed["[REDACTED]"] != "existing safe key" || keyed["[REDACTED]_2"] == nil {
		t.Fatalf("redacting an object key lost colliding metadata: %#v", keyed)
	}
	embedded := nested[1].(string)
	if !strings.Contains(embedded, `"id":9007199254740993`) || strings.Contains(embedded, "inline-secret") || !strings.Contains(embedded, `"token":"[REDACTED]"`) {
		t.Fatalf("structured JSON string lost precision or secret: %s", embedded)
	}
}

// This protects the exported sanitizer contract for typed callers and shared values.
func TestSanitizeRoundTripsTypedValuesWithoutMutatingInput(t *testing.T) {
	input := map[string]any{
		"top-secret": "top-secret",
		"nested":     map[string]any{"text": "known-secret", "input_tokens": uint64(9007199254740993)},
	}
	cleanedValue, err := Sanitize(input, []string{"", "top-secret", "known-secret"})
	if err != nil {
		t.Fatal(err)
	}
	cleaned := cleanedValue.(map[string]any)
	if cleaned["[REDACTED]"] != redactedValue {
		t.Fatalf("cleaned keys/values = %#v", cleaned)
	}
	nested := cleaned["nested"].(map[string]any)
	if nested["text"] != redactedValue || nested["input_tokens"] != json.Number("9007199254740993") {
		t.Fatalf("cleaned nested value = %#v", nested)
	}
	if input["top-secret"] != "top-secret" || input["nested"].(map[string]any)["text"] != "known-secret" {
		t.Fatalf("Sanitize mutated the caller's input: %#v", input)
	}
	if _, err := Sanitize(make(chan int), nil); err == nil {
		t.Fatal("unsupported typed value should return a marshal error")
	}
}
