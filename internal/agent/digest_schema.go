package agent

import "github.com/openai/openai-go/v3/responses"

func digestFormat() *responses.ResponseTextConfigParam {
	str := map[string]any{"type": "string"}
	array := func(item any) map[string]any { return map[string]any{"type": "array", "items": item} }
	ref := object(map[string]any{"job_id": map[string]any{"type": "integer"}, "source_id": str})
	item := object(map[string]any{
		"entry_ids": array(map[string]any{"type": "integer"}),
		"title":     str, "review": str, "refs": array(ref),
	})
	section := object(map[string]any{"name": str, "items": array(item)})
	return Format("weekly_digest", object(map[string]any{
		"opening": str, "sections": array(section), "closing": str,
	}))
}
