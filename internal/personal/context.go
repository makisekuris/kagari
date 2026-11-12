package personal

import "time"

type ContextSelection struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	Version        int64  `json:"version,omitempty"`
	Reason         string `json:"reason"`
	IncludedBytes  int    `json:"included_bytes"`
	ContentVersion string `json:"content_version,omitempty"`
}

type ContextManifest struct {
	FormatVersion   int                `json:"format_version"`
	Revision        int64              `json:"revision"`
	AssembledAt     time.Time          `json:"assembled_at"`
	Selections      []ContextSelection `json:"selections"`
	ContextWindow   int64              `json:"context_window"`
	MaxOutputTokens int64              `json:"max_output_tokens"`
}
