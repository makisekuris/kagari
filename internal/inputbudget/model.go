package inputbudget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

var ErrContextLimit = errors.New("inputbudget: request exceeds the context input limit")

type ErrorKind string

const (
	ErrorRequiredInputExceedsLimit ErrorKind = "required_input_exceeds_limit"
	ErrorToolsSchemaExceedsLimit   ErrorKind = "tools_schema_exceeds_limit"
	ErrorHistoryExceedsLimit       ErrorKind = "history_exceeds_limit"
	ErrorUnsupportedContentCount   ErrorKind = "unsupported_content_count"
	ErrorCountInput                ErrorKind = "input_count_failed"
)

type ContextLimitError struct {
	Kind     ErrorKind
	Estimate Estimate
	Limit    int64
}

func (e *ContextLimitError) Error() string {
	return fmt.Sprintf("%s: estimated input %d %s exceeds hard limit %d", e.Kind, e.Estimate.TotalBytes, e.Estimate.CountBasis, e.Limit)
}

func (e *ContextLimitError) Unwrap() error { return ErrContextLimit }

type ModelRecord struct {
	Estimate     Estimate  `json:"estimate"`
	HardLimit    int64     `json:"hard_limit"`
	InputTokens  int       `json:"input_tokens,omitempty"`
	OutputTokens int       `json:"output_tokens,omitempty"`
	TotalTokens  int       `json:"total_tokens,omitempty"`
	Outcome      string    `json:"outcome"`
	ErrorKind    ErrorKind `json:"error_kind,omitempty"`
}

type ModelObserver func(ModelRecord)

type ModelConfig struct {
	Limits       Limits
	OutputSchema json.RawMessage
	// RequiredPrefix identifies messages that cannot be compressed.
	RequiredPrefix func(messages []*schema.AgenticMessage) int
	Observe        ModelObserver
}

// AgenticModel applies the same hard input check to Generate and Stream.
type AgenticModel struct {
	base           model.AgenticModel
	limits         Limits
	outputSchema   json.RawMessage
	requiredPrefix func([]*schema.AgenticMessage) int
	observe        ModelObserver
}

func NewAgenticModel(base model.AgenticModel, cfg ModelConfig) (*AgenticModel, error) {
	if base == nil {
		return nil, errors.New("inputbudget: model is required")
	}
	limits, err := NewLimits(cfg.Limits.Window, cfg.Limits.Output)
	if err != nil {
		return nil, err
	}
	schemaJSON, err := canonicalJSON(cfg.OutputSchema)
	if err != nil {
		return nil, err
	}
	return &AgenticModel{base: base, limits: limits, outputSchema: schemaJSON, requiredPrefix: cfg.RequiredPrefix, observe: cfg.Observe}, nil
}

// WithOutputSchema returns a wrapper over the same raw model with a call-specific schema.
func (m *AgenticModel) WithOutputSchema(outputSchema json.RawMessage) (*AgenticModel, error) {
	return NewAgenticModel(m.base, ModelConfig{Limits: m.limits, OutputSchema: outputSchema, RequiredPrefix: m.requiredPrefix, Observe: m.observe})
}

func (m *AgenticModel) Limits() Limits { return m.limits }

func (m *AgenticModel) Generate(ctx context.Context, messages []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	limits, requestOpts, err := m.requestOptions(opts)
	if err != nil {
		m.report(ModelRecord{HardLimit: m.limits.Hard, Outcome: "rejected", ErrorKind: ErrorCountInput})
		return nil, err
	}
	estimate, err := m.estimate(messages, requestOpts)
	if err != nil {
		m.report(ModelRecord{HardLimit: limits.Hard, Outcome: "rejected", ErrorKind: errorKind(err)})
		return nil, err
	}
	if err = checkLimit(estimate, limits); err != nil {
		m.report(ModelRecord{Estimate: estimate, HardLimit: limits.Hard, Outcome: "rejected", ErrorKind: errorKind(err)})
		return nil, err
	}
	response, err := m.base.Generate(ctx, messages, requestOpts...)
	record := ModelRecord{Estimate: estimate, HardLimit: limits.Hard, Outcome: "sent"}
	if err != nil {
		record.Outcome = "model_error"
	} else if response != nil && response.ResponseMeta != nil && response.ResponseMeta.TokenUsage != nil {
		usage := response.ResponseMeta.TokenUsage
		record.InputTokens, record.OutputTokens, record.TotalTokens = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
	}
	m.report(record)
	return response, err
}

