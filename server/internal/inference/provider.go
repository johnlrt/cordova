// Package inference defines the boundary between HTTP and an inference executor.
package inference

import "context"

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Messages   []Message
	MaxTokens  int
	Parameters Parameters
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
