package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const redactedValue = "[REDACTED]"

var sensitiveKeys = map[string]struct{}{
	"authorization": {}, "cookie": {}, "setcookie": {}, "apikey": {},
	"accesstoken": {}, "refreshtoken": {}, "token": {}, "password": {}, "secret": {},
}

func normalizeKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isSensitiveKey(key string) bool {
	_, ok := sensitiveKeys[normalizeKey(key)]
	return ok
}

func redactValue(value any, secrets []string) any {
	switch value := value.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(value))
		for key, nested := range value {
			baseKey := redactString(key, secrets)
			cleanKey := baseKey
			for suffix := 2; ; suffix++ {
				if _, exists := cleaned[cleanKey]; !exists {
					break
				}
				cleanKey = baseKey + "_" + strconv.Itoa(suffix)
			}
			if isSensitiveKey(key) {
				cleaned[cleanKey] = redactedValue
			} else {
				cleaned[cleanKey] = redactValue(nested, secrets)
			}
		}
		return cleaned
	case []any:
		for i := range value {
			value[i] = redactValue(value[i], secrets)
		}
		return value
	case string:
		var structured any
		if json.Valid([]byte(value)) {
			decoder := json.NewDecoder(bytes.NewBufferString(value))
			decoder.UseNumber()
			_ = decoder.Decode(&structured)
			switch structured.(type) {
			case map[string]any, []any:
				encoded, err := json.Marshal(redactValue(structured, secrets))
				if err == nil {
					return string(encoded)
				}
			}
		}
		return redactString(value, secrets)
	default:
		return value
	}
}

func redactString(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, redactedValue)
		}
	}
	return value
}

func Sanitize(value any, secrets []string) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode value for sanitization: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode value for sanitization: %w", err)
	}
	cleanSecrets := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret != "" {
			cleanSecrets = append(cleanSecrets, secret)
		}
	}
	sort.Slice(cleanSecrets, func(i, j int) bool { return len(cleanSecrets[i]) > len(cleanSecrets[j]) })
	return redactValue(decoded, cleanSecrets), nil
}
