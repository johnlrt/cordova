package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"cordova/server/internal/inference"
)

var ErrIdempotencyUnauthorized = errors.New("idempotency principal is no longer authorized")

// CompletionRequest carries identity and validated request data to the
// accounting boundary. Accepted is called only after a node accepts work.
type CompletionRequest struct {
	ID        string
	Principal Principal
	ModelID   string
	Input     inference.Request
	Streaming bool
	Accepted  func() error
}

type CompletionResult struct {
	Text         string
	Usage        inference.Usage
	FinishReason string
	Status       string
	Charged      string
}

type CompletionFailure struct {
	StatusCode        int
	Code              string
	Message           string
	RetryAfterSeconds int
}

type CompletionExecutor interface {
	Execute(context.Context, CompletionRequest, func(string) error) (CompletionResult, *CompletionFailure)
}

type IdempotencyDecision struct {
	Created   bool
	RequestID string
	Status    string
	Compared  bool
	BodyMatch bool
}

// IdempotencyManager receives canonical bytes only to derive a keyed digest;
// implementations must never persist request bodies or completion text.
type IdempotencyManager interface {
	Claim(context.Context, Principal, string, string, string, []byte) (IdempotencyDecision, error)
	Complete(context.Context, Principal, string, string, string) error
}

// AccountedUsageSink persists a content-free projection after the terminal
// ledger operation. The ledger remains the financial source of truth.
type AccountedUsageSink func(context.Context, string, string, string, string, string, string, inference.Usage, time.Duration) error

type executionEvent struct {
	kind   string
	text   string
	ack    chan error
	result CompletionResult
	fail   *CompletionFailure
	err    error
}

