package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"kagari/internal/personal"
)

type formatValidationError struct {
	reason     string
	diagnostic string
}

func (e *formatValidationError) Error() string { return e.diagnostic }

func decodePersonalTurnResponse(raw string) (personalTurnResponse, error) {
	fields, err := strictJSONObject(raw, "text", "reply_kind", "attachments")
	if err != nil {
		return personalTurnResponse{}, fmt.Errorf("invalid personal response: %w", err)
	}
	var text *string
	var kind personal.ReplyKind
	if err := json.Unmarshal(fields["text"], &text); err != nil || text == nil {
		return personalTurnResponse{}, &formatValidationError{reason: "回复文本（text）格式无效", diagnostic: "invalid personal response text"}
	}
	if err := json.Unmarshal(fields["reply_kind"], &kind); err != nil || (kind != personal.ReplySilent && kind != personal.ReplyReceipt && kind != personal.ReplyContent) {
		return personalTurnResponse{}, &formatValidationError{reason: "回复类型（reply_kind）无效", diagnostic: "invalid personal response reply kind"}
	}
	var attachments []string
	if err := json.Unmarshal(fields["attachments"], &attachments); err != nil || attachments == nil {
		return personalTurnResponse{}, &formatValidationError{reason: "附件列表（attachments）格式无效", diagnostic: "invalid personal response attachments"}
	}
	for _, id := range attachments {
		if strings.TrimSpace(id) == "" {
			return personalTurnResponse{}, &formatValidationError{reason: "附件 ID 不能为空", diagnostic: "blank attachment ID"}
		}
	}
	if kind != personal.ReplySilent && strings.TrimSpace(*text) == "" && len(attachments) == 0 {
		return personalTurnResponse{}, &formatValidationError{reason: "回复缺少正文和附件", diagnostic: "personal response text is blank"}
	}
	if kind != personal.ReplyContent && len(attachments) > 0 {
		return personalTurnResponse{}, &formatValidationError{reason: "只有内容回复可以包含附件", diagnostic: "only a content response can include attachments"}
	}
	return personalTurnResponse{Text: *text, ReplyKind: kind, Attachments: attachments}, nil
}

func strictJSONObject(raw string, expected ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	token, err := dec.Token()
	delim, ok := token.(json.Delim)
	if err != nil || !ok || delim != '{' {
		return nil, &formatValidationError{reason: "模型没有返回 JSON 对象", diagnostic: "expected object"}
	}
	allowed := make(map[string]struct{}, len(expected))
	for _, key := range expected {
		allowed[key] = struct{}{}
	}
	fields := make(map[string]json.RawMessage, len(expected))
	for dec.More() {
		token, err = dec.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, &formatValidationError{reason: "JSON 对象结构无效", diagnostic: "invalid object key"}
		}
		if _, ok := allowed[key]; !ok {
			return nil, &formatValidationError{reason: "包含未知字段", diagnostic: fmt.Sprintf("unexpected field %q", key)}
		}
		if _, exists := fields[key]; exists {
			return nil, &formatValidationError{reason: "包含重复字段", diagnostic: fmt.Sprintf("duplicate field %q", key)}
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, &formatValidationError{reason: "JSON 对象结构无效", diagnostic: "invalid field value"}
		}
		fields[key] = value
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim('}') {
		return nil, &formatValidationError{reason: "JSON 对象结构无效", diagnostic: "missing or malformed fields"}
	}
	for _, key := range expected {
		if _, ok := fields[key]; ok {
			continue
		}
		return nil, &formatValidationError{reason: "缺少" + formatFieldLabel(key) + "字段", diagnostic: "missing or malformed fields"}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &formatValidationError{reason: "JSON 后包含多余内容", diagnostic: "trailing JSON data"}
	}
	return fields, nil
}

func formatFieldLabel(field string) string {
	switch field {
	case "text":
		return "回复文本（text）"
	case "reply_kind":
		return "回复类型（reply_kind）"
	case "attachments":
		return "附件列表（attachments）"
	default:
		return field
	}
}
