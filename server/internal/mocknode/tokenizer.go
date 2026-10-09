package mocknode

import (
	"context"
	"strings"

	"cordova/server/internal/inference"
)

// SyntheticTokenizer mirrors the M1 mock-node's documented word counts. It is
// suitable only with zero-priced mock traffic and must never back real billing.
type SyntheticTokenizer struct{}

func (SyntheticTokenizer) CountInput(ctx context.Context, _ string, messages []inference.Message) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, message := range messages {
		total += int64(len(strings.Fields(message.Content)))
	}
	return total, nil
}

func (SyntheticTokenizer) CountOutput(ctx context.Context, _ string, text string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return int64(len(strings.Fields(text))), nil
}
