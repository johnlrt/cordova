package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"cordova/server/internal/inference"
)

func (s *Server) chat(w http.ResponseWriter, r *http.Request, requestID string, principal Principal) {
	started := time.Now()
	req, code, message := decodeRequest(w, r)
	if code != "" {
		fail(w, 400, requestID, code, message)
		return
	}
	if req.Model != ModelID {
		fail(w, 404, requestID, "model_not_found", "Unknown catalog model.")
		return
	}
	if len(principal.AllowedModels) > 0 && !principal.AllowedModels[req.Model] {
		fail(w, http.StatusForbidden, requestID, "forbidden", "This API key cannot use the requested model.")
		return
	}
	idempotencyKey, present, valid := requestIdempotencyKey(r)
	if !valid {
		fail(w, http.StatusBadRequest, requestID, "invalid_request", "Idempotency-Key must be a single value of at most 128 characters without control characters.")
		return
	}
	if present {
		if s.idempotency == nil {
			fail(w, http.StatusBadRequest, requestID, "unsupported_parameter", "Idempotency-Key storage is unavailable in this server mode.")
			return
		}
		canonical, err := json.Marshal(req)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, requestID, "storage_unavailable", "The request could not be recorded safely.")
			return
		}
		decision, err := s.idempotency.Claim(r.Context(), principal, idempotencyKey, req.Model, requestID, canonical)
		if err != nil {
			if errors.Is(err, ErrIdempotencyUnauthorized) {
				fail(w, http.StatusUnauthorized, requestID, "unauthenticated", "A valid API key is required.")
				return
			}
			fail(w, http.StatusServiceUnavailable, requestID, "storage_unavailable", "The request could not be recorded safely.")
			return
		}
		if !decision.Created {
			code := "conflict"
			if decision.Compared && !decision.BodyMatch {
				writeIdempotencyConflict(w, requestID, decision.RequestID, code)
				return
			}
			if decision.Status == "settled" || decision.Status == "cancelled" || decision.Status == "refunded" || decision.Status == "failed" {
				code = "replay_unavailable"
			}
			writeIdempotencyConflict(w, requestID, decision.RequestID, code)
			return
		}
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
	if s.executor != nil {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		s.accountedChat(w, r, requestID, principal, req.Model, input, req.Stream, includeUsage, started, idempotencyKey)
		return
	}
	completionID := "chatcmpl-" + requestID
	created := time.Now().Unix()
	if !req.Stream {
		var output strings.Builder
		result, err := s.provider.Generate(ctx, input, func(text string) error {
			if len(text) > (1<<20)-output.Len() {
				return errors.New("response limit")
			}
			output.WriteString(text)
			return ctx.Err()
		})
		if err != nil {
			fail(w, 503, requestID, "node_failed", "The test executor could not complete the request.")
			return
		}
		if !validResult(result, limit) {
			fail(w, 503, requestID, "node_failed", "The test executor returned invalid completion metadata.")
			return
		}
		if s.usageSink != nil {
			if err := s.usageSink(r.Context(), principal.AccountID, principal.KeyID, requestID, ModelID, result.Usage, time.Since(started)); err != nil {
				fail(w, 503, requestID, "storage_unavailable", "Request metadata could not be persisted.")
				return
			}
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
	type generationEvent struct {
		text   string
		result *inference.Result
		err    error
	}
	generated := make(chan generationEvent, 1)
	go func() {
		result, err := s.provider.Generate(ctx, input, func(text string) error {
			select {
			case generated <- generationEvent{text: text}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		select {
		case generated <- generationEvent{result: &result, err: err}:
		case <-ctx.Done():
		}
	}()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case frame := <-generated:
			if frame.result == nil {
				if ctx.Err() != nil {
					continue
				}
				if frame.text == "" || len(frame.text) > 16<<10 || bytesSent > (1<<20)-len(frame.text) || !utf8.ValidString(frame.text) {
					cancel()
					_ = event(w, errorBody(requestID, "node_failed", "The test executor returned an invalid response chunk.", 503))
					return
				}
				bytesSent += len(frame.text)
				base.Choices = []streamChoice{{Index: 0, Delta: delta{Content: frame.text}}}
				if err := event(w, base); err != nil {
					cancel()
					return
				}
				continue
			}
			if frame.err != nil {
				if r.Context().Err() == nil {
					_ = event(w, errorBody(requestID, "node_failed", "The test executor stopped before completion.", 503))
				}
				return
			}
			if !validResult(*frame.result, limit) {
				_ = event(w, errorBody(requestID, "node_failed", "The test executor returned invalid completion metadata.", 503))
				return
			}
			if s.usageSink != nil {
				if err := s.usageSink(r.Context(), principal.AccountID, principal.KeyID, requestID, ModelID, frame.result.Usage, time.Since(started)); err != nil {
					_ = event(w, errorBody(requestID, "storage_unavailable", "Request metadata could not be persisted.", 503))
					return
				}
			}
			base.Choices = []streamChoice{{Index: 0, Delta: delta{}, FinishReason: &frame.result.FinishReason}}
			if err := event(w, base); err != nil {
				return
			}
			if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
				base.Choices = []streamChoice{}
				base.Usage = &frame.result.Usage
				if err := event(w, base); err != nil {
					return
				}
			}
			_ = writeEvent(w, []byte("[DONE]"))
			return
		case <-heartbeat.C:
			if err := heartbeatEvent(w); err != nil {
				cancel()
				return
			}
		case <-ctx.Done():
			if r.Context().Err() == nil {
				_ = event(w, errorBody(requestID, "node_failed", "The test executor stopped before completion.", 503))
			}
			return
		}
	}
}

func requestIdempotencyKey(r *http.Request) (value string, present, valid bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", false, true
	}
	if len(values) != 1 {
		return "", true, false
	}
	value = values[0]
	if len(value) == 0 || utf8.RuneCountInString(value) > 128 || !utf8.ValidString(value) {
		return "", true, false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return "", true, false
		}
	}
	return value, true, true
}

func writeIdempotencyConflict(w http.ResponseWriter, requestID, originalRequestID, code string) {
	message := "A request with this idempotency key is already in progress."
	if code == "replay_unavailable" {
		message = "A request with this idempotency key already finished; its response is not retained."
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error:     apiError{Message: message, Type: "invalid_request_error", Code: code},
		RequestID: requestID, OriginalRequestID: originalRequestID,
	})
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

func heartbeatEvent(w http.ResponseWriter) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
		return err
	}
	return controller.Flush()
}

func validResult(result inference.Result, maxTokens int) bool {
	usage := result.Usage
	if result.FinishReason != "stop" && result.FinishReason != "length" {
		return false
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.CompletionTokens > maxTokens || usage.PromptTokens > int(^uint(0)>>1)-usage.CompletionTokens {
		return false
	}
	return usage.TotalTokens == usage.PromptTokens+usage.CompletionTokens
}