func (m *AgenticModel) Stream(ctx context.Context, messages []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	limits, requestOpts, err := m.requestOptions(opts)
	if err != nil {
		m.report(ModelRecord{HardLimit: m.limits.Hard, Outcome: "rejected", ErrorKind: ErrorCountInput})
		return nil, err
	}
	estimate, err := m.estimate(messages, requestOpts)
	if err != nil {
		m.report(ModelRecord{HardLimit: limits.Hard, Outcome: "rejected", ErrorKind: errorKind(err)})
		return nil, err
	}
	if err = checkLimit(estimate, limits); err != nil {
		m.report(ModelRecord{Estimate: estimate, HardLimit: limits.Hard, Outcome: "rejected", ErrorKind: errorKind(err)})
		return nil, err
	}
	source, err := m.base.Stream(ctx, messages, requestOpts...)
	if err != nil {
		m.report(ModelRecord{Estimate: estimate, HardLimit: limits.Hard, Outcome: "model_error"})
		return nil, err
	}
	if source == nil {
		err = errors.New("inputbudget: model returned a nil stream")
		m.report(ModelRecord{Estimate: estimate, HardLimit: limits.Hard, Outcome: "model_error"})
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.AgenticMessage](1)
	go func() {
		defer writer.Close()
		defer source.Close()
		record := ModelRecord{Estimate: estimate, HardLimit: limits.Hard, Outcome: "sent"}
		for {
			chunk, recvErr := source.Recv()
			if errors.Is(recvErr, io.EOF) {
				m.report(record)
				return
			}
			if recvErr != nil {
				record.Outcome = "stream_error"
				m.report(record)
				writer.Send(nil, recvErr)
				return
			}
			if chunk != nil && chunk.ResponseMeta != nil && chunk.ResponseMeta.TokenUsage != nil {
				usage := chunk.ResponseMeta.TokenUsage
				record.InputTokens, record.OutputTokens, record.TotalTokens = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
			}
			if writer.Send(chunk, nil) {
				record.Outcome = "stream_closed"
				m.report(record)
				return
			}
		}
	}()
	return reader, nil
}

func (m *AgenticModel) estimate(messages []*schema.AgenticMessage, opts []model.Option) (Estimate, error) {
	common := model.GetCommonOptions(nil, opts...)
	prefix := 0
	if m.requiredPrefix != nil {
		prefix = m.requiredPrefix(messages)
	}
	return estimateWithOptions(messages, common, m.outputSchema, prefix)
}

func (m *AgenticModel) requestOptions(opts []model.Option) (Limits, []model.Option, error) {
	common := model.GetCommonOptions(nil, opts...)
	output := m.limits.Output
	if common.MaxTokens != nil {
		output = int64(*common.MaxTokens)
	} else {
		opts = append(append([]model.Option(nil), opts...), model.WithMaxTokens(int(output)))
	}
	limits, err := NewLimits(m.limits.Window, output)
	return limits, opts, err
}

func checkLimit(estimate Estimate, limits Limits) error {
	if estimate.TotalBytes <= limits.Hard {
		return nil
	}
	kind := ErrorHistoryExceedsLimit
	schemaBytes := estimate.ToolsBytes + estimate.OutputSchemaBytes
	if schemaBytes >= limits.Hard {
		kind = ErrorToolsSchemaExceedsLimit
	} else if estimate.RequiredBytes > limits.Hard-schemaBytes {
		kind = ErrorRequiredInputExceedsLimit
	}
	return &ContextLimitError{Kind: kind, Estimate: estimate, Limit: limits.Hard}
}

func (m *AgenticModel) report(record ModelRecord) {
	if m.observe != nil {
		m.observe(record)
	}
}

func errorKind(err error) ErrorKind {
	var unsupported *UnsupportedContentError
	if errors.As(err, &unsupported) {
		return ErrorUnsupportedContentCount
	}
	var limit *ContextLimitError
	if errors.As(err, &limit) {
		return limit.Kind
	}
	return ErrorCountInput
}
