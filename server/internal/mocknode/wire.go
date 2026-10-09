package mocknode

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const maxFrameBytes = 1 << 20
const maxDeltaBytes = 16 << 10

type request struct {
	Version   int       `json:"v"`
	Type      string    `json:"type"`
	RequestID string    `json:"request_id"`
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type frame struct {
	Version      int    `json:"v"`
	Type         string `json:"type"`
	RequestID    string `json:"request_id"`
	Sequence     int    `json:"sequence"`
	Text         string `json:"text,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	Usage        *usage `json:"usage,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func decode(data []byte, target any) error {
	if len(data) == 0 || len(data) > maxFrameBytes || !json.Valid(data) || hasDuplicateKeys(data) {
		return errors.New("invalid frame")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func hasDuplicateKeys(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if walk(decoder, 0) != nil {
		return true
	}
	_, err := decoder.Token()
	return err != io.EOF
}

func walk(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("nesting limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("object key required")
			}
			if _, exists := seen[name]; exists {
				return errors.New("duplicate key")
			}
			seen[name] = struct{}{}
			if err := walk(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := walk(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}
