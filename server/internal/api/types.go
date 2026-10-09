package api

import "cordova/server/internal/inference"

const ModelID = "cordova-chat-small-v1"

type chatRequest struct {
	Model         string              `json:"model"`
	Messages      []inference.Message `json:"messages"`
	MaxTokens     *int                `json:"max_tokens,omitempty"`
	Stream        bool                `json:"stream,omitempty"`
	StreamOptions *streamOptions      `json:"stream_options,omitempty"`
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
