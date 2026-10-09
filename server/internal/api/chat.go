package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cordova/server/internal/inference"
)

func (s *Server) chat(w http.ResponseWriter, r *http.Request, requestID string) {
	req, code, message := decodeRequest(w, r)
	if code != "" {
		fail(w, 400, requestID, code, message)
		return
	}
	if req.Model != ModelID {
		fail(w, 404, requestID, "model_not_found", "Unknown catalog model.")
		return
	}
	limit := 512
	if req.MaxTokens != nil {
		limit = *req.MaxTokens
	}
	parameters := inference.Parameters{Temperature: 0.7, TopP: 0.95}
	if req.Temperature != nil {
		parameters.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		parameters.TopP = *req.TopP
	}
	if req.Stop != nil {
		parameters.Stop = append([]string(nil), (*req.Stop)...)
	}
	if req.Seed != nil {
		seed := *req.Seed
		parameters.Seed = &seed
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	input := inference.Request{Messages: req.Messages, MaxTokens: limit, Parameters: parameters}
	completionID := "chatcmpl-" + requestID
	created := time.Now().Unix()
	if !req.Stream {
		var output strings.Builder
		result, err := s.provider.Generate(ctx, input, func(text string) error {
			if output.Len()+len(text) > 1<<20 {
				return errors.New("response limit")
			}
			output.WriteString(text)
			return ctx.Err()
		})
		if err != nil {
			fail(w, 503, requestID, "node_failed", "The test executor could not complete the request.")
			return
		}
		writeJSON(w, 200, map[string]any{
			"id": completionID, "object": "chat.completion", "created": created, "model": ModelID,
			"choices": []any{map[string]any{"index": 0, "message": inference.Message{Role: "assistant", Content: output.String()}, "finish_reason": result.FinishReason}},
			"usage":   result.Usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	base := chunk{ID: completionID, Object: "chat.completion.chunk", Created: created, Model: ModelID}
	base.Choices = []streamChoice{{Index: 0, Delta: delta{Role: "assistant"}}}
	if err := event(w, base); err != nil {
		return
	}
	bytesSent := 0
	result, err := s.provider.Generate(ctx, input, func(text string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		bytesSent += len(text)
		if bytesSent > 1<<20 || len(text) > 16<<10 {
			return errors.New("response limit")
		}
		base.Choices = []streamChoice{{Index: 0, Delta: delta{Content: text}}}
		return event(w, base)
	})
	if err != nil {
		if r.Context().Err() == nil {
			_ = event(w, errorBody(requestID, "node_failed", "The test executor stopped before completion.", 503))
		}
		return
	}
	base.Choices = []streamChoice{{Index: 0, Delta: delta{}, FinishReason: &result.FinishReason}}
	if err := event(w, base); err != nil {
		return
	}
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		base.Choices = []streamChoice{}
		base.Usage = &result.Usage
		if err := event(w, base); err != nil {
			return
		}
	}
	_ = writeEvent(w, []byte("[DONE]"))
}

func event(w http.ResponseWriter, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeEvent(w, data)
}

func writeEvent(w http.ResponseWriter, data []byte) error {
	controller := http.NewResponseController(w)
	// A deadline per flush bounds slow readers without a server-wide timeout
	// cutting off a healthy long-running SSE request.
	if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return controller.Flush()
}
