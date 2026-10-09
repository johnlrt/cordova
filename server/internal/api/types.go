package api

import (
	"bytes"
	"encoding/json"
	"errors"

	"cordova/server/internal/inference"
)

const ModelID = "cordova-chat-small-v1"

type chatRequest struct {
	Model         string              `json:"model"`
	Messages      []inference.Message `json:"messages"`
	MaxTokens     *int                `json:"max_tokens,omitempty"`
	Stream        bool                `json:"stream,omitempty"`
	StreamOptions *streamOptions      `json:"stream_options,omitempty"`
	Temperature   *float64            `json:"temperature,omitempty"`
	TopP          *float64            `json:"top_p,omitempty"`
	Stop          *stopSequences      `json:"stop,omitempty"`
	Seed          *int64              `json:"seed,omitempty"`
	N             *int                `json:"n,omitempty"`
	User          *string             `json:"user,omitempty"`
}

// stopSequences accepts the C2 string-or-array form while storing one
// normalized list for the inference boundary.
type stopSequences []string

func (s *stopSequences) UnmarshalJSON(data []byte) error {
	value := bytes.TrimSpace(data)
	if len(value) == 0 {
		return errors.New("stop must be a string or array of strings")
	}
	if value[0] == '"' {
		var stop string
		if err := json.Unmarshal(value, &stop); err != nil {
			return err
		}
		*s = stopSequences{stop}
		return nil
	}
	if value[0] != '[' {
		return errors.New("stop must be a string or array of strings")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(value, &entries); err != nil {
		return err
	}
	stops := make(stopSequences, 0, len(entries))
	for _, entry := range entries {
		entry = bytes.TrimSpace(entry)
		if len(entry) == 0 || entry[0] != '"' {
			return errors.New("stop array entries must be strings")
		}
		var stop string
		if err := json.Unmarshal(entry, &stop); err != nil {
			return err
		}
		stops = append(stops, stop)
	}
	*s = stops
	return nil
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type errorResponse struct {
	Error     apiError `json:"error"`
	RequestID string   `json:"request_id"`
}

type delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type streamChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type chunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []streamChoice   `json:"choices"`
	Usage   *inference.Usage `json:"usage"`
}
