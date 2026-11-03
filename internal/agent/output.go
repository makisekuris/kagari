package agent

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"kagari/internal/domain"
)

var (
	errInvalidAnalysisOutput = errors.New("invalid analysis output")
	errModelResponseRefused  = errors.New("model response refused")
)

func decodeAnalysisOutput(raw string) (domain.ResultKind, string, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	token, err := dec.Token()
	delim, ok := token.(json.Delim)
	if err != nil || !ok || delim != '{' {
		return "", "", errInvalidAnalysisOutput
	}
	fields := make(map[string]json.RawMessage, 2)
	for dec.More() {

		token, err = dec.Token()
		name, ok := token.(string)
		if err != nil || !ok || (name != "kind" && name != "body") {
			return "", "", errInvalidAnalysisOutput
		}
		if _, exists := fields[name]; exists {
			return "", "", errInvalidAnalysisOutput
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return "", "", errInvalidAnalysisOutput
		}
		fields[name] = value
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim('}') || len(fields) != 2 {
		return "", "", errInvalidAnalysisOutput
	}
	if _, err = dec.Token(); err != io.EOF {
		return "", "", errInvalidAnalysisOutput
	}
	var result struct {
		Kind domain.ResultKind `json:"kind"`
		Body string            `json:"body"`
	}
	if json.Unmarshal(fields["kind"], &result.Kind) != nil || json.Unmarshal(fields["body"], &result.Body) != nil {
		return "", "", errInvalidAnalysisOutput
	}
	if (result.Kind != domain.ResultKindAnalysis && result.Kind != domain.ResultKindChat) || strings.TrimSpace(result.Body) == "" {
		return "", "", errInvalidAnalysisOutput
	}
	return result.Kind, result.Body, nil
}
