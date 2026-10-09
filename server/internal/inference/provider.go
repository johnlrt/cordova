// Package inference defines the boundary between HTTP and an inference executor.
package inference

import "context"

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Messages  []Message
	MaxTokens int
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
// or retain request content after the call. Real tokenization arrives in stage 2.
type Provider interface {
	Generate(context.Context, Request, func(string) error) (Result, error)
}
