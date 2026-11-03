package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func (a *MainAgent) ReviewMemory(ctx context.Context, input MemoryReviewInput) (MemoryReview, error) {
	if a == nil || a.Model == nil {
		return MemoryReview{}, errors.New("memory reviewer is not configured")
	}
	if a.Timeout <= 0 {
		return MemoryReview{}, errors.New("memory reviewer timeout must be positive")
	}
	if input.Event.Kind != EventUser || input.Event.ID <= 0 {
		return MemoryReview{}, errors.New("memory review requires a persisted direct user event")
	}
	if strings.TrimSpace(input.Event.Text) == "" {
		return MemoryReview{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()

	input = boundedMemoryReviewInput(input)
	agent, err := adk.NewTypedChatModelAgent(ctx, &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: "memory_reviewer", Description: "Extract only durable personal facts directly supported by the current user event.",
		Instruction: memoryReviewInstruction, Model: a.Model, MaxIterations: 1,
	})
	if err != nil {
		return MemoryReview{}, fmt.Errorf("create memory reviewer: %w", err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return MemoryReview{}, fmt.Errorf("encode memory review input: %w", err)
	}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: agent})
	output, err := consumePersonalEvents(runner.Query(ctx, string(encoded), adk.WithChatModelOptions([]model.Option{memoryReviewOutputOption()})))
	if err != nil {
		return MemoryReview{}, err
	}
	review, err := decodeMemoryReview(output)
	if err != nil {
		return MemoryReview{}, err
	}
	if err := validateMemoryReview(input, &review); err != nil {
		return MemoryReview{}, err
	}
	return review, nil
}

func boundedMemoryReviewInput(input MemoryReviewInput) MemoryReviewInput {
	input.Event.Text = limitRunes(input.Event.Text, maxEventRunes)
	input.Memories = append([]Memory(nil), input.Memories...)
	if len(input.Memories) > maxMemories {
		input.Memories = input.Memories[:maxMemories]
	}
	for i := range input.Memories {
		input.Memories[i].Key = limitRunes(input.Memories[i].Key, 300)
		input.Memories[i].Value = limitRunes(input.Memories[i].Value, maxMemoryRunes)
		if input.Memories[i].Deleted {
			input.Memories[i].Value = ""
		}
	}
	return input
}

func memoryReviewOutputOption() model.Option {
	return personalJSONOutputOption("personal_memory_review", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"changes": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"operation":        map[string]any{"type": "string", "enum": []string{"put"}},
					"key":              map[string]any{"type": "string"},
					"value":            map[string]any{"type": "string"},
					"expected_version": map[string]any{"type": "integer"},
					"evidence":         map[string]any{"type": "string"},
				},
				"required":             []string{"operation", "key", "value", "expected_version", "evidence"},
				"additionalProperties": false,
			}},
			"question": map[string]any{"type": "string"},
		},
		"required":             []string{"changes", "question"},
		"additionalProperties": false,
	})
}

func decodeMemoryReview(raw string) (MemoryReview, error) {
	fields, err := strictJSONObject(raw, "changes", "question")
	if err != nil {
		return MemoryReview{}, fmt.Errorf("invalid memory review: %w", err)
	}
	var rawChanges []json.RawMessage
	if !strings.HasPrefix(strings.TrimSpace(string(fields["changes"])), "[") || json.Unmarshal(fields["changes"], &rawChanges) != nil {
		return MemoryReview{}, errors.New("invalid memory review changes")
	}
	var question *string
	if err := json.Unmarshal(fields["question"], &question); err != nil || question == nil {
		return MemoryReview{}, errors.New("invalid memory review question")
	}
	if len(rawChanges) > 20 {
		return MemoryReview{}, errors.New("memory review has too many changes")
	}
	review := MemoryReview{Changes: make([]MemoryChange, 0, len(rawChanges)), Question: *question}
	for _, rawChange := range rawChanges {
		changeFields, err := strictJSONObject(string(rawChange), "operation", "key", "value", "expected_version", "evidence")
		if err != nil {
			return MemoryReview{}, fmt.Errorf("invalid memory change: %w", err)
		}
		var change MemoryChange
		if err := json.Unmarshal(rawChange, &change); err != nil {
			return MemoryReview{}, fmt.Errorf("invalid memory change: %w", err)
		}
		var operation, key, value, evidence *string
		var version *int64
		for field, target := range map[string]**string{
			"operation": &operation, "key": &key, "value": &value, "evidence": &evidence,
		} {
			if err := json.Unmarshal(changeFields[field], target); err != nil || *target == nil {
				return MemoryReview{}, fmt.Errorf("invalid memory change %s", field)
			}
		}
		if err := json.Unmarshal(changeFields["expected_version"], &version); err != nil || version == nil {
			return MemoryReview{}, errors.New("invalid memory change expected_version")
		}
		if *operation != "put" || strings.TrimSpace(*key) == "" || strings.TrimSpace(*value) == "" || strings.TrimSpace(*evidence) == "" || *version < 0 {
			return MemoryReview{}, errors.New("invalid memory change fields")
		}
		review.Changes = append(review.Changes, change)
	}
	return review, nil
}

func validateMemoryReview(input MemoryReviewInput, review *MemoryReview) error {
	if len([]rune(review.Question)) > 1000 {
		return errors.New("memory review question is too long")
	}
	if strings.TrimSpace(review.Question) != "" && len(review.Changes) > 0 {
		return errors.New("memory review cannot apply changes while asking for clarification")
	}
	existing := make(map[string]Memory, len(input.Memories))
	for _, memory := range input.Memories {
		key := normalizedMemoryKey(memory.Key)
		if key == "" {
			continue
		}
		if _, exists := existing[key]; exists {
			return errors.New("memory review input contains duplicate normalized keys")
		}
		existing[key] = memory
	}
	seen := make(map[string]bool, len(review.Changes))
	for i := range review.Changes {
		change := &review.Changes[i]
		if change.Operation != "put" || len([]rune(change.Key)) > 120 || len([]rune(change.Value)) > 2000 || len([]rune(change.Evidence)) > 2000 {
			return errors.New("memory review change exceeds allowed fields")
		}
		if !strings.Contains(input.Event.Text, change.Evidence) {
			return errors.New("memory review evidence is not an exact quote from the current user event")
		}
		key := normalizedMemoryKey(change.Key)
		if key == "" || seen[key] {
			return errors.New("memory review contains an empty or duplicate key")
		}
		seen[key] = true
		if memory, ok := existing[key]; ok {
			if change.ExpectedVersion != memory.Version {
				return errors.New("memory review expected version does not match the current memory")
			}
			change.Key = memory.Key
		} else {
			if change.ExpectedVersion != 0 {
				return errors.New("new memory key must use expected version zero")
			}
			if change.Key != strings.TrimSpace(change.Key) || change.Key != normalizedMemoryKey(change.Key) {
				return errors.New("new memory key must be normalized")
			}
		}
	}
	return nil
}

func normalizedMemoryKey(key string) string { return strings.ToLower(strings.TrimSpace(key)) }
