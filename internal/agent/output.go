package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"kagari/internal/domain"
)

func decode(raw string, target any) error {
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected a single JSON object")
	}
	return nil
}

// usable 不把所有 incomplete 视为证据：只有明确截断且保留正文的片段才可引用，
// 发布时仍必须披露截断；supplied 只能支持用户提交的讨论内容。
func usable(s domain.Source) bool {
	return (s.Status == "ok" || s.Status == "supplied" || (s.Status == "incomplete" && s.Truncated)) && strings.TrimSpace(s.Content) != ""
}

// validate 在 endpoint 的 JSON Schema 约束之外再检查业务引用，拒绝模型编造来源 ID。
func validate(a domain.Analysis, sources []domain.Source, categories []string) error {
	if strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Overview) == "" || len(a.Summary) == 0 || len(a.Tags) > 5 {
		return errors.New("analysis lacks title, overview or summary, or has too many tags")
	}
	categoryOK := false
	for _, c := range categories {
		if c == a.Category {
			categoryOK = true
		}
	}
	if !categoryOK {
		return errors.New("analysis returned an unknown category")
	}
	ids := map[string]bool{}
	for _, s := range sources {
		if usable(s) {
			ids[s.ID] = true
		}
	}
	for _, group := range [][]domain.Claim{a.Summary, a.Discussion, a.Evaluation} {
		if len(group) > 20 {
			return errors.New("too many claims")
		}
		for _, claim := range group {
			if strings.TrimSpace(claim.Text) == "" || len(claim.SourceIDs) == 0 {
				return errors.New("claim lacks text or sources")
			}
			for _, id := range claim.SourceIDs {
				if !ids[id] {
					return fmt.Errorf("claim cites unread source %q", id)
				}
			}
		}
	}
	return nil
}

func analysisFormat(categories []string) *responses.ResponseTextConfigParam {
	str := map[string]any{"type": "string"}
	array := func(item any) map[string]any { return map[string]any{"type": "array", "items": item} }
	claim := object(map[string]any{"text": str, "source_ids": array(str)})
	return Format("reading_analysis", object(map[string]any{
		"title": str, "overview": str, "summary": array(claim), "discussion": array(claim), "evaluation": array(claim),
		"category": map[string]any{"type": "string", "enum": categories}, "tags": array(str), "relevance": str, "uncertainties": array(str),
	}))
}

func object(properties map[string]any) map[string]any {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	return map[string]any{"type": "object", "properties": properties, "required": keys, "additionalProperties": false}
}

func Format(name string, shape map[string]any) *responses.ResponseTextConfigParam {
	return &responses.ResponseTextConfigParam{Format: responses.ResponseFormatTextConfigUnionParam{OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{Name: name, Schema: shape, Strict: openai.Bool(true)}}}
}
