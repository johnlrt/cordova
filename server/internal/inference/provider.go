// Package inference defines the boundary between HTTP and an inference executor.
package inference

import (
	"context"
	"errors"
)

var ErrContextExceeded = errors.New("model context limit exceeded")

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Messages       []Message  `json:"messages"`
	InputTokenIDs  []uint32   `json:"input_token_ids,omitempty"`
	InputTokens    int64      `json:"input_tokens,omitempty"`
	CatalogVersion string     `json:"catalog_version,omitempty"`
	MaxTokens      int        `json:"max_tokens"`
	Parameters     Parameters `json:"parameters"`
}

// PreparedInput is the trusted tokenizer's exact representation of one prompt.
// Token IDs remain request-owned memory and are passed only to the node runner.
type PreparedInput struct {
	TokenIDs       []uint32
	TokenCount     int64
	ContextTokens  uint32
	CatalogVersion string
}

// Parameters mirrors the validated generation controls in PLAN.md C2/C3.
// Executors may apply them once the real model path is admitted.
type Parameters struct {
	Temperature float64
	TopP        float64
	Stop        []string
	Seed        *int64
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Result struct {
	FinishReason string
	Usage        Usage
}

// Generate must observe cancellation and propagate emit errors. It must not log
// or retain request content after the call. Real tokenization arrives after
// the stage-2 model and tokenizer admission checks pass.
type Provider interface {
	Generate(context.Context, Request, func(string) error) (Result, error)
}