func (s *Server) accountedChat(w http.ResponseWriter, r *http.Request, requestID string, principal Principal,
	modelID string, input inference.Request, stream, includeUsage bool, started time.Time, idempotencyKey string) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	if s.requests != nil && principal.Kind == "key" {
		if !s.requests.Register(principal.KeyID, requestID, cancel) {
			fail(w, http.StatusUnauthorized, requestID, "unauthenticated", "A valid API key is required.")
			return
		}
		defer s.requests.Unregister(principal.KeyID, requestID)
	}

	if !stream {
		result, failure := s.executor.Execute(ctx, CompletionRequest{ID: requestID, Principal: principal, ModelID: modelID, Input: input}, nil)
		if terminalUsageStatus(result.Status) && s.accountedUsageSink != nil {
			if err := s.persistAccountedUsage(ctx, principal, requestID, modelID, result, started); err != nil && failure == nil {
				failure = storageFailure()
			}
		}
		if err := s.completeIdempotency(ctx, principal, idempotencyKey, requestID, result.Status, failure); err != nil && failure == nil {
			failure = storageFailure()
		}
		if failure != nil {
			writeCompletionFailure(w, requestID, failure, false)
			return
		}
		if !validResult(result.UsageResult(), input.MaxTokens) || !utf8.ValidString(result.Text) || len(result.Text) > 1<<20 {
			fail(w, http.StatusServiceUnavailable, requestID, "node_failed", "The completion could not be verified.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chatcmpl-" + requestID, "object": "chat.completion", "created": time.Now().Unix(), "model": modelID,
			"choices": []any{map[string]any{"index": 0, "message": inference.Message{Role: "assistant", Content: result.Text}, "finish_reason": result.FinishReason}},
			"usage":   result.Usage,
		})
		return
	}

	events := make(chan executionEvent, 1)
	request := CompletionRequest{ID: requestID, Principal: principal, ModelID: modelID, Input: input, Streaming: true}
	request.Accepted = func() error { return s.sendExecutionEvent(ctx, events, executionEvent{kind: "accepted"}) }
	emit := func(text string) error {
		ack := make(chan error, 1)
		return s.sendExecutionEvent(ctx, events, executionEvent{kind: "content", text: text, ack: ack})
	}
	done := make(chan executionEvent, 1)
	go func() {
		result, failure := s.executor.Execute(ctx, request, emit)
		var persistenceErr error
		if terminalUsageStatus(result.Status) && s.accountedUsageSink != nil {
			persistenceErr = s.persistAccountedUsage(ctx, principal, requestID, modelID, result, started)
		}
		if idempotencyErr := s.completeIdempotency(ctx, principal, idempotencyKey, requestID, result.Status, failure); persistenceErr == nil {
			persistenceErr = idempotencyErr
		}
		select {
		case done <- executionEvent{kind: "done", result: result, fail: failure, err: persistenceErr}:
		case <-ctx.Done():
		}
	}()

	base := chunk{ID: "chatcmpl-" + requestID, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: modelID}
	streamStarted := false
	startStream := func() error {
		if streamStarted {
			return nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		base.Choices = []streamChoice{{Index: 0, Delta: delta{Role: "assistant"}}}
		if err := event(w, base); err != nil {
			return err
		}
		streamStarted = true
		return nil
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case accepted := <-events:
			if accepted.kind == "accepted" {
				err := startStream()
				if accepted.ack != nil {
					accepted.ack <- err
				}
				if err != nil {
					cancel()
					return
				}
				continue
			}
			if accepted.kind != "content" {
				cancel()
				return
			}
			err := startStream()
			if err == nil {
				base.Choices = []streamChoice{{Index: 0, Delta: delta{Content: accepted.text}}}
				err = event(w, base)
			}
			accepted.ack <- err
			if err != nil {
				cancel()
				return
			}
		case completion := <-done:
			if completion.err != nil && completion.fail == nil {
				completion.fail = storageFailure()
			}
			if completion.fail != nil {
				if streamStarted {
					_ = event(w, errorBody(requestID, completion.fail.Code, completion.fail.Message, completion.fail.StatusCode))
				} else {
					writeCompletionFailure(w, requestID, completion.fail, false)
				}
				return
			}
			if !validResult(completion.result.UsageResult(), input.MaxTokens) || !utf8.ValidString(completion.result.Text) || len(completion.result.Text) > 1<<20 {
				if streamStarted {
					_ = event(w, errorBody(requestID, "node_failed", "The completion could not be verified.", http.StatusServiceUnavailable))
				} else {
					fail(w, http.StatusServiceUnavailable, requestID, "node_failed", "The completion could not be verified.")
				}
				return
			}
			if err := startStream(); err != nil {
				cancel()
				return
			}
			base.Choices = []streamChoice{{Index: 0, Delta: delta{}, FinishReason: &completion.result.FinishReason}}
			if err := event(w, base); err != nil {
				return
			}
			if includeUsage {
				base.Choices = []streamChoice{}
				base.Usage = &completion.result.Usage
				if err := event(w, base); err != nil {
					return
				}
			}
			_ = writeEvent(w, []byte("[DONE]"))
			return
		case <-heartbeat.C:
			if streamStarted {
				if err := heartbeatEvent(w); err != nil {
					cancel()
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) completeIdempotency(ctx context.Context, principal Principal, key, requestID, status string,
	failure *CompletionFailure) error {
	if key == "" || s.idempotency == nil {
		return nil
	}
	if status == "recovering" {
		return nil
	}
	if !terminalUsageStatus(status) {
		status = "failed"
		if failure != nil && failure.Code == "storage_unavailable" {
			return nil
		}
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.idempotency.Complete(finishCtx, principal, key, requestID, status)
}

func (s *Server) sendExecutionEvent(ctx context.Context, events chan<- executionEvent, current executionEvent) error {
	select {
	case events <- current:
	case <-ctx.Done():
		return ctx.Err()
	}
	if current.ack == nil {
		return nil
	}
	select {
	case err := <-current.ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) persistAccountedUsage(ctx context.Context, principal Principal, requestID, modelID string,
	result CompletionResult, started time.Time) error {
	if s.accountedUsageSink == nil {
		return nil
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.accountedUsageSink(persistCtx, principal.AccountID, principal.KeyID, requestID, modelID,
		result.Status, result.Charged, result.Usage, time.Since(started))
}

func terminalUsageStatus(status string) bool {
	return status == "settled" || status == "cancelled" || status == "refunded"
}

func writeCompletionFailure(w http.ResponseWriter, requestID string, failure *CompletionFailure, streaming bool) {
	if failure == nil {
		failure = storageFailure()
	}
	status := failure.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	if failure.Code == "" {
		failure.Code = "storage_unavailable"
	}
	if failure.Message == "" {
		failure.Message = "The request could not be completed."
	}
	if failure.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(failure.RetryAfterSeconds))
	}
	if streaming {
		_ = event(w, errorBody(requestID, failure.Code, failure.Message, status))
		return
	}
	fail(w, status, requestID, failure.Code, failure.Message)
}

func storageFailure() *CompletionFailure {
	return &CompletionFailure{StatusCode: http.StatusServiceUnavailable, Code: "storage_unavailable", Message: "Request metadata could not be persisted."}
}

func (r CompletionResult) UsageResult() inference.Result {
	return inference.Result{Usage: r.Usage, FinishReason: r.FinishReason}
}
