package personal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"kagari/internal/textpage"
)

type boundedContextTool struct{ tool.InvokableTool }

func (t boundedContextTool) InvokableRun(ctx context.Context, args string, options ...tool.Option) (string, error) {
	raw, err := t.InvokableTool.InvokableRun(ctx, args, options...)
	if err != nil {
		var changed *textpage.VersionChangedError
		if errors.Is(err, errInvalidProgress) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, textpage.ErrInvalidPage) || errors.Is(err, textpage.ErrVersionRequired) || errors.As(err, &changed) || strings.Contains(err.Error(), "not associated") || strings.Contains(err.Error(), "not explicitly associated") || strings.Contains(err.Error(), "not authorized") || strings.Contains(err.Error(), "checkpoint_conflict") || strings.Contains(err.Error(), "response_item_too_large") {
			value := map[string]any{"error": boundedQueryError(err)}
			if changed != nil {
				value["version"] = changed.Version
			}
			encoded, encodeErr := textpage.EncodeBounded(value, textpage.MaxBytes(ctx), func() bool { return shrinkToolValue(value) })
			return string(encoded), encodeErr
		}
		return "", err
	}
	if len(raw) <= textpage.MaxBytes(ctx) {
		return raw, nil
	}
	var value map[string]any
	if err = json.Unmarshal([]byte(raw), &value); err != nil {
		return "", err
	}
	encoded, err := textpage.EncodeBounded(value, textpage.MaxBytes(ctx), func() bool { return shrinkToolValue(value) })
	if err != nil {
		return `{"error":"response_item_too_large: request a smaller page or read a specific field"}`, nil
	}
	return string(encoded), nil
}
func shrinkToolValue(value map[string]any) bool {
	// Cards retain complete identities; canonical text remains accessible through field readers.
	if _, ok := value["id"]; ok {
		for _, field := range []string{"context", "input", "context_manifest", "result"} {
			if text, ok := value[field].(string); ok && text != "" {
				value[field] = ""
				value["omitted_fields"] = appendField(value["omitted_fields"], field)
				return true
			}
		}
	}
	for _, key := range []string{"task", "run"} {
		if card, ok := value[key].(map[string]any); ok {
			for _, field := range []string{"context", "input", "context_manifest"} {
				if text, ok := card[field].(string); ok && text != "" {
					card[field] = ""
					card["omitted_fields"] = appendField(card["omitted_fields"], field)
					return true
				}
			}
		}
	}
	for _, key := range []string{"tasks", "runs", "items", "memories", "entries", "suggestions", "inputs"} {
		if items, ok := value[key].([]any); ok && len(items) > 1 {
			items = items[:len(items)-1]
			value[key] = items
			value["has_more"] = true
			if item, ok := items[len(items)-1].(map[string]any); ok {
				if id, ok := item["id"]; ok {
					value["next_before_id"] = id
				}
				if seq, ok := item["seq"]; ok {
					value["next_before_id"] = seq
				}
			}
			return true
		}
	}
	for _, field := range []string{"summary", "title", "goal", "error", "reason", "description"} {
		if text, ok := value[field].(string); ok && runeLen(text) > 128 {
			value[field] = truncateRunes(text, runeLen(text)/2)
			value["omitted_fields"] = appendField(value["omitted_fields"], field)
			return true
		}
	}
	for _, key := range []string{"task", "run", "schedule", "suggestion", "progress"} {
		if child, ok := value[key].(map[string]any); ok && shrinkToolValue(child) {
			return true
		}
	}
	return false
}
func appendField(old any, field string) []any {
	fields, _ := old.([]any)
	for _, v := range fields {
		if v == field {
			return fields
		}
	}
	return append(fields, field)
}
func boundContextTools(tools []tool.BaseTool) []tool.BaseTool {
	bounded := make([]tool.BaseTool, len(tools))
	for i, t := range tools {
		bounded[i] = boundedContextTool{t.(tool.InvokableTool)}
	}
	return bounded
}
