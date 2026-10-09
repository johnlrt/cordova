package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

func decodeRequest(w http.ResponseWriter, r *http.Request) (chatRequest, string, string) {
	var req chatRequest
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return req, "invalid_request", "Content-Type must be application/json."
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !utf8.Valid(body) {
		return req, "invalid_request", "Body must be valid UTF-8 JSON no larger than 1 MiB."
	}
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return req, "invalid_request", "Malformed JSON, duplicate key or excessive nesting."
	}
	if err := requiredTypes(body); err != nil {
		return req, "invalid_request", "Required fields must have the documented JSON types, without null values."
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		code := "invalid_request"
		if strings.HasPrefix(err.Error(), "json: unknown field") {
			code = "unsupported_parameter"
		}
		return req, code, "Request does not match the documented request schema."
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return req, "invalid_request", "Send exactly one JSON object."
	}
	if len(req.Messages) == 0 || len(req.Messages) > 128 {
		return req, "invalid_request", "Provide 1 to 128 text messages."
	}
	userTurn := false
	for _, msg := range req.Messages {
		if msg.Role != "system" && msg.Role != "user" && msg.Role != "assistant" {
			return req, "invalid_request", "Supported message roles are system, user and assistant."
		}
		if msg.Role == "user" {
			userTurn = true
		}
		if len(msg.Content) > 256<<10 {
			return req, "invalid_request", "Message content exceeds 256 KiB."
		}
	}
	if !userTurn || req.Model == "" {
		return req, "invalid_request", "A model and at least one user message are required."
	}
	if req.MaxTokens != nil && (*req.MaxTokens < 1 || *req.MaxTokens > 2048) {
		return req, "invalid_request", "max_tokens must be between 1 and 2048."
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return req, "invalid_request", "temperature must be between 0 and 2."
	}
	if req.TopP != nil && (*req.TopP <= 0 || *req.TopP > 1) {
		return req, "invalid_request", "top_p must be greater than 0 and no greater than 1."
	}
	if req.Stop != nil {
		if len(*req.Stop) > 4 {
			return req, "invalid_request", "stop accepts at most 4 strings."
		}
		for _, stop := range *req.Stop {
			if utf8.RuneCountInString(stop) > 128 {
				return req, "invalid_request", "stop strings must be no longer than 128 characters."
			}
		}
	}
	if req.Seed != nil && (*req.Seed < 0 || *req.Seed > 2147483647) {
		return req, "invalid_request", "seed must be between 0 and 2147483647."
	}
	if req.N != nil && *req.N != 1 {
		return req, "unsupported_parameter", "n must be 1."
	}
	if req.StreamOptions != nil && !req.Stream {
		return req, "invalid_request", "stream_options requires stream=true."
	}
	return req, "", ""
}

func requiredTypes(body []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return errors.New("object required")
	}
	for _, key := range []string{"model", "messages"} {
		if len(object[key]) == 0 || bytes.Equal(object[key], []byte("null")) {
			return errors.New("required field")
		}
	}
	for _, key := range []string{"max_tokens", "stream", "stream_options", "temperature", "top_p", "stop", "seed", "n", "user"} {
		if bytes.Equal(bytes.TrimSpace(object[key]), []byte("null")) {
			return errors.New("null option")
		}
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(object["messages"], &messages); err != nil {
		return err
	}
	for _, message := range messages {
		for _, key := range []string{"role", "content"} {
			value := message[key]
			if len(value) == 0 || value[0] != '"' {
				return errors.New("message string required")
			}
		}
	}
	if raw, ok := object["stream_options"]; ok {
		var options map[string]json.RawMessage
		if err := json.Unmarshal(raw, &options); err != nil || options == nil {
			return errors.New("stream options object required")
		}
		value := string(options["include_usage"])
		if value != "true" && value != "false" {
			return errors.New("include_usage boolean required")
		}
	}
	return nil
}

// Reject duplicate object keys before typed decoding so different consumers
// cannot interpret an ambiguous body differently. Body size is already bounded.
func uniqueJSON(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("nesting limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errors.New("duplicate or invalid key")
			}
			keys[name] = true
		}
		if err := uniqueJSON(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}
