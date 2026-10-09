package inference

import (
	"context"
	"strings"
	"time"
)

// Mock provides deterministic synthetic output for transport development.
// Its word counts are fixture units, never model tokens or billable usage.
type Mock struct {
	Delay time.Duration
}

func (m Mock) Generate(ctx context.Context, req Request, emit func(string) error) (Result, error) {
	words := strings.Fields("[MOCK] Cordova gateway is connected. This is a synthetic response; no AI model is running yet.")
	finish := "stop"
	if len(words) > req.MaxTokens {
		words = words[:req.MaxTokens]
		finish = "length"
	}
	for i, word := range words {
		timer := time.NewTimer(m.Delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Result{}, ctx.Err()
		case <-timer.C:
		}
		if i > 0 {
			word = " " + word
		}
		if err := emit(word); err != nil {
			return Result{}, err
		}
	}
	input := 0
	for _, message := range req.Messages {
		input += len(strings.Fields(message.Content))
	}
	return Result{FinishReason: finish, Usage: Usage{
		PromptTokens: input, CompletionTokens: len(words), TotalTokens: input + len(words),
	}}, nil
}
